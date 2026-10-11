// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Integration tests for namespaces, administration, concurrency, lease
// expiry, request cancellation, and idempotent retries.

package fdbdriver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/k3s-io/kine/pkg/server"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The lab driver concatenated raw namespace prefixes, so "/a/" contained
// "/a/b/": resetting the parent wiped the child and sizes included it.
func TestNestedNamespacesAreIsolated(t *testing.T) {
	parentNS := testNamespace(t)
	childNS := parentNS + "child/"
	parent := newTestBackend(t, testOptions{namespace: parentNS})
	child := newTestBackend(t, testOptions{namespace: childNS})
	t.Cleanup(func() { deleteNamespace(t, child.endpoint) })
	t.Cleanup(func() { deleteNamespace(t, parent.endpoint) })
	ctx := testCtx(t)

	mustCreate(t, parent, "/only-parent", "p")
	mustCreate(t, child, "/only-child", "c")
	_, kvs, _ := parent.List(ctx, "", "", 0, 0, true)
	if slices.Contains(keysOf(kvs), "/only-child") {
		t.Fatalf("parent namespace sees child keys: %v", keysOf(kvs))
	}

	a, err := OpenAdmin(parent.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	names, err := a.Namespaces(ctx)
	if err != nil || !slices.Contains(names, parentNS) || !slices.Contains(names, childNS) {
		t.Fatalf("Namespaces = %v, %v", names, err)
	}

	parent.stop()
	if err := a.Delete(ctx, parentNS); err != nil {
		t.Fatal(err)
	}
	if _, kv, err := child.Get(ctx, "/only-child", 0, false); err != nil || kv == nil {
		t.Fatalf("deleting the parent namespace affected the child: %+v, %v", kv, err)
	}
	if err := a.Delete(ctx, parentNS); !errors.Is(err, ErrNamespaceNotFound) {
		t.Fatalf("deleting a missing namespace: %v", err)
	}
}

// A server whose namespace is deleted underneath it must stop writing
// instead of filling an orphaned key range.
func TestDeletedNamespaceStopsWrites(t *testing.T) {
	b := newTestBackend(t, testOptions{namespace: testNamespace(t)})
	mustCreate(t, b, "/x", "v")
	deleteNamespace(t, b.endpoint)
	_, err := b.Create(testCtx(t), "/y", []byte("v"), 0)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("write after namespace deletion: %v", err)
	}
}

