// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Integration tests for single-key reads and writes: revisions, versions,
// conditional updates and deletes, large values, and the health key.

package fdbdriver

import (
	"bytes"
	"errors"
	"testing"

	"github.com/k3s-io/kine/pkg/server"
)

func TestStartSeedsHealthKey(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx := testCtx(t)
	rev, kv, err := b.Get(ctx, server.HealthKey, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	// Create plus update, as with kine's other backends.
	if rev != 2 || kv == nil || kv.ModRevision != 2 || kv.CreateRevision != 1 || kv.Version != 2 {
		t.Fatalf("fresh namespace: rev=%d kv=%+v, want rev 2 and health key created at 1, modified at 2", rev, kv)
	}

	// Restarting on the same namespace must not write again.
	b.stop()
	b2 := newTestBackend(t, testOptions{namespace: b.namespace})
	if rev, err := b2.CurrentRevision(ctx); err != nil || rev != 2 {
		t.Fatalf("after restart CurrentRevision = %d, %v; want 2", rev, err)
	}
}

func TestCreateGetUpdateDelete(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx := testCtx(t)

	r1 := mustCreate(t, b, "/a", "v1")
	if r1 != 3 {
		t.Fatalf("first create got revision %d, want 3", r1)
	}
	if rev, err := b.Create(ctx, "/a", []byte("again"), 0); !errors.Is(err, server.ErrKeyExists) || rev != r1 {
		t.Fatalf("duplicate create = %d, %v; want %d, ErrKeyExists", rev, err, r1)
	}

	rev, kv, err := b.Get(ctx, "/a", 0, false)
	if err != nil || rev != r1 || kv == nil || string(kv.Value) != "v1" || kv.Version != 1 || kv.CreateRevision != r1 || kv.ModRevision != r1 {
		t.Fatalf("Get = %d, %+v, %v", rev, kv, err)
	}

	// Update with a stale revision fails and reports the current value.
	rev, kv, ok, err := b.Update(ctx, "/a", []byte("nope"), r1-1, 0)
	if err != nil || ok || rev != r1 || kv == nil || string(kv.Value) != "v1" {
		t.Fatalf("stale update = %d, %+v, %v, %v", rev, kv, ok, err)
	}
	r2 := mustUpdate(t, b, "/a", "v2", r1)
	_, kv, _ = b.Get(ctx, "/a", 0, false)
	if string(kv.Value) != "v2" || kv.Version != 2 || kv.CreateRevision != r1 || kv.ModRevision != r2 {
		t.Fatalf("after update: %+v", kv)
	}
	// The previous version stays readable at its revision.
	_, kv, _ = b.Get(ctx, "/a", r1, false)
	if kv == nil || string(kv.Value) != "v1" {
		t.Fatalf("Get at %d = %+v, want v1", r1, kv)
	}

	// Delete with a stale revision fails.
	if _, kv, ok, err := b.Delete(ctx, "/a", r1); err != nil || ok || kv == nil || kv.ModRevision != r2 {
		t.Fatalf("stale delete = %+v, %v, %v", kv, ok, err)
	}
	rev, kv, ok, err = b.Delete(ctx, "/a", r2)
	if err != nil || !ok || rev != r2+1 || kv == nil || string(kv.Value) != "v2" {
		t.Fatalf("delete = %d, %+v, %v, %v", rev, kv, ok, err)
	}
	if _, kv, _ := b.Get(ctx, "/a", 0, false); kv != nil {
		t.Fatalf("deleted key still readable: %+v", kv)
	}
	if _, kv, _ := b.Get(ctx, "/a", r2, false); kv == nil || string(kv.Value) != "v2" {
		t.Fatalf("deleted key not readable at %d: %+v", r2, kv)
	}

	// Updating or conditionally deleting a missing key fails; an
	// unconditional delete of a missing key succeeds, as in etcd.
	if _, _, ok, err := b.Update(ctx, "/a", []byte("x"), r2, 0); err != nil || ok {
		t.Fatalf("update of deleted key: ok=%v err=%v", ok, err)
	}
	if _, _, ok, err := b.Delete(ctx, "/missing", 5); err != nil || ok {
		t.Fatalf("conditional delete of missing key: ok=%v err=%v", ok, err)
	}
	if _, kv, ok, err := b.Delete(ctx, "/missing", 0); err != nil || !ok || kv != nil {
		t.Fatalf("unconditional delete of missing key: kv=%v ok=%v err=%v", kv, ok, err)
	}

	// Recreating starts a new lifetime with version 1.
	r4 := mustCreate(t, b, "/a", "v3")
	_, kv, _ = b.Get(ctx, "/a", 0, false)
	if kv.CreateRevision != r4 || kv.Version != 1 || string(kv.Value) != "v3" {
		t.Fatalf("recreated key: %+v", kv)
	}
	mustVerify(t, b)
}

func TestReadRevisionValidation(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx := testCtx(t)
	mustCreate(t, b, "/f", "v")
	cur, _ := b.CurrentRevision(ctx)
	if rev, _, err := b.Get(ctx, "/f", cur+1, false); !errors.Is(err, server.ErrFutureRev) || rev != cur {
		t.Fatalf("Get future revision = %d, %v", rev, err)
	}
	if _, _, err := b.List(ctx, "/", "0", 0, cur+5, false); !errors.Is(err, server.ErrFutureRev) {
		t.Fatalf("List future revision: %v", err)
	}
	if _, _, err := b.Count(ctx, "/", "0", cur+5); !errors.Is(err, server.ErrFutureRev) {
		t.Fatalf("Count future revision: %v", err)
	}
}

func TestKeysOnlyAndLeases(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx := testCtx(t)
	rev, err := b.Create(ctx, "/lease", []byte("value"), 3600)
	if err != nil {
		t.Fatal(err)
	}
	_, kv, err := b.Get(ctx, "/lease", 0, true)
	if err != nil || kv == nil || kv.Value != nil || kv.Lease != 3600 || kv.ModRevision != rev {
		t.Fatalf("keys-only Get = %+v, %v", kv, err)
	}
}

func TestLargeValues(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx := testCtx(t)
	big1 := bytes.Repeat([]byte("0123456789abcdef"), 20_000) // 320 KB, 4 chunks
	big2 := bytes.Repeat([]byte("z"), 1_400_000)             // 1.4 MB
	small := []byte("small again")

	r1, err := b.Create(ctx, "/big", big1, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, kv, err := b.Get(ctx, "/big", 0, false)
	if err != nil || !bytes.Equal(kv.Value, big1) {
		t.Fatalf("large value mismatch: %v", err)
	}
	r2, kv, ok, err := b.Update(ctx, "/big", big2, r1, 0)
	if err != nil || !ok || !bytes.Equal(kv.Value, big2) {
		t.Fatalf("update to larger value: ok=%v err=%v", ok, err)
	}
	r3 := mustUpdate(t, b, "/big", string(small), r2)
	_, kvs, err := b.List(ctx, "/big", "", 0, r2, false)
	if err != nil || len(kvs) != 1 || !bytes.Equal(kvs[0].Value, big2) {
		t.Fatalf("historical list of large value: %v", err)
	}
	_, kv, _, err = b.Delete(ctx, "/big", r3)
	if err != nil || !bytes.Equal(kv.Value, small) {
		t.Fatalf("delete returned %v, %v", kv, err)
	}
	mustVerify(t, b)
}

func TestKeyValidation(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx := testCtx(t)
	if _, err := b.Create(ctx, "", []byte("v"), 0); err == nil {
		t.Fatal("empty key accepted")
	}
	if _, err := b.Create(ctx, string(bytes.Repeat([]byte("k"), 20_000)), []byte("v"), 0); err == nil {
		t.Fatal("oversized key accepted")
	}
	// Binary keys, including NUL and 0xFF bytes, are stored verbatim.
	key := "/bin/\x00\xff\x01"
	mustCreate(t, b, key, "binary")
	_, kv, err := b.Get(ctx, key, 0, false)
	if err != nil || kv == nil || kv.Key != key {
		t.Fatalf("binary key round trip: %+v, %v", kv, err)
	}
}
