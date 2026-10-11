// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// The storage core shared by the kine backend and the admin tooling: opening
// a namespace, reading its metadata, locating key versions, and loading
// inline or chunked values.

package fdbdriver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/k3s-io/kine/pkg/server"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/avkcode/kine-fdb/internal/record"
)

// store is one open namespace.
type store struct {
	cfg     Config
	db      fdb.Database
	ks      *keyspace
	metrics *metrics
}

// openStore connects to the cluster and opens cfg.Namespace. With create it
// initializes a missing namespace. Transient unavailability is retried until
// cfg.StartupTimeout elapses so that kine-fdb can start before its cluster.
func openStore(ctx context.Context, cfg Config, create bool) (*store, error) {
	db, err := openDatabase(cfg)
	if err != nil {
		return nil, err
	}
	s := &store{cfg: cfg, db: db, metrics: newMetrics(cfg.Namespace)}

	ctx, cancel := context.WithTimeout(ctx, cfg.StartupTimeout)
	defer cancel()
	for {
		err := s.openNamespace(ctx, create)
		if err == nil {
			return s, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("fdb: namespace %s not available after %s: %w", cfg.Namespace, cfg.StartupTimeout, err)
		}
		if !isRetryableStartup(err) {
			return nil, err
		}
		logrus.Warnf("fdb: waiting for the FoundationDB cluster (%s): %v", cfg, err)
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *store) openNamespace(ctx context.Context, create bool) error {
	opts := txnOptions{op: "open", write: create, timeout: min(s.cfg.TxnTimeout, 10*time.Second)}
	ret, err := s.run(ctx, opts, func(tr fdb.Transaction, _ *attempt) (any, error) {
		dir, err := openDirectory(tr, s.cfg.Namespace, create)
		if err != nil {
			return nil, err
		}
		ks := newKeyspace(s.cfg.Namespace, dir)
		raw, err := tr.Get(ks.metaFormat).Get()
		if err != nil {
			return nil, err
		}
		if raw == nil {
			if !create {
				return nil, fmt.Errorf("%w: %s", ErrNamespaceNotFound, s.cfg.Namespace)
			}
			tr.Set(ks.metaFormat, record.EncodeInt64(StorageFormat))
			tr.Set(ks.metaRev, record.EncodeInt64(0))
			tr.Set(ks.metaCompact, record.EncodeInt64(0))
			tr.Set(ks.metaCompacted, record.EncodeInt64(0))
			return ks, nil
		}
		format, err := record.DecodeInt64(raw)
		if err != nil {
			return nil, err
		}
		if format != StorageFormat {
			return nil, fmt.Errorf("fdb: namespace %s uses storage format %d; this build supports format %d", s.cfg.Namespace, format, StorageFormat)
		}
		return ks, nil
	})
	if err != nil {
		return err
	}
	s.ks = ret.(*keyspace)
	return nil
}

// revisions is the namespace metadata read at the start of most operations.
type revisions struct {
	current   int64
	compact   int64
	compacted int64
}

type revisionsFuture struct {
	current, compact, compacted fdb.FutureByteSlice
}

// readRevisions reads all namespace metadata. Use it in read-only
// transactions, which never conflict.
func (s *store) readRevisions(tr fdb.ReadTransaction) revisionsFuture {
	return revisionsFuture{
		current:   tr.Get(s.ks.metaRev),
		compact:   tr.Get(s.ks.metaCompact),
		compacted: tr.Get(s.ks.metaCompacted),
	}
}

// readCompaction reads the metadata a compaction transaction needs. The
// current revision is a snapshot read so that compaction never conflicts
// with writers, which update it on every change.
func (s *store) readCompaction(tr fdb.Transaction) revisionsFuture {
	return revisionsFuture{
		current:   tr.Snapshot().Get(s.ks.metaRev),
		compact:   tr.Get(s.ks.metaCompact),
		compacted: tr.Get(s.ks.metaCompacted),
	}
}

// readCurrent reads only the current revision. Writers use it so that they
// conflict with each other (revisions must be dense) but never with
// compaction.
func (s *store) readCurrent(tr fdb.ReadTransaction) currentFuture {
	return currentFuture{tr.Get(s.ks.metaRev)}
}

type currentFuture struct{ f fdb.FutureByteSlice }

func (f currentFuture) get() (int64, error) {
	raw, err := f.f.Get()
	if err != nil {
		return 0, err
	}
	if raw == nil {
		return 0, errNamespaceGone
	}
	return record.DecodeInt64(raw)
}

func (f revisionsFuture) get() (revisions, error) {
	raw, err := f.current.Get()
	if err != nil {
		return revisions{}, err
	}
	if raw == nil {
		return revisions{}, errNamespaceGone
	}
	var r revisions
	if r.current, err = record.DecodeInt64(raw); err != nil {
		return revisions{}, err
	}
	for _, p := range []struct {
		f   fdb.FutureByteSlice
		dst *int64
	}{{f.compact, &r.compact}, {f.compacted, &r.compacted}} {
		raw, err := p.f.Get()
		if err != nil {
			return revisions{}, err
		}
		if *p.dst, err = record.DecodeInt64(raw); err != nil {
			return revisions{}, err
		}
	}
	return r, nil
}

// checkRead validates a requested read revision against the namespace state,
// with the same semantics as kine's reference backends: revision zero means
// the current revision, revisions above the current one do not exist yet,
// and revisions below the compact revision are gone.
func (r revisions) checkRead(revision int64) error {
	if revision <= 0 {
		return nil
	}
	if revision > r.current {
		return server.ErrFutureRev
	}
	if revision < r.compact {
		return server.ErrCompacted
	}
	return nil
}

// latestRevision reads the newest version of a key.
const latestRevision = math.MaxInt64 - 1

// version is one stored version of a key.
type version struct {
	key   string
	rev   int64
	entry record.Entry
}

// floor starts a read of the newest version of key at or below rev.
func (s *store) floor(tr fdb.ReadTransaction, key string, rev int64) fdb.RangeResult {
	return tr.GetRange(s.ks.historyUpTo(key, rev), fdb.RangeOptions{Limit: 1, Reverse: true, Mode: fdb.StreamingModeExact})
}

// decodeFloor resolves a floor read. It returns nil when the key has no
// version at or below the requested revision.
func (s *store) decodeFloor(rr fdb.RangeResult) (*version, error) {
	kvs, err := rr.GetSliceWithError()
	if err != nil || len(kvs) == 0 {
		return nil, err
	}
	return s.decodeVersion(kvs[0])
}

func (s *store) decodeVersion(kv fdb.KeyValue) (*version, error) {
	key, rev, err := s.ks.historyRevision(kv.Key)
	if err != nil {
		return nil, err
	}
	e, err := record.DecodeEntry(kv.Value)
	if err != nil {
		return nil, fmt.Errorf("fdb: decode %q@%d: %w", key, rev, err)
	}
	return &version{key: key, rev: rev, entry: e}, nil
}

// exact starts a read of the version of key written at rev.
func (s *store) exact(tr fdb.ReadTransaction, key string, rev int64) fdb.FutureByteSlice {
	return tr.Get(s.ks.historyKey(key, rev))
}

func (s *store) decodeExact(key string, rev int64, f fdb.FutureByteSlice) (*version, error) {
	raw, err := f.Get()
	if err != nil || raw == nil {
		return nil, err
	}
	e, err := record.DecodeEntry(raw)
	if err != nil {
		return nil, fmt.Errorf("fdb: decode %q@%d: %w", key, rev, err)
	}
	return &version{key: key, rev: rev, entry: e}, nil
}

// valueFuture is a value that is either already in hand or being read from
// its chunks.
type valueFuture struct {
	v      *version
	chunks *fdb.RangeResult
}

// loadValue starts loading the value of v.
func (s *store) loadValue(tr fdb.ReadTransaction, v *version) valueFuture {
	if v.entry.Chunks == 0 {
		return valueFuture{v: v}
	}
	rr := tr.GetRange(s.ks.chunkRange(v.key, v.rev), fdb.RangeOptions{Mode: fdb.StreamingModeWantAll})
	return valueFuture{v: v, chunks: &rr}
}

func (f valueFuture) get() ([]byte, error) {
	if f.chunks == nil {
		return f.v.entry.Value, nil
	}
	kvs, err := f.chunks.GetSliceWithError()
	if err != nil {
		return nil, err
	}
	if len(kvs) != f.v.entry.Chunks {
		return nil, fmt.Errorf("fdb: value of %q@%d has %d chunks, expected %d", f.v.key, f.v.rev, len(kvs), f.v.entry.Chunks)
	}
	value := make([]byte, 0, f.v.entry.ValueSize)
	for _, kv := range kvs {
		value = append(value, kv.Value...)
	}
	if len(value) != f.v.entry.ValueSize {
		return nil, fmt.Errorf("fdb: value of %q@%d is %d bytes, expected %d", f.v.key, f.v.rev, len(value), f.v.entry.ValueSize)
	}
	return value, nil
}

// readValue loads the value of v synchronously.
func (s *store) readValue(tr fdb.ReadTransaction, v *version) ([]byte, error) {
	return s.loadValue(tr, v).get()
}

// keyValue converts a live version to the kine representation.
func (v *version) keyValue(value []byte) *server.KeyValue {
	return &server.KeyValue{
		Key:            v.key,
		Value:          value,
		CreateRevision: v.entry.CreateRevision,
		ModRevision:    v.rev,
		Version:        v.entry.Version,
		Lease:          v.entry.Lease,
	}
}

// sameValue reports whether v holds exactly value, reading chunks if needed.
func (s *store) sameValue(tr fdb.ReadTransaction, v *version, value []byte) (bool, error) {
	if v.entry.ValueSize != len(value) {
		return false, nil
	}
	got, err := s.readValue(tr, v)
	if err != nil {
		return false, err
	}
	return bytes.Equal(got, value), nil
}

// mutation is everything written for one revision.
type mutation struct {
	key    string
	rev    int64
	entry  record.Entry
	chunks [][]byte
	log    record.LogEntry
}

func (s *store) apply(tr fdb.Transaction, m mutation) {
	tr.Set(s.ks.metaRev, record.EncodeInt64(m.rev))
	tr.Set(s.ks.historyKey(m.key, m.rev), record.EncodeEntry(m.entry))
	for i, c := range m.chunks {
		tr.Set(s.ks.chunkKey(m.key, m.rev, i), c)
	}
	idx := record.Index{ModRevision: m.rev, Deleted: m.entry.Deleted}
	if !m.entry.Deleted {
		idx.CreateRevision = m.entry.CreateRevision
		idx.Version = m.entry.Version
		idx.Lease = m.entry.Lease
		idx.ValueSize = m.entry.ValueSize
	}
	tr.Set(s.ks.indexKey(m.key), record.EncodeIndex(idx))
	tr.Set(s.ks.logKey(m.rev), record.EncodeLog(m.log))
}

// isRetryableStartup reports whether an error seen while opening a namespace
// is worth retrying.
func isRetryableStartup(err error) bool {
	return status.Code(err) == codes.Unavailable && !errors.Is(err, context.Canceled)
}