func TestNamespaceRefusesForeignDirectory(t *testing.T) {
	cf := testClusterFile(t)
	ns := testNamespace(t)
	b := newTestBackend(t, testOptions{namespace: ns})
	b.stop()
	// Opening an existing namespace whose format is newer than this build
	// must fail instead of misreading it.
	_, err := b.run(testCtx(t), txnOptions{op: "test", write: true}, func(tr fdb.Transaction, _ *attempt) (any, error) {
		tr.Set(b.ks.metaFormat, []byte{9, 0, 0, 0, 0, 0, 0, 0})
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := ParseEndpoint("fdb://" + cf + "?namespace=" + ns)
	if _, err := openStore(testCtx(t), cfg, true); err == nil {
		t.Fatal("opened a namespace with an unknown storage format")
	}
	deleteNamespace(t, b.endpoint)
}

func TestConcurrentWritersProduceDenseRevisions(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx := testCtx(t)
	start, _ := b.CurrentRevision(ctx)

	const workers, perWorker = 16, 40
	var mu sync.Mutex
	seen := map[int64]string{}
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Workers fight over a shared key and also write their own keys.
			for i := range perWorker {
				key := fmt.Sprintf("/c/%d/%d", w, i)
				rev, err := b.Create(ctx, key, []byte("v"), 0)
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				if other, dup := seen[rev]; dup {
					mu.Unlock()
					errs <- fmt.Errorf("revision %d returned for %s and %s", rev, other, key)
					return
				}
				seen[rev] = key
				mu.Unlock()
				for {
					cur, kv, err := b.Get(ctx, "/c/shared", 0, false)
					if err != nil {
						errs <- err
						return
					}
					var ok bool
					if kv == nil {
						_, err = b.Create(ctx, "/c/shared", []byte(key), 0)
						ok = err == nil
						if errors.Is(err, server.ErrKeyExists) {
							err = nil
						}
					} else {
						var rev int64
						rev, _, ok, err = b.Update(ctx, "/c/shared", []byte(key), kv.ModRevision, 0)
						if ok {
							mu.Lock()
							seen[rev] = "/c/shared"
							mu.Unlock()
						}
					}
					_ = cur
					if err != nil {
						errs <- err
						return
					}
					if ok {
						break
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	end, _ := b.CurrentRevision(ctx)
	_, kv, _ := b.Get(ctx, "/c/shared", 0, false)
	if kv.Version != workers*perWorker {
		t.Fatalf("shared key version %d, want %d: lost updates", kv.Version, workers*perWorker)
	}
	// Creates plus updates (the first shared write is a create).
	if want := start + 2*workers*perWorker; end != want {
		t.Fatalf("current revision %d, want %d", end, want)
	}
	mustVerify(t, b)
}

func TestLeasedKeysExpire(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx := testCtx(t)
	if _, err := b.Create(ctx, "/lease/short", []byte("v"), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Create(ctx, "/lease/long", []byte("v"), 3600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, kv, err := b.Get(ctx, "/lease/short", 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if kv == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("key with a 1s lease was not deleted")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, kv, _ := b.Get(ctx, "/lease/long", 0, false); kv == nil {
		t.Fatal("key with a long lease was deleted")
	}
}

// Leases keep expiring after a compaction; kine's TTL watcher died there
// with the lab driver because every watch reported a compact revision.
func TestLeasesExpireAfterCompactionAndRestart(t *testing.T) {
	ns := testNamespace(t)
	b := newTestBackend(t, testOptions{namespace: ns})
	ctx := testCtx(t)
	rev := mustCreate(t, b, "/pad", "v")
	rev = mustUpdate(t, b, "/pad", "v2", rev)
	if _, err := b.Compact(ctx, rev); err != nil {
		t.Fatal(err)
	}
	b.stop()
	b2 := newTestBackend(t, testOptions{namespace: ns})
	t.Cleanup(func() { deleteNamespace(t, b2.endpoint) })
	if _, err := b2.Create(ctx, "/lease/after", []byte("v"), 1); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, kv, _ := b2.Get(ctx, "/lease/after", 0, false); kv == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("lease did not expire after compaction and restart")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestCancelledContext(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Create(ctx, "/cancel", []byte("v"), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create with a cancelled context: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if _, _, err := b.List(ctx, "", "", 0, 0, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("List past its deadline: %v", err)
	}
}

// retryAfterUnknownResult runs one write as the retry of an attempt that was
// assigned revision rev and whose commit outcome was unknown.
func retryAfterUnknownResult(t *testing.T, b *testBackend, key string, rev int64, decide decideFunc) (writeResult, error) {
	t.Helper()
	o := &writeOp{ctx: testCtx(t), key: key, decide: decide, candidates: []int64{rev}}
	_, err := b.run(testCtx(t), txnOptions{op: "test", write: true}, func(tr fdb.Transaction, at *attempt) (any, error) {
		return nil, b.writer.apply(tr, at, []*writeOp{o})
	})
	if err != nil {
		return writeResult{}, err
	}
	return o.outcome.res, o.outcome.err
}

// A write whose commit succeeded but was reported as unknown must, when
// retried, report its own success instead of a conflict, and must not be
// applied a second time.
func TestRetryAfterUnknownCommitResult(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx := testCtx(t)
	const key = "/idempotent"

	r1 := mustCreate(t, b, key, "v1")
	res, err := retryAfterUnknownResult(t, b, key, r1, b.decideCreate(key, []byte("v1"), 0))
	if err != nil || !res.ok || res.rev != r1 {
		t.Fatalf("create retry = %+v, %v; want success at %d", res, err, r1)
	}

	r2 := mustUpdate(t, b, key, "v2", r1)
	res, err = retryAfterUnknownResult(t, b, key, r2, b.decideUpdate(key, []byte("v2"), r1, 0))
	if err != nil || !res.ok || res.rev != r2 || string(res.kv.Value) != "v2" {
		t.Fatalf("update retry = %+v, %v; want success at %d", res, err, r2)
	}

	r3 := mustDelete(t, b, key, r2)
	res, err = retryAfterUnknownResult(t, b, key, r3, b.decideDelete(key, r2))
	if err != nil || !res.ok || res.rev != r3 || res.kv == nil || string(res.kv.Value) != "v2" {
		t.Fatalf("delete retry = %+v, %v; want success at %d returning v2", res, err, r3)
	}
	if cur, _ := b.CurrentRevision(ctx); cur != r3 {
		t.Fatalf("retries wrote revisions: current %d, want %d", cur, r3)
	}

	// A version at the candidate revision written by someone else (here: a
	// different value) is not mistaken for this write's own commit.
	r4 := mustCreate(t, b, key, "theirs")
	if _, err := retryAfterUnknownResult(t, b, key, r4, b.decideCreate(key, []byte("mine"), 0)); !errors.Is(err, server.ErrKeyExists) {
		t.Fatalf("create retry with a foreign candidate: %v, want ErrKeyExists", err)
	}
	mustVerify(t, b)
}

// Writes queued together are committed in one transaction, each at its own
// revision and in order, including several writes to the same key.
func TestBatchedWritesToTheSameKey(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx := testCtx(t)
	start, _ := b.CurrentRevision(ctx)
	ops := []*writeOp{
		{ctx: ctx, key: "/b/k", decide: b.decideCreate("/b/k", []byte("1"), 0)},
		{ctx: ctx, key: "/b/k", decide: b.decideUpdate("/b/k", []byte("2"), start+1, 0)},
		{ctx: ctx, key: "/b/k", decide: b.decideCreate("/b/k", []byte("dup"), 0)},
		{ctx: ctx, key: "/b/other", decide: b.decideCreate("/b/other", bytes.Repeat([]byte("x"), 200_000), 0)},
		{ctx: ctx, key: "/b/k", decide: b.decideDelete("/b/k", start+2)},
		{ctx: ctx, key: "/b/other", decide: b.decideUpdate("/b/other", []byte("small"), start+3, 0)},
	}
	for _, o := range ops {
		o.done = make(chan writeOutcome, 1)
	}
	b.writer.commit(ctx, ops)
	want := []struct {
		rev int64
		ok  bool
		err error
	}{
		{start + 1, true, nil},
		{start + 2, true, nil},
		{start + 2, false, server.ErrKeyExists},
		{start + 3, true, nil},
		{start + 4, true, nil},
		{start + 5, true, nil},
	}
	for i, o := range ops {
		out := <-o.done
		if out.res.rev != want[i].rev || out.res.ok != want[i].ok || !errors.Is(out.err, want[i].err) {
			t.Fatalf("op %d = rev %d ok %v err %v; want rev %d ok %v err %v", i, out.res.rev, out.res.ok, out.err, want[i].rev, want[i].ok, want[i].err)
		}
	}
	if _, kv, _ := b.Get(ctx, "/b/k", 0, false); kv != nil {
		t.Fatalf("/b/k should be deleted, got %+v", kv)
	}
	if _, kv, _ := b.Get(ctx, "/b/other", 0, false); kv == nil || string(kv.Value) != "small" || kv.Version != 2 {
		t.Fatalf("/b/other = %+v", kv)
	}
	mustVerify(t, b)
}
