// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// The kine server.Backend implementation: driver registration, startup, and
// the single-key read and write operations.

package fdbdriver

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/k3s-io/kine/pkg/drivers"
	"github.com/k3s-io/kine/pkg/server"
	"github.com/k3s-io/kine/pkg/ttl"
	"github.com/sirupsen/logrus"

	"github.com/avkcode/kine-fdb/internal/record"
)

func init() {
	drivers.Register(Scheme, New)
}

type backend struct {
	*store

	ctx    context.Context
	wg     *sync.WaitGroup
	hub    *hub
	writer *writer

	// currentRev caches the newest revision this process knows about, from
	// its own writes and from the watch poller. It backs CurrentRevision,
	// which kine calls frequently for watch progress notifications.
	currentRev atomic.Int64

	dbSizeMu   sync.Mutex
	dbSize     int64
	dbSizeTime time.Time
}

var _ server.Backend = (*backend)(nil)

// New is the kine driver constructor for the fdb scheme. FoundationDB is
// shared storage, so it asks kine embedders to enable leader election.
func New(ctx context.Context, wg *sync.WaitGroup, dc *drivers.Config) (bool, server.Backend, error) {
	cfg, err := NewConfig(dc)
	if err != nil {
		return false, nil, err
	}
	s, err := openStore(ctx, cfg, true)
	if err != nil {
		return false, nil, err
	}
	s.metrics.register(dc.MetricsRegisterer)
	logrus.Infof("fdb: using namespace %s (%s)", cfg.Namespace, cfg)

	b := &backend{store: s, ctx: ctx, wg: wg}
	b.hub = newHub(b)
	b.writer = newWriter(s, b.committed)
	b.writer.start(ctx, wg)
	return true, b, nil
}

// Start seeds the health key, then starts the watch poller, automatic
// compaction, and lease expiry.
func (b *backend) Start(ctx context.Context) error {
	// A fresh namespace ends up at revision 2 after the create and update,
	// matching kine's other backends and the apiserver storage tests.
	rev, err := b.Create(ctx, server.HealthKey, []byte(server.HealthVal), 0)
	switch {
	case err == nil:
		if _, _, _, err := b.Update(ctx, server.HealthKey, []byte(server.HealthVal), rev, 0); err != nil {
			logrus.Errorf("fdb: update health check key: %v", err)
		}
	case err == server.ErrKeyExists:
	default:
		return err
	}

	if err := b.hub.start(ctx, b.wg); err != nil {
		return err
	}
	b.startCompactor(ctx)
	go ttl.Run(ctx, b)
	return nil
}

// CurrentRevision returns the newest revision known to this process.
func (b *backend) CurrentRevision(ctx context.Context) (int64, error) {
	if rev := b.currentRev.Load(); rev > 0 {
		return rev, nil
	}
	ret, err := b.read(ctx, "current-revision", func(tr fdb.Transaction) (any, error) {
		r, err := b.readRevisions(tr).get()
		return r.current, err
	})
	if err != nil {
		return 0, err
	}
	rev := ret.(int64)
	b.observeRevision(rev)
	return rev, nil
}

// observeRevision raises the cached current revision to rev.
func (b *backend) observeRevision(rev int64) {
	for {
		cur := b.currentRev.Load()
		if rev <= cur || b.currentRev.CompareAndSwap(cur, rev) {
			return
		}
	}
}

// committed records a revision written by this process and wakes the watch
// poller so local writes reach watchers without waiting for a notification.
func (b *backend) committed(rev int64) {
	b.observeRevision(rev)
	b.hub.poke()
}

// DbSize returns FoundationDB's estimate of the namespace size. The estimate
// is sampled and can be coarse for small namespaces; it is cached briefly
// because the apiserver polls it.
func (b *backend) DbSize(ctx context.Context) (int64, error) {
	b.dbSizeMu.Lock()
	defer b.dbSizeMu.Unlock()
	if time.Since(b.dbSizeTime) < 10*time.Second {
		return b.dbSize, nil
	}
	ret, err := b.read(ctx, "db-size", func(tr fdb.Transaction) (any, error) {
		return tr.GetEstimatedRangeSizeBytes(b.ks.all()).Get()
	})
	if err != nil {
		return 0, err
	}
	b.dbSize, b.dbSizeTime = ret.(int64), time.Now()
	return b.dbSize, nil
}

type getResult struct {
	rev int64
	kv  *server.KeyValue
}

// Get returns key as of revision (zero for the current revision).
func (b *backend) Get(ctx context.Context, key string, revision int64, keysOnly bool) (int64, *server.KeyValue, error) {
	ret, err := b.read(ctx, "get", func(tr fdb.Transaction) (any, error) {
		revs := b.readRevisions(tr)
		floor := b.floor(tr, key, readTarget(revision))
		r, err := revs.get()
		if err != nil {
			return nil, err
		}
		if err := r.checkRead(revision); err != nil {
			return getResult{rev: r.current}, err
		}
		v, err := b.decodeFloor(floor)
		if err != nil || v == nil || v.entry.Deleted {
			return getResult{rev: r.current}, err
		}
		if keysOnly {
			return getResult{rev: r.current, kv: v.keyValue(nil)}, nil
		}
		value, err := b.readValue(tr, v)
		if err != nil {
			return nil, err
		}
		return getResult{rev: r.current, kv: v.keyValue(value)}, nil
	})
	res, _ := ret.(getResult)
	return res.rev, res.kv, err
}

// readTarget maps a requested revision to the newest revision to read.
// Revision zero reads the latest version; the metadata read in the same
// transaction supplies the header revision.
func readTarget(revision int64) int64 {
	if revision <= 0 {
		return latestRevision
	}
	return revision
}

type writeResult struct {
	rev int64
	kv  *server.KeyValue
	ok  bool
}

