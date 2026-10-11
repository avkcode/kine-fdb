// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Binary encodings for the values kine-fdb stores in FoundationDB: per-key
// history entries, the latest-state index, the revision change log, and the
// little-endian integers kept in namespace metadata.

// Package record implements the versioned binary value formats used by the
// kine-fdb storage layout. It has no FoundationDB dependency so it can be
// tested and fuzzed on any platform.
package record

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// FormatVersion is the first byte of every encoded record. Bump it only
// together with a documented migration path.
const FormatVersion byte = 1

const (
	// MaxInlineValue is the largest value stored inside a history entry.
	// FoundationDB rejects values above 100,000 bytes, so anything larger is
	// split into chunks of ChunkSize bytes stored under separate keys.
	MaxInlineValue = 90_000
	// ChunkSize is the size of each chunk of a large value.
	ChunkSize = 90_000
)

const (
	flagDeleted     byte = 1 << 0
	flagChunked     byte = 1 << 1
	flagCreate      byte = 1 << 2
	flagPrevChunked byte = 1 << 3
)

// ErrCorrupt reports a value that cannot be decoded.
var ErrCorrupt = errors.New("record: corrupt value")

// Entry is one version of a key, stored under (key, modRevision).
type Entry struct {
	CreateRevision int64
	Version        int64
	Lease          int64
	PrevRevision   int64
	Deleted        bool
	// ValueSize is the total size of the value in bytes.
	ValueSize int
	// Chunks is the number of separately stored value chunks. Zero means the
	// value is stored inline in Value.
	Chunks int
	// Value holds the inline value. It is nil when Chunks > 0.
	Value []byte
}

// Index is the latest state of a key. It carries everything needed to answer
// keys-only and count requests without touching the value.
type Index struct {
	ModRevision    int64
	CreateRevision int64
	Version        int64
	Lease          int64
	Deleted        bool
	// ValueSize is the size of the current value, used to size list pages
	// before any value is read.
	ValueSize int
}

// LogEntry is the change-log record stored under each revision. Watchers use
// it to find the key that changed; compaction uses PrevRevision and the flags
// to find superseded history without reading values.
type LogEntry struct {
	Key          []byte
	PrevRevision int64
	Create       bool
	Deleted      bool
	// PrevChunked reports whether the entry at PrevRevision stored its value
	// in chunks, so compaction knows whether a chunk range must be cleared.
	PrevChunked bool
}

// NewEntry builds an Entry for value, choosing inline or chunked storage.
// The returned chunks must be written alongside the entry when non-nil.
func NewEntry(value []byte) (Entry, [][]byte) {
	e := Entry{ValueSize: len(value)}
	if len(value) <= MaxInlineValue {
		e.Value = value
		return e, nil
	}
	chunks := make([][]byte, 0, (len(value)+ChunkSize-1)/ChunkSize)
	for start := 0; start < len(value); start += ChunkSize {
		end := min(start+ChunkSize, len(value))
		chunks = append(chunks, value[start:end])
	}
	e.Chunks = len(chunks)
	return e, chunks
}

// EncodeEntry serializes e.
func EncodeEntry(e Entry) []byte {
	var flags byte
	if e.Deleted {
		flags |= flagDeleted
	}
	if e.Chunks > 0 {
		flags |= flagChunked
	}
	buf := make([]byte, 0, 2+5*binary.MaxVarintLen64+len(e.Value))
	buf = append(buf, FormatVersion, flags)
	buf = binary.AppendUvarint(buf, uint64(e.CreateRevision))
	buf = binary.AppendUvarint(buf, uint64(e.Version))
	buf = binary.AppendVarint(buf, e.Lease)
	buf = binary.AppendUvarint(buf, uint64(e.PrevRevision))
	buf = binary.AppendUvarint(buf, uint64(e.ValueSize))
	if e.Chunks > 0 {
		return binary.AppendUvarint(buf, uint64(e.Chunks))
	}
	return append(buf, e.Value...)
}

// DecodeEntry parses a value written by EncodeEntry. The returned Value
// aliases b.
func DecodeEntry(b []byte) (Entry, error) {
	r, flags, err := newReader(b)
	if err != nil {
		return Entry{}, err
	}
	if flags&^(flagDeleted|flagChunked) != 0 {
		return Entry{}, fmt.Errorf("%w: unknown entry flags %#x", ErrCorrupt, flags)
	}
	var e Entry
	e.Deleted = flags&flagDeleted != 0
	e.CreateRevision = r.revision()
	e.Version = r.revision()
	e.Lease = r.varint()
	e.PrevRevision = r.revision()
	e.ValueSize = r.size()
	if flags&flagChunked != 0 {
		e.Chunks = r.size()
		if r.err == nil && (e.Chunks == 0 || len(r.b) != 0) {
			r.err = fmt.Errorf("%w: invalid chunk header", ErrCorrupt)
		}
	} else if r.err == nil {
		if len(r.b) != e.ValueSize {
			r.err = fmt.Errorf("%w: inline value is %d bytes, header says %d", ErrCorrupt, len(r.b), e.ValueSize)
		}
		e.Value = r.b
	}
	if r.err != nil {
		return Entry{}, r.err
	}
	return e, nil
}

