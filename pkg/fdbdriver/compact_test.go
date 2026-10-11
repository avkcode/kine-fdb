// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Integration tests for compaction: unreachable history is deleted, reads
// keep their meaning, minimum retention is honored, and automatic
// compaction runs on its own.

package fdbdriver

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/k3s-io/kine/pkg/server"
)

// The lab driver only moved a marker; nothing was ever deleted.
func TestCompactionDeletesHistory(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx := testCtx(t)
	big := bytes.Repeat([]byte("B"), 250_000)

	revs := map[string]int64{}
	for i := range 20 {
		key := fmt.Sprintf("/k/%02d", i)
		revs[key] = mustCreate(t, b, key, "v0")
	}
	for round := 1; round <= 5; round++ {
		for i := range 20 {
			key := fmt.Sprintf("/k/%02d", i)
			revs[key] = mustUpdate(t, b, key, fmt.Sprintf("v%d", round), revs[key])
		}
	}
	bigRev, _ := b.Create(ctx, "/big", big, 0)
	bigRev = mustUpdate(t, b, "/big", string(big[:200_000]), bigRev)
	for i := range 10 {
		key := fmt.Sprintf("/k/%02d", i)
		mustDelete(t, b, key, revs[key])
	}
	// The change-log entry at the compact revision is kept so watches can
	// start there; make it a plain create so the counts below are exact.
	target := mustCreate(t, b, "/sentinel", "s")
	beforeStats := statsOf(t, b)

	if _, err := b.Compact(ctx, target); err != nil {
		t.Fatal(err)
	}
	after := statsOf(t, b)
	// Every live key (10 /k keys, /big, /sentinel, and the health key) keeps
	// exactly one version; deleted keys vanish; the change log keeps only the
	// entry at the compact revision.
	if after.Versions != 13 || after.Keys != 13 || after.DeletedKeys != 0 || after.LogEntries != 1 {
		t.Fatalf("after compaction: %+v (before: %+v)", after, beforeStats)
	}
	if after.ValueChunks != 3 {
		t.Fatalf("expected only the current large value's 3 chunks, found %d", after.ValueChunks)
	}
	if after.CompactRevision != target || after.CompactedRevision != target {
		t.Fatalf("compact revision %d, cleanup point %d; want %d", after.CompactRevision, after.CompactedRevision, target)
	}
	mustVerify(t, b)

	// Current data is intact, the compact revision itself is readable, and
	// anything older is reported as compacted.
	_, kv, err := b.Get(ctx, "/k/15", 0, false)
	if err != nil || string(kv.Value) != "v5" {
		t.Fatalf("Get after compaction = %+v, %v", kv, err)
	}
	if _, kv, err := b.Get(ctx, "/big", 0, false); err != nil || len(kv.Value) != 200_000 || kv.ModRevision != bigRev {
		t.Fatalf("large value after compaction: %v", err)
	}
	if _, kvs, err := b.List(ctx, "/k/", "/k0", 0, target, false); err != nil || len(kvs) != 10 {
		t.Fatalf("List at the compact revision = %d keys, %v", len(kvs), err)
	}
	if _, _, err := b.Get(ctx, "/k/15", target-1, false); !errors.Is(err, server.ErrCompacted) {
		t.Fatalf("Get below compact revision: %v", err)
	}
	if _, err := b.Compact(ctx, target); !errors.Is(err, server.ErrCompacted) {
		t.Fatalf("second compaction to the same revision: %v", err)
	}
	if _, err := b.Compact(ctx, target+100); !errors.Is(err, server.ErrFutureRev) {
		t.Fatalf("compaction to a future revision: %v", err)
	}

	// Recreating a key whose tombstone was compacted starts a clean history.
	r := mustCreate(t, b, "/k/03", "new")
	if _, kv, _ := b.Get(ctx, "/k/03", 0, false); kv.CreateRevision != r || kv.Version != 1 {
		t.Fatalf("recreated key: %+v", kv)
	}
	mustVerify(t, b)
}

