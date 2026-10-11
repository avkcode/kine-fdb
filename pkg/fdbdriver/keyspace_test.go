// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Unit tests for the key layout: range mapping, ordering, and isolation of
// per-key history.

package fdbdriver

import (
	"bytes"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
)

func testKeyspace() *keyspace {
	return newKeyspace("/t/", subspace.FromBytes([]byte{0x15, 0x2a}))
}

func inRange(r fdb.KeyRange, k fdb.Key) bool {
	return bytes.Compare(k, r.Begin.FDBKey()) >= 0 && bytes.Compare(k, r.End.FDBKey()) < 0
}

func TestIndexRangeMatchesKineSemantics(t *testing.T) {
	ks := testKeyspace()
	cases := []struct {
		key, end string
		in, out  []string
	}{
		{"", "", []string{"", "/a", "compact_rev_key", "\xff\xff"}, nil},
		{"/a", "", []string{"/a"}, []string{"/a/", "/a\x00", "/b", "/"}},
		{"/registry/pods/kube/", "/registry/pods/kube0", []string{"/registry/pods/kube/x"},
			[]string{"/registry/pods/kube", "/registry/pods/kube-system/x", "/registry/pods/kube.x", "/registry/pods/kube0"}},
		{"/a/m", "/a0", []string{"/a/m", "/a/z", "/a/\xff"}, []string{"/a/a", "/a0", "/b"}},
	}
	for _, c := range cases {
		r, ok := ks.indexRange(c.key, c.end)
		if !ok {
			t.Fatalf("range (%q, %q) reported empty", c.key, c.end)
		}
		for _, k := range c.in {
			if !inRange(r, ks.indexKey(k)) {
				t.Errorf("range (%q, %q) should contain %q", c.key, c.end, k)
			}
		}
		for _, k := range c.out {
			if inRange(r, ks.indexKey(k)) {
				t.Errorf("range (%q, %q) should not contain %q", c.key, c.end, k)
			}
		}
	}
	if _, ok := ks.indexRange("/b", "/a"); ok {
		t.Fatal("inverted range not reported empty")
	}
	all, _ := ks.indexRange("", "")
	for _, other := range []fdb.Key{ks.metaRev, ks.historyKey("/a", 1), ks.logKey(1), ks.chunkKey("/a", 1, 0)} {
		if inRange(all, other) {
			t.Fatalf("index range overlaps another subspace at %q", other)
		}
	}
}

func TestHistoryRangesDoNotOverlapAcrossKeys(t *testing.T) {
	ks := testKeyspace()
	// "a" is a prefix of the other keys; tuple encoding must keep their
	// histories apart, including keys containing NUL.
	keys := []string{"a", "a\x00", "a\x00b", "ab", "a/"}
	for _, k := range keys {
		r := ks.historyAll(k)
		upTo := ks.historyUpTo(k, 10)
		for _, other := range keys {
			for _, rev := range []int64{1, 9, 10, 11, 1 << 40} {
				hk := ks.historyKey(other, rev)
				if got, want := inRange(r, hk), other == k; got != want {
					t.Errorf("historyAll(%q) contains %q@%d = %v", k, other, rev, got)
				}
				if got, want := inRange(upTo, hk), other == k && rev <= 10; got != want {
					t.Errorf("historyUpTo(%q, 10) contains %q@%d = %v", k, other, rev, got)
				}
			}
		}
		key, rev, err := ks.historyRevision(ks.historyKey(k, 77))
		if err != nil || key != k || rev != 77 {
			t.Errorf("historyRevision round trip for %q = %q, %d, %v", k, key, rev, err)
		}
	}
}

func TestLogKeysSortByRevision(t *testing.T) {
	ks := testKeyspace()
	revs := []int64{1, 2, 255, 256, 65535, 65536, 1 << 32, 1 << 50}
	for i := 1; i < len(revs); i++ {
		if bytes.Compare(ks.logKey(revs[i-1]), ks.logKey(revs[i])) >= 0 {
			t.Fatalf("log key for %d does not sort before %d", revs[i-1], revs[i])
		}
		got, err := ks.logRevision(ks.logKey(revs[i]))
		if err != nil || got != revs[i] {
			t.Fatalf("logRevision(%d) = %d, %v", revs[i], got, err)
		}
	}
	r := ks.logRange(5, 10)
	for rev := int64(1); rev < 15; rev++ {
		if got, want := inRange(r, ks.logKey(rev)), rev >= 5 && rev < 10; got != want {
			t.Errorf("logRange(5,10) contains %d = %v", rev, got)
		}
	}
}

func TestStrinc(t *testing.T) {
	for in, want := range map[string]string{
		"a":            "b",
		"a\xff":        "b",
		"\x01\xff\xff": "\x02",
	} {
		if got := string(strinc([]byte(in))); got != want {
			t.Errorf("strinc(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckKeySize(t *testing.T) {
	ks := testKeyspace()
	if err := ks.checkKeySize(""); err == nil {
		t.Fatal("empty key accepted")
	}
	if err := ks.checkKeySize(string(bytes.Repeat([]byte("k"), 9000))); err != nil {
		t.Fatalf("9000-byte key rejected: %v", err)
	}
	if err := ks.checkKeySize(string(bytes.Repeat([]byte{0}, 6000))); err == nil {
		t.Fatal("key whose escaped form exceeds the limit was accepted")
	}
}
