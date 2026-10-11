// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Round-trip, corruption, and fuzz tests for the binary record formats.

package record

import (
	"bytes"
	"errors"
	"math"
	"testing"
)

func TestEntryRoundTrip(t *testing.T) {
	cases := []Entry{
		{CreateRevision: 1, Version: 1, Value: []byte("v"), ValueSize: 1},
		{CreateRevision: 7, Version: 3, Lease: 30, PrevRevision: 6, Value: []byte{}, ValueSize: 0},
		{CreateRevision: 5, Version: 2, PrevRevision: 9, Deleted: true},
		{CreateRevision: math.MaxInt64, Version: math.MaxInt64, Lease: -1, PrevRevision: math.MaxInt64, ValueSize: 300_000, Chunks: 4},
	}
	for _, want := range cases {
		got, err := DecodeEntry(EncodeEntry(want))
		if err != nil {
			t.Fatalf("DecodeEntry(%+v): %v", want, err)
		}
		if got.CreateRevision != want.CreateRevision || got.Version != want.Version || got.Lease != want.Lease ||
			got.PrevRevision != want.PrevRevision || got.Deleted != want.Deleted || got.ValueSize != want.ValueSize ||
			got.Chunks != want.Chunks || !bytes.Equal(got.Value, want.Value) {
			t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
		}
	}
}

func TestNewEntryChoosesStorage(t *testing.T) {
	small := bytes.Repeat([]byte("a"), MaxInlineValue)
	e, chunks := NewEntry(small)
	if e.Chunks != 0 || chunks != nil || !bytes.Equal(e.Value, small) || e.ValueSize != len(small) {
		t.Fatalf("value of %d bytes should be inline, got chunks=%d", len(small), e.Chunks)
	}

	large := bytes.Repeat([]byte("abc"), 100_001)
	e, chunks = NewEntry(large)
	if e.Value != nil || e.Chunks != len(chunks) || e.ValueSize != len(large) {
		t.Fatalf("large value header mismatch: %+v (%d chunks)", e, len(chunks))
	}
	if len(chunks) != 4 {
		t.Fatalf("got %d chunks, want 4", len(chunks))
	}
	if got := bytes.Join(chunks, nil); !bytes.Equal(got, large) {
		t.Fatal("chunks do not reassemble to the original value")
	}
	for i, c := range chunks {
		if len(c) > ChunkSize {
			t.Fatalf("chunk %d is %d bytes", i, len(c))
		}
	}
	encoded := EncodeEntry(e)
	if len(encoded) > 64 {
		t.Fatalf("chunked entry header is %d bytes, expected a small header", len(encoded))
	}
}

func TestInlineEntryFitsFoundationDBValueLimit(t *testing.T) {
	e, _ := NewEntry(bytes.Repeat([]byte("x"), MaxInlineValue))
	e.CreateRevision, e.Version, e.PrevRevision, e.Lease = math.MaxInt64, math.MaxInt64, math.MaxInt64, math.MinInt64
	if n := len(EncodeEntry(e)); n > 100_000 {
		t.Fatalf("largest inline entry encodes to %d bytes, above the FoundationDB value limit", n)
	}
}

func TestIndexRoundTrip(t *testing.T) {
	for _, want := range []Index{
		{ModRevision: 2, CreateRevision: 1, Version: 2, ValueSize: 17},
		{ModRevision: 9, CreateRevision: 3, Version: 4, Lease: 60, Deleted: true},
		{ModRevision: 11, CreateRevision: 10, Version: 2, ValueSize: 1_500_000},
	} {
		got, err := DecodeIndex(EncodeIndex(want))
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("got %+v want %+v", got, want)
		}
	}
}

func TestLogRoundTrip(t *testing.T) {
	for _, want := range []LogEntry{
		{Key: []byte("/registry/pods/default/a"), Create: true},
		{Key: []byte("/k"), PrevRevision: 41, Deleted: true, PrevChunked: true},
		{Key: []byte{0, 0xff, '/'}, PrevRevision: 1},
	} {
		got, err := DecodeLog(EncodeLog(want))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Key, want.Key) || got.PrevRevision != want.PrevRevision ||
			got.Create != want.Create || got.Deleted != want.Deleted || got.PrevChunked != want.PrevChunked {
			t.Fatalf("got %+v want %+v", got, want)
		}
	}
}