// Create stores key if it does not exist (or is deleted).
func (b *backend) Create(ctx context.Context, key string, value []byte, lease int64) (int64, error) {
	if err := b.ks.checkKeySize(key); err != nil {
		return 0, err
	}
	res, err := b.writer.submit(ctx, "create", key, len(value), b.decideCreate(key, value, lease))
	return res.rev, err
}

// Update replaces key if its current revision is revision.
func (b *backend) Update(ctx context.Context, key string, value []byte, revision, lease int64) (int64, *server.KeyValue, bool, error) {
	if err := b.ks.checkKeySize(key); err != nil {
		return 0, nil, false, err
	}
	res, err := b.writer.submit(ctx, "update", key, len(value), b.decideUpdate(key, value, revision, lease))
	return res.rev, res.kv, res.ok, err
}

// Delete removes key, optionally only if its current revision is revision.
// Deleting a missing key succeeds only when no revision was required,
// matching etcd's transaction semantics.
func (b *backend) Delete(ctx context.Context, key string, revision int64) (int64, *server.KeyValue, bool, error) {
	if key == "" {
		return 0, nil, false, errEmptyKey
	}
	res, err := b.writer.submit(ctx, "delete", key, 0, b.decideDelete(key, revision))
	return res.rev, res.kv, res.ok, err
}

func (b *backend) decideCreate(key string, value []byte, lease int64) decideFunc {
	entry, chunks := record.NewEntry(value)
	entry.Version = 1
	entry.Lease = lease
	return func(tr fdb.Transaction, st writeState, next int64) (writeResult, *mutation, error) {
		for _, v := range st.earlier {
			if !v.entry.Deleted && v.entry.CreateRevision == v.rev && v.entry.Version == 1 && v.entry.Lease == lease {
				same, err := b.sameValue(tr, v, value)
				if err != nil {
					return writeResult{}, nil, err
				}
				if same {
					return writeResult{rev: v.rev, ok: true}, nil, nil
				}
			}
		}
		if st.latest != nil && !st.latest.entry.Deleted {
			return writeResult{rev: st.current}, nil, server.ErrKeyExists
		}
		e := entry
		e.CreateRevision = next
		l := record.LogEntry{Key: []byte(key), Create: true}
		if st.latest != nil {
			e.PrevRevision = st.latest.rev
			l.PrevRevision = st.latest.rev
		}
		return writeResult{rev: next, ok: true}, &mutation{key: key, rev: next, entry: e, chunks: chunks, log: l}, nil
	}
}

func (b *backend) decideUpdate(key string, value []byte, revision, lease int64) decideFunc {
	entry, chunks := record.NewEntry(value)
	entry.Lease = lease
	return func(tr fdb.Transaction, st writeState, next int64) (writeResult, *mutation, error) {
		for _, v := range st.earlier {
			if !v.entry.Deleted && v.entry.PrevRevision == revision && v.entry.Lease == lease {
				same, err := b.sameValue(tr, v, value)
				if err != nil {
					return writeResult{}, nil, err
				}
				if same {
					return writeResult{rev: v.rev, kv: v.keyValue(value), ok: true}, nil, nil
				}
			}
		}
		cur := st.latest
		if cur == nil || cur.entry.Deleted {
			return writeResult{rev: st.current}, nil, nil
		}
		if cur.rev != revision {
			value, err := b.readValue(tr, cur)
			if err != nil {
				return writeResult{}, nil, err
			}
			return writeResult{rev: st.current, kv: cur.keyValue(value)}, nil, nil
		}
		e := entry
		e.CreateRevision = cur.entry.CreateRevision
		e.Version = cur.entry.Version + 1
		e.PrevRevision = cur.rev
		m := &mutation{key: key, rev: next, entry: e, chunks: chunks, log: record.LogEntry{
			Key:          []byte(key),
			PrevRevision: cur.rev,
			PrevChunked:  cur.entry.Chunks > 0,
		}}
		updated := &version{key: key, rev: next, entry: e}
		return writeResult{rev: next, kv: updated.keyValue(value), ok: true}, m, nil
	}
}

func (b *backend) decideDelete(key string, revision int64) decideFunc {
	return func(tr fdb.Transaction, st writeState, next int64) (writeResult, *mutation, error) {
		for _, v := range st.earlier {
			if !v.entry.Deleted || (revision != 0 && v.entry.PrevRevision != revision) {
				continue
			}
			// An earlier attempt deleted the key; report the version it removed.
			res := writeResult{rev: v.rev, ok: true}
			prev, err := b.decodeExact(key, v.entry.PrevRevision, b.exact(tr, key, v.entry.PrevRevision))
			if err != nil {
				return writeResult{}, nil, err
			}
			if prev != nil {
				value, err := b.readValue(tr, prev)
				if err != nil {
					return writeResult{}, nil, err
				}
				res.kv = prev.keyValue(value)
			}
			return res, nil, nil
		}
		cur := st.latest
		if cur == nil || cur.entry.Deleted {
			return writeResult{rev: st.current, ok: revision == 0}, nil, nil
		}
		value, err := b.readValue(tr, cur)
		if err != nil {
			return writeResult{}, nil, err
		}
		if revision != 0 && cur.rev != revision {
			return writeResult{rev: st.current, kv: cur.keyValue(value)}, nil, nil
		}
		m := &mutation{key: key, rev: next, entry: record.Entry{Deleted: true, PrevRevision: cur.rev}, log: record.LogEntry{
			Key:          []byte(key),
			PrevRevision: cur.rev,
			Deleted:      true,
			PrevChunked:  cur.entry.Chunks > 0,
		}}
		return writeResult{rev: next, kv: cur.keyValue(value), ok: true}, m, nil
	}
}
