// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Key layout of a namespace: metadata, the latest-state index, per-key
// history, large-value chunks, and the revision change log, all inside one
// directory-layer subspace so namespaces never overlap.

package fdbdriver

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

const (
	// DirectoryRoot is the directory-layer path element under which every
	// namespace directory is created: ["kine", <namespace>].
	DirectoryRoot = "kine"
	// StorageFormat is the on-disk layout version recorded in each namespace.
	StorageFormat = 1

	// maxFDBKeySize is FoundationDB's key size limit.
	maxFDBKeySize = 10_000
)

// directoryLayer tags namespace directories so that unrelated directories
// with the same path are refused instead of being reinterpreted.
var directoryLayer = []byte("kine-fdb")

// keyspace computes the FoundationDB keys of one namespace.
//
//	(m, "format")          storage format version
//	(m, "rev")             current revision
//	(m, "compact")         compact revision: reads and watches below it fail
//	(m, "compacted")       history below this revision has been cleaned up
//	(k) + <key>            latest state of <key> (record.Index); raw key
//	                       bytes so that etcd ranges map to key ranges
//	(h, <key>, rev)        version of <key> written at rev (record.Entry)
//	(v, <key>, rev, i)     chunk i of a value too large to store inline
//	(r, rev)               change log: which key changed at rev (record.LogEntry)
type keyspace struct {
	namespace string
	dir       subspace.Subspace

	metaFormat    fdb.Key
	metaRev       fdb.Key
	metaCompact   fdb.Key
	metaCompacted fdb.Key

	index    []byte
	indexEnd []byte
	history  subspace.Subspace
	values   subspace.Subspace
	log      subspace.Subspace
}

func newKeyspace(namespace string, dir subspace.Subspace) *keyspace {
	meta := dir.Sub("m")
	index := dir.Sub("k").Bytes()
	return &keyspace{
		namespace:     namespace,
		dir:           dir,
		metaFormat:    meta.Pack(tuple.Tuple{"format"}),
		metaRev:       meta.Pack(tuple.Tuple{"rev"}),
		metaCompact:   meta.Pack(tuple.Tuple{"compact"}),
		metaCompacted: meta.Pack(tuple.Tuple{"compacted"}),
		index:         index,
		indexEnd:      strinc(index),
		history:       dir.Sub("h"),
		values:        dir.Sub("v"),
		log:           dir.Sub("r"),
	}
}

func namespacePath(namespace string) []string {
	return []string{DirectoryRoot, namespace}
}

// all is the whole namespace.
func (k *keyspace) all() fdb.KeyRange {
	prefix := k.dir.Bytes()
	return fdb.KeyRange{Begin: fdb.Key(prefix), End: fdb.Key(strinc(prefix))}
}

func (k *keyspace) indexKey(key string) fdb.Key {
	b := make([]byte, 0, len(k.index)+len(key))
	return append(append(b, k.index...), key...)
}

func (k *keyspace) keyFromIndex(fk fdb.Key) (string, error) {
	if !bytes.HasPrefix(fk, k.index) {
		return "", fmt.Errorf("fdb: index key %q outside namespace %s", fk, k.namespace)
	}
	return string(fk[len(k.index):]), nil
}

// indexRange maps a kine key range onto the index. kine passes key == "" for
// "every key", end == "" for "exactly key", and otherwise the half-open range
// [key, end). The second result is false when the range is empty.
func (k *keyspace) indexRange(key, end string) (fdb.KeyRange, bool) {
	switch {
	case key == "":
		return fdb.KeyRange{Begin: fdb.Key(k.index), End: fdb.Key(k.indexEnd)}, true
	case end == "":
		begin := k.indexKey(key)
		return fdb.KeyRange{Begin: begin, End: append(append(fdb.Key{}, begin...), 0)}, true
	case key >= end:
		return fdb.KeyRange{}, false
	default:
		return fdb.KeyRange{Begin: k.indexKey(key), End: k.indexKey(end)}, true
	}
}

func (k *keyspace) historyKey(key string, rev int64) fdb.Key {
	return k.history.Pack(tuple.Tuple{[]byte(key), rev})
}