func TestInt64(t *testing.T) {
	for _, v := range []int64{0, 1, 1 << 40, math.MaxInt64} {
		got, err := DecodeInt64(EncodeInt64(v))
		if err != nil || got != v {
			t.Fatalf("DecodeInt64(EncodeInt64(%d)) = %d, %v", v, got, err)
		}
	}
	if v, err := DecodeInt64(nil); err != nil || v != 0 {
		t.Fatalf("DecodeInt64(nil) = %d, %v", v, err)
	}
	if _, err := DecodeInt64([]byte{1, 2, 3}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("expected ErrCorrupt for short integer, got %v", err)
	}
}

func TestDecodeRejectsCorruptValues(t *testing.T) {
	valid := EncodeEntry(Entry{CreateRevision: 1, Version: 1, Value: []byte("hello"), ValueSize: 5})
	bad := map[string][]byte{
		"empty":            nil,
		"short":            {FormatVersion},
		"future version":   append([]byte{FormatVersion + 1}, valid[1:]...),
		"unknown flag":     append([]byte{FormatVersion, 0x80}, valid[2:]...),
		"truncated value":  valid[:len(valid)-1],
		"extra value byte": append(append([]byte{}, valid...), '!'),
	}
	for name, b := range bad {
		if _, err := DecodeEntry(b); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: expected ErrCorrupt, got %v", name, err)
		}
	}

	idx := EncodeIndex(Index{ModRevision: 1, CreateRevision: 1, Version: 1})
	if _, err := DecodeIndex(append(idx, 0)); !errors.Is(err, ErrCorrupt) {
		t.Errorf("index with trailing byte: expected ErrCorrupt, got %v", err)
	}
	if _, err := DecodeLog([]byte{FormatVersion, 0x40, 0}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("log with unknown flag: expected ErrCorrupt, got %v", err)
	}
}

func FuzzDecodeEntry(f *testing.F) {
	f.Add(EncodeEntry(Entry{CreateRevision: 3, Version: 1, Value: []byte("seed"), ValueSize: 4}))
	f.Add(EncodeEntry(Entry{Deleted: true, PrevRevision: 4}))
	f.Add(EncodeEntry(Entry{ValueSize: 200_000, Chunks: 3}))
	f.Fuzz(func(t *testing.T, b []byte) {
		e, err := DecodeEntry(b)
		if err != nil {
			return
		}
		again, err := DecodeEntry(EncodeEntry(e))
		if err != nil {
			t.Fatalf("re-decoding a valid entry failed: %v", err)
		}
		if again.ValueSize != e.ValueSize || again.Chunks != e.Chunks || !bytes.Equal(again.Value, e.Value) {
			t.Fatalf("unstable round trip: %+v vs %+v", e, again)
		}
	})
}

func FuzzDecodeIndex(f *testing.F) {
	f.Add(EncodeIndex(Index{ModRevision: 5, CreateRevision: 2, Version: 3, Lease: 10, ValueSize: 42}))
	f.Fuzz(func(t *testing.T, b []byte) {
		i, err := DecodeIndex(b)
		if err != nil {
			return
		}
		again, err := DecodeIndex(EncodeIndex(i))
		if err != nil || again != i {
			t.Fatalf("unstable round trip: %+v vs %+v (%v)", i, again, err)
		}
	})
}

func FuzzDecodeLog(f *testing.F) {
	f.Add(EncodeLog(LogEntry{Key: []byte("/a"), PrevRevision: 2, Deleted: true}))
	f.Fuzz(func(t *testing.T, b []byte) {
		l, err := DecodeLog(b)
		if err != nil {
			return
		}
		again, err := DecodeLog(EncodeLog(l))
		if err != nil || !bytes.Equal(again.Key, l.Key) || again.PrevRevision != l.PrevRevision {
			t.Fatalf("unstable round trip: %+v vs %+v (%v)", l, again, err)
		}
	})
}