// EncodeIndex serializes i.
func EncodeIndex(i Index) []byte {
	var flags byte
	if i.Deleted {
		flags |= flagDeleted
	}
	buf := make([]byte, 0, 2+5*binary.MaxVarintLen64)
	buf = append(buf, FormatVersion, flags)
	buf = binary.AppendUvarint(buf, uint64(i.ModRevision))
	buf = binary.AppendUvarint(buf, uint64(i.CreateRevision))
	buf = binary.AppendUvarint(buf, uint64(i.Version))
	buf = binary.AppendVarint(buf, i.Lease)
	return binary.AppendUvarint(buf, uint64(i.ValueSize))
}

// DecodeIndex parses a value written by EncodeIndex.
func DecodeIndex(b []byte) (Index, error) {
	r, flags, err := newReader(b)
	if err != nil {
		return Index{}, err
	}
	if flags&^flagDeleted != 0 {
		return Index{}, fmt.Errorf("%w: unknown index flags %#x", ErrCorrupt, flags)
	}
	i := Index{Deleted: flags&flagDeleted != 0}
	i.ModRevision = r.revision()
	i.CreateRevision = r.revision()
	i.Version = r.revision()
	i.Lease = r.varint()
	i.ValueSize = r.size()
	if r.err == nil && len(r.b) != 0 {
		r.err = fmt.Errorf("%w: trailing bytes in index", ErrCorrupt)
	}
	if r.err != nil {
		return Index{}, r.err
	}
	return i, nil
}

// EncodeLog serializes l.
func EncodeLog(l LogEntry) []byte {
	var flags byte
	if l.Create {
		flags |= flagCreate
	}
	if l.Deleted {
		flags |= flagDeleted
	}
	if l.PrevChunked {
		flags |= flagPrevChunked
	}
	buf := make([]byte, 0, 2+binary.MaxVarintLen64+len(l.Key))
	buf = append(buf, FormatVersion, flags)
	buf = binary.AppendUvarint(buf, uint64(l.PrevRevision))
	return append(buf, l.Key...)
}

// DecodeLog parses a value written by EncodeLog. The returned Key aliases b.
func DecodeLog(b []byte) (LogEntry, error) {
	r, flags, err := newReader(b)
	if err != nil {
		return LogEntry{}, err
	}
	if flags&^(flagCreate|flagDeleted|flagPrevChunked) != 0 {
		return LogEntry{}, fmt.Errorf("%w: unknown log flags %#x", ErrCorrupt, flags)
	}
	l := LogEntry{
		Create:      flags&flagCreate != 0,
		Deleted:     flags&flagDeleted != 0,
		PrevChunked: flags&flagPrevChunked != 0,
	}
	l.PrevRevision = r.revision()
	if r.err != nil {
		return LogEntry{}, r.err
	}
	l.Key = r.b
	return l, nil
}

// EncodeInt64 encodes v as 8 little-endian bytes, the layout FoundationDB
// atomic operations use for integers.
func EncodeInt64(v int64) []byte {
	return binary.LittleEndian.AppendUint64(nil, uint64(v))
}

// DecodeInt64 parses a value written by EncodeInt64. A nil or empty value
// decodes to zero.
func DecodeInt64(b []byte) (int64, error) {
	switch len(b) {
	case 0:
		return 0, nil
	case 8:
		return int64(binary.LittleEndian.Uint64(b)), nil
	default:
		return 0, fmt.Errorf("%w: integer is %d bytes, want 8", ErrCorrupt, len(b))
	}
}

type reader struct {
	b   []byte
	err error
}

func newReader(b []byte) (*reader, byte, error) {
	if len(b) < 2 {
		return nil, 0, fmt.Errorf("%w: value too short", ErrCorrupt)
	}
	if b[0] != FormatVersion {
		return nil, 0, fmt.Errorf("%w: unsupported format version %d", ErrCorrupt, b[0])
	}
	return &reader{b: b[2:]}, b[1], nil
}

func (r *reader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.err = fmt.Errorf("%w: truncated varint", ErrCorrupt)
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *reader) varint() int64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Varint(r.b)
	if n <= 0 {
		r.err = fmt.Errorf("%w: truncated varint", ErrCorrupt)
		return 0
	}
	r.b = r.b[n:]
	return v
}

// revision reads a non-negative int64.
func (r *reader) revision() int64 {
	v := r.uvarint()
	if v > 1<<63-1 {
		if r.err == nil {
			r.err = fmt.Errorf("%w: revision out of range", ErrCorrupt)
		}
		return 0
	}
	return int64(v)
}

// size reads a length that must fit comfortably in an int.
func (r *reader) size() int {
	v := r.uvarint()
	if v > 1<<31-1 {
		if r.err == nil {
			r.err = fmt.Errorf("%w: size out of range", ErrCorrupt)
		}
		return 0
	}
	return int(v)
}