// historyUpTo covers every version of key with revision <= rev.
func (k *keyspace) historyUpTo(key string, rev int64) fdb.KeyRange {
	sub := k.history.Sub([]byte(key))
	return fdb.KeyRange{Begin: tupleRangeBegin(sub), End: sub.Pack(tuple.Tuple{rev + 1})}
}

// historyAll covers every version of key.
func (k *keyspace) historyAll(key string) fdb.KeyRange {
	return tupleRange(k.history.Sub([]byte(key)))
}

// tupleRange covers the tuples that extend sub. It must not be the whole
// prefix range: an escaped NUL inside a byte-string element encodes as
// 0x00 0xFF, so the prefix of key "a" is also a prefix of key "a\x00...".
// Tuple elements that extend sub start with a type code below 0xFF.
func tupleRange(sub subspace.Subspace) fdb.KeyRange {
	return fdb.KeyRange{Begin: tupleRangeBegin(sub), End: append(append(fdb.Key{}, sub.Bytes()...), 0xff)}
}

func tupleRangeBegin(sub subspace.Subspace) fdb.Key {
	return append(append(fdb.Key{}, sub.Bytes()...), 0x00)
}

func (k *keyspace) historyRevision(fk fdb.Key) (string, int64, error) {
	t, err := k.history.Unpack(fk)
	if err != nil || len(t) != 2 {
		return "", 0, fmt.Errorf("fdb: malformed history key %q", fk)
	}
	key, ok1 := t[0].([]byte)
	rev, ok2 := t[1].(int64)
	if !ok1 || !ok2 {
		return "", 0, fmt.Errorf("fdb: malformed history key %q", fk)
	}
	return string(key), rev, nil
}

func (k *keyspace) chunkKey(key string, rev int64, i int) fdb.Key {
	return k.values.Pack(tuple.Tuple{[]byte(key), rev, i})
}

func (k *keyspace) chunkRange(key string, rev int64) fdb.KeyRange {
	return tupleRange(k.values.Sub([]byte(key), rev))
}

func (k *keyspace) logKey(rev int64) fdb.Key {
	return k.log.Pack(tuple.Tuple{rev})
}

// logRange covers revisions in [from, to).
func (k *keyspace) logRange(from, to int64) fdb.KeyRange {
	return fdb.KeyRange{Begin: k.logKey(from), End: k.logKey(to)}
}

func (k *keyspace) logRevision(fk fdb.Key) (int64, error) {
	t, err := k.log.Unpack(fk)
	if err != nil || len(t) != 1 {
		return 0, fmt.Errorf("fdb: malformed log key %q", fk)
	}
	rev, ok := t[0].(int64)
	if !ok {
		return 0, fmt.Errorf("fdb: malformed log key %q", fk)
	}
	return rev, nil
}

// checkKeySize rejects keys whose derived FoundationDB keys would exceed the
// FoundationDB key size limit.
func (k *keyspace) checkKeySize(key string) error {
	if key == "" {
		return errEmptyKey
	}
	if n := len(k.chunkKey(key, 1<<62, 1<<30)); n > maxFDBKeySize {
		return fmt.Errorf("fdb: key of %d bytes is too large to store", len(key))
	}
	return nil
}

var errEmptyKey = errors.New("fdb: key must not be empty")

// strinc returns the first key that does not have prefix as a prefix.
func strinc(prefix []byte) []byte {
	b := bytes.TrimRight(prefix, "\xff")
	if len(b) == 0 {
		// Only reachable for an all-0xFF prefix, which no namespace uses.
		return []byte{0xff}
	}
	out := append([]byte{}, b...)
	out[len(out)-1]++
	return out
}

// openDirectory opens (or with create, creates) the directory of a namespace.
func openDirectory(tr fdb.Transaction, namespace string, create bool) (subspace.Subspace, error) {
	var (
		dir directory.DirectorySubspace
		err error
	)
	if create {
		dir, err = directory.CreateOrOpen(tr, namespacePath(namespace), directoryLayer)
	} else {
		dir, err = directory.Open(tr, namespacePath(namespace), directoryLayer)
	}
	if errors.Is(err, directory.ErrDirNotExists) {
		return nil, fmt.Errorf("%w: %s", ErrNamespaceNotFound, namespace)
	}
	if err != nil {
		return nil, fmt.Errorf("fdb: open namespace directory %s: %w", namespace, err)
	}
	return dir, nil
}
