// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Integration tests for List, ListStream, and Count: range semantics,
// sibling-prefix isolation, limits, historical reads, and multi-page reads.

package fdbdriver

import (
	"fmt"
	"slices"
	"testing"

	"github.com/k3s-io/kine/pkg/server"
)

// The lab driver trimmed the trailing slash from the start key, so listing
// namespace "kube" also returned objects from "kube-system".
func TestListDoesNotLeakSiblingPrefixes(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx := testCtx(t)
	for _, k := range []string{
		"/registry/pods/kube/a",
		"/registry/pods/kube-system/coredns",
		"/registry/pods/kube.public/x",
		"/registry/pods/kube",
		"/registry/pods/kubez/y",
	} {
		mustCreate(t, b, k, "v")
	}
	// kine passes etcd's range: key = prefix, end = prefix with its last
	// byte incremented.
	_, kvs, err := b.List(ctx, "/registry/pods/kube/", "/registry/pods/kube0", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(kvs); !slices.Equal(got, []string{"/registry/pods/kube/a"}) {
		t.Fatalf("List of namespace kube returned %v", got)
	}
	_, n, err := b.Count(ctx, "/registry/pods/kube/", "/registry/pods/kube0", 0)
	if err != nil || n != 1 {
		t.Fatalf("Count of namespace kube = %d, %v; want 1", n, err)
	}
}

func TestListRangeForms(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx := testCtx(t)
	for _, k := range []string{"/a", "/a/1", "/a/2", "/b", "compact_rev_key_apiserver"} {
		mustCreate(t, b, k, "v-"+k)
	}

	// key == "" lists every key, including ones outside "/".
	_, kvs, err := b.List(ctx, "", "", 0, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/a", "/a/1", "/a/2", "/b", server.HealthKey, "compact_rev_key_apiserver"}
	slices.Sort(want)
	if got := keysOf(kvs); !slices.Equal(got, want) {
		t.Fatalf("all keys = %v, want %v", got, want)
	}
	for _, kv := range kvs {
		if kv.Value != nil {
			t.Fatalf("keys-only list returned a value for %s", kv.Key)
		}
	}

	// end == "" is an exact key.
	_, kvs, _ = b.List(ctx, "/a", "", 0, 0, false)
	if got := keysOf(kvs); !slices.Equal(got, []string{"/a"}) || string(kvs[0].Value) != "v-/a" {
		t.Fatalf("exact key list = %v", got)
	}
	_, kvs, _ = b.List(ctx, "/nope", "", 0, 0, false)
	if len(kvs) != 0 {
		t.Fatalf("missing exact key returned %v", keysOf(kvs))
	}

	// [key, end), with the start key inclusive.
	_, kvs, _ = b.List(ctx, "/a/1", "/a0", 0, 0, false)
	if got := keysOf(kvs); !slices.Equal(got, []string{"/a/1", "/a/2"}) {
		t.Fatalf("range list = %v", got)
	}
	// Empty and inverted ranges.
	if _, kvs, err := b.List(ctx, "/z", "/a", 0, 0, false); err != nil || len(kvs) != 0 {
		t.Fatalf("inverted range = %v, %v", keysOf(kvs), err)
	}
}

func TestListLimitSkipsDeletedKeys(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx := testCtx(t)
	var revs []int64
	for i := range 40 {
		revs = append(revs, mustCreate(t, b, fmt.Sprintf("/l/%02d", i), "v"))
	}
	// Delete the first 30 so the limit has to look past index tombstones.
	for i := range 30 {
		mustDelete(t, b, fmt.Sprintf("/l/%02d", i), revs[i])
	}
	_, kvs, err := b.List(ctx, "/l/", "/l0", 5, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(kvs); !slices.Equal(got, []string{"/l/30", "/l/31", "/l/32", "/l/33", "/l/34"}) {
		t.Fatalf("limited list = %v", got)
	}
	_, n, _ := b.Count(ctx, "/l/", "/l0", 0)
	if n != 10 {
		t.Fatalf("Count = %d, want 10", n)
	}
	// Counting from a start key counts the remaining keys, as kine expects
	// for paginated lists.
	_, n, _ = b.Count(ctx, "/l/35", "/l0", 0)
	if n != 5 {
		t.Fatalf("Count from /l/35 = %d, want 5", n)
	}
}

func TestListAtRevision(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx := testCtx(t)
	ra := mustCreate(t, b, "/h/a", "a1")
	rb := mustCreate(t, b, "/h/b", "b1")
	snapshot := rb
	mustUpdate(t, b, "/h/a", "a2", ra)
	mustDelete(t, b, "/h/b", rb)
	mustCreate(t, b, "/h/c", "c1")

	cur, kvs, err := b.List(ctx, "/h/", "/h0", 0, snapshot, false)
	if err != nil {
		t.Fatal(err)
	}
	if cur <= snapshot {
		t.Fatalf("header revision %d should be the current revision, not the requested one", cur)
	}
	got := map[string]string{}
	for _, kv := range kvs {
		got[kv.Key] = string(kv.Value)
	}
	if len(got) != 2 || got["/h/a"] != "a1" || got["/h/b"] != "b1" {
		t.Fatalf("list at %d = %v", snapshot, got)
	}
	_, n, _ := b.Count(ctx, "/h/", "/h0", snapshot)
	if n != 2 {
		t.Fatalf("count at %d = %d", snapshot, n)
	}
	_, kvs, _ = b.List(ctx, "/h/", "/h0", 0, 0, false)
	if got := keysOf(kvs); !slices.Equal(got, []string{"/h/a", "/h/c"}) {
		t.Fatalf("current list = %v", got)
	}
}

// Ranges larger than one page must come back complete, in order, and
// consistent with a single revision.
func TestListAcrossPages(t *testing.T) {
	if testing.Short() {
		t.Skip("writes several thousand keys")
	}
	b := newTestBackend(t, testOptions{})
	ctx := testCtx(t)
	const n = 2500
	value := make([]byte, 6000) // 2500 * 6000 B exceeds one 8 MiB page
	for i := range n {
		if _, err := b.Create(ctx, fmt.Sprintf("/p/%05d", i), value, 0); err != nil {
			t.Fatal(err)
		}
	}
	at, kvs, err := b.List(ctx, "/p/", "/p0", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(kvs) != n {
		t.Fatalf("listed %d keys, want %d", len(kvs), n)
	}
	for i, kv := range kvs {
		if kv.Key != fmt.Sprintf("/p/%05d", i) || len(kv.Value) != len(value) {
			t.Fatalf("item %d = %s (%d bytes)", i, kv.Key, len(kv.Value))
		}
	}
	// Changes after the pinned revision do not leak into a historical read.
	mustUpdate(t, b, "/p/00000", "changed", kvs[0].ModRevision)
	_, again, err := b.List(ctx, "/p/", "/p0", 0, at, false)
	if err != nil || len(again) != n || len(again[0].Value) != len(value) {
		t.Fatalf("historical multi-page list: %d items, %v", len(again), err)
	}
	if _, cnt, err := b.Count(ctx, "/p/", "/p0", 0); err != nil || cnt != n {
		t.Fatalf("Count = %d, %v", cnt, err)
	}
	_, limited, _ := b.List(ctx, "/p/", "/p0", 1201, 0, true)
	if len(limited) != 1201 || limited[1200].Key != "/p/01200" {
		t.Fatalf("limited keys-only list returned %d items", len(limited))
	}
}

func TestListStream(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx := testCtx(t)
	for i := range 30 {
		mustCreate(t, b, fmt.Sprintf("/s/%02d", i), "v")
	}
	res := b.ListStream(ctx, "/s/", "/s0", 21, 0, false)
	var got []string
	for kv := range res.KVc {
		got = append(got, kv.Key)
	}
	if err := <-res.Errorc; err != nil {
		t.Fatal(err)
	}
	if len(got) != 21 || got[0] != "/s/00" || got[20] != "/s/20" {
		t.Fatalf("stream = %v", got)
	}
	cur, _ := b.CurrentRevision(ctx)
	if res.CurrentRevision != cur {
		t.Fatalf("stream header revision %d, want %d", res.CurrentRevision, cur)
	}

	bad := b.ListStream(ctx, "/s/", "/s0", 0, cur+10, false)
	for range bad.KVc {
	}
	if err := <-bad.Errorc; err != server.ErrFutureRev {
		t.Fatalf("stream at future revision: %v", err)
	}
}