func statsOf(t testing.TB, b *testBackend) Stats {
	t.Helper()
	a, err := OpenAdmin(b.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	st, err := a.Stats(testCtx(t), "")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// Reads pinned to a revision inside the retained window keep returning the
// same answer while compaction removes older history around them.
func TestCompactionPreservesRetainedRevisions(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx := testCtx(t)
	r := map[string]int64{}
	for i := range 5 {
		key := fmt.Sprintf("/r/%d", i)
		r[key] = mustCreate(t, b, key, "a")
	}
	mid, _ := b.CurrentRevision(ctx)
	for i := range 5 {
		key := fmt.Sprintf("/r/%d", i)
		r[key] = mustUpdate(t, b, key, "b", r[key])
	}
	_, before, _ := b.List(ctx, "/r/", "/r0", 0, mid, false)

	if _, err := b.Compact(ctx, mid); err != nil {
		t.Fatal(err)
	}
	_, after, err := b.List(ctx, "/r/", "/r0", 0, mid, false)
	if err != nil || len(after) != len(before) {
		t.Fatalf("list at %d after compaction: %d keys, %v", mid, len(after), err)
	}
	for i := range after {
		if string(after[i].Value) != "a" || after[i].ModRevision != before[i].ModRevision {
			t.Fatalf("value at %d changed by compaction: %+v vs %+v", mid, after[i], before[i])
		}
	}
	mustVerify(t, b)
}

func TestCompactMinRetain(t *testing.T) {
	b := newTestBackend(t, testOptions{minRetain: 10})
	ctx := testCtx(t)
	var rev int64
	for i := range 20 {
		rev = mustCreate(t, b, fmt.Sprintf("/m/%d", i), "v")
	}
	if _, err := b.Compact(ctx, rev); err != nil {
		t.Fatal(err)
	}
	if st := statsOf(t, b); st.CompactRevision != rev-10 {
		t.Fatalf("compact revision %d, want %d (current minus min-retain)", st.CompactRevision, rev-10)
	}
}

func TestAutomaticCompaction(t *testing.T) {
	b := newTestBackend(t, testOptions{compactInterval: 500 * time.Millisecond})
	ctx := testCtx(t)
	rev := mustCreate(t, b, "/auto", "0")
	for i := 1; i <= 50; i++ {
		rev = mustUpdate(t, b, "/auto", fmt.Sprint(i), rev)
	}
	// With automatic compaction enabled, client compaction requests are
	// accepted but the driver keeps its own schedule.
	if got, err := b.Compact(ctx, rev); err != nil || got != rev {
		t.Fatalf("Compact with automatic compaction = %d, %v", got, err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		st := statsOf(t, b)
		if st.CompactRevision >= rev && st.CompactedRevision == st.CompactRevision {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("automatic compaction did not catch up: %+v", st)
		}
		time.Sleep(250 * time.Millisecond)
	}
	mustVerify(t, b)
}

// Compaction runs concurrently with writers without making them fail, and
// a namespace verified afterwards is consistent.
func TestCompactionDuringWrites(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx := testCtx(t)
	done := make(chan error, 1)
	go func() {
		revs := map[int]int64{}
		for i := range 400 {
			key := fmt.Sprintf("/cw/%d", i%40)
			var err error
			if rev, ok := revs[i%40]; ok {
				var ok2 bool
				revs[i%40], _, ok2, err = b.Update(ctx, key, []byte(fmt.Sprint(i)), rev, 0)
				if err == nil && !ok2 {
					err = fmt.Errorf("update of %s at %d was rejected", key, rev)
				}
			} else {
				revs[i%40], err = b.Create(ctx, key, []byte("0"), 0)
			}
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			cur, _ := b.CurrentRevision(ctx)
			if _, err := b.Compact(ctx, cur); err != nil && !errors.Is(err, server.ErrCompacted) {
				t.Fatal(err)
			}
			mustVerify(t, b)
			return
		case <-time.After(20 * time.Millisecond):
			cur, _ := b.CurrentRevision(ctx)
			if _, err := b.Compact(ctx, cur-5); err != nil && !errors.Is(err, server.ErrCompacted) && !errors.Is(err, server.ErrFutureRev) {
				t.Fatal(err)
			}
		}
	}
}
