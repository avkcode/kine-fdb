// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Compaction. Moving the compact revision forward is cheap and immediate;
// the history it makes unreachable is then deleted in small resumable
// batches driven by the change log, so storage stays bounded.

package fdbdriver

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	kinemetrics "github.com/k3s-io/kine/pkg/metrics"
	"github.com/k3s-io/kine/pkg/server"
	"github.com/sirupsen/logrus"

	"github.com/avkcode/kine-fdb/internal/record"
)

// Compact handles compaction requests from etcd clients. As with kine's SQL
// backends, requests only take effect when automatic compaction is disabled;
// otherwise the driver compacts on its own schedule.
func (b *backend) Compact(ctx context.Context, revision int64) (int64, error) {
	ret, err := b.read(ctx, "compact-check", func(tr fdb.Transaction) (any, error) {
		return b.readRevisions(tr).get()
	})
	if err != nil {
		return 0, err
	}
	revs := ret.(revisions)
	if revision > revs.current {
		return 0, server.ErrFutureRev
	}
	if revision <= revs.compact {
		return 0, server.ErrCompacted
	}
	if b.cfg.CompactInterval > 0 {
		return revs.current, nil
	}
	target := safeCompactRevision(revision, revs.current, b.cfg.CompactMinRetain)
	if target > revs.compact {
		if _, err := b.compactTo(ctx, target); err != nil {
			kinemetrics.CompactTotal.WithLabelValues(kinemetrics.ResultError).Inc()
			return 0, err
		}
		kinemetrics.CompactTotal.WithLabelValues(kinemetrics.ResultSuccess).Inc()
	}
	return b.CurrentRevision(ctx)
}

// safeCompactRevision keeps the newest minRetain revisions out of reach of
// compaction, like kine's SQL backends.
func safeCompactRevision(target, current, minRetain int64) int64 {
	return max(0, min(target, current-minRetain))
}

// CompactStats describes one compaction run.
type CompactStats struct {
	// Revision is the compact revision after the run.
	Revision int64 `json:"revision"`
	// CurrentRevision is the namespace revision when the run started.
	CurrentRevision int64 `json:"currentRevision"`
	// Revisions is the number of change-log entries cleaned up.
	Revisions int64 `json:"revisions"`
	// DeletedVersions is the number of history entries removed.
	DeletedVersions int64 `json:"deletedVersions"`
	// Transactions is the number of cleanup transactions.
	Transactions int `json:"transactions"`
}

// compactTo moves the compact revision to target (if it is not already past
// it) and removes everything that became unreachable.
func (s *store) compactTo(ctx context.Context, target int64) (CompactStats, error) {
	ret, err := s.run(ctx, txnOptions{op: "compact", write: true, timeout: s.cfg.CompactTimeout}, func(tr fdb.Transaction, _ *attempt) (any, error) {
		revs, err := s.readCompaction(tr).get()
		if err != nil {
			return nil, err
		}
		if target > revs.current {
			return nil, server.ErrFutureRev
		}
		if target > revs.compact {
			tr.Set(s.ks.metaCompact, record.EncodeInt64(target))
			revs.compact = target
		}
		return revs, nil
	})
	if err != nil {
		return CompactStats{}, err
	}
	revs := ret.(revisions)
	s.metrics.compactRevision(revs.compact)
	stats, err := s.cleanup(ctx)
	stats.CurrentRevision = revs.current
	if stats.Revision == 0 {
		stats.Revision = revs.compact
	}
	return stats, err
}

// cleanup deletes history below the compact revision in batches until
// nothing is left. It is safe to run concurrently from several processes and
// to interrupt at any point.
func (s *store) cleanup(ctx context.Context) (CompactStats, error) {
	var stats CompactStats
	start := time.Now()
	for {
		ret, err := s.run(ctx, txnOptions{op: "compact-cleanup", write: true, timeout: s.cfg.CompactTimeout}, func(tr fdb.Transaction, _ *attempt) (any, error) {
			return s.cleanupBatch(tr)
		})
		if err != nil {
			return stats, err
		}
		batch := ret.(cleanupResult)
		stats.Revision = batch.compact
		if batch.revisions == 0 {
			break
		}
		stats.Revisions += batch.revisions
		stats.DeletedVersions += batch.deleted
		stats.Transactions++
		s.metrics.compacted(batch.deleted)
	}
	if stats.Transactions > 0 {
		logrus.Infof("fdb: compacted %s to revision %d: %d revisions, %d versions removed in %d transactions over %s",
			s.ks.namespace, stats.Revision, stats.Revisions, stats.DeletedVersions, stats.Transactions, time.Since(start).Round(time.Millisecond))
	}
	return stats, nil
}

type cleanupResult struct {
	compact   int64
	revisions int64
	deleted   int64
}

// cleanupBatch processes up to CompactBatchSize change-log entries below the
// compact revision. For each entry it deletes the version the entry
// superseded; deletions also drop their tombstone and, when the key has not
// been recreated since, its index row. The entry at the compact revision
// itself is kept so that watches may still start there.
func (s *store) cleanupBatch(tr fdb.Transaction) (cleanupResult, error) {
	revs, err := s.readCompaction(tr).get()
	if err != nil {
		return cleanupResult{}, err
	}
	res := cleanupResult{compact: revs.compact}
	if revs.compacted >= revs.compact {
		return res, nil
	}
	from := revs.compacted
	to := min(revs.compact, from+s.cfg.CompactBatchSize)
	entries, err := tr.GetRange(s.ks.logRange(from, to), fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
	if err != nil {
		return cleanupResult{}, err
	}

	type tombstone struct {
		key string
		rev int64
		idx fdb.FutureByteSlice
	}
	var tombstones []tombstone
	for _, e := range entries {
		rev, err := s.ks.logRevision(e.Key)
		if err != nil {
			return cleanupResult{}, err
		}
		l, err := record.DecodeLog(e.Value)
		if err != nil {
			return cleanupResult{}, fmt.Errorf("fdb: decode change log at %d: %w", rev, err)
		}
		key := string(l.Key)
		if l.PrevRevision > 0 {
			tr.Clear(s.ks.historyKey(key, l.PrevRevision))
			if l.PrevChunked {
				tr.ClearRange(s.ks.chunkRange(key, l.PrevRevision))
			}
			res.deleted++
		}
		if l.Deleted {
			tr.Clear(s.ks.historyKey(key, rev))
			res.deleted++
			tombstones = append(tombstones, tombstone{key: key, rev: rev, idx: tr.Get(s.ks.indexKey(key))})
		}
	}
	for _, t := range tombstones {
		raw, err := t.idx.Get()
		if err != nil {
			return cleanupResult{}, err
		}
		if raw == nil {
			continue
		}
		idx, err := record.DecodeIndex(raw)
		if err != nil {
			return cleanupResult{}, fmt.Errorf("fdb: decode index of %q: %w", t.key, err)
		}
		if idx.Deleted && idx.ModRevision == t.rev {
			tr.Clear(s.ks.indexKey(t.key))
		}
	}
	tr.ClearRange(s.ks.logRange(from, to))
	tr.Set(s.ks.metaCompacted, record.EncodeInt64(to))
	res.revisions = to - from
	return res, nil
}

// startCompactor runs automatic compaction in the background. Every
// interval it compacts up to the revision that was current one interval
// earlier, never touching the newest CompactMinRetain revisions, and it
// finishes any cleanup an earlier process left behind.
func (b *backend) startCompactor(ctx context.Context) {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		if _, err := b.cleanup(ctx); err != nil && ctx.Err() == nil {
			logrus.Errorf("fdb: resume compaction cleanup: %v", err)
		}
		if b.cfg.CompactInterval <= 0 {
			logrus.Debugf("fdb: automatic compaction disabled")
			return
		}
		interval := b.cfg.CompactInterval
		if j := b.cfg.CompactIntervalJitter; j > 0 {
			spread := float64(interval) * float64(j) / 100
			interval += time.Duration(rand.Float64()*2*spread - spread)
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		target, _ := b.CurrentRevision(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			target = b.compactTick(ctx, target)
		}
	}()
}

// compactTick runs one automatic compaction toward target and returns the
// target for the next tick.
func (b *backend) compactTick(ctx context.Context, target int64) int64 {
	ret, err := b.read(ctx, "compact-check", func(tr fdb.Transaction) (any, error) {
		return b.readRevisions(tr).get()
	})
	if err != nil {
		if ctx.Err() == nil {
			logrus.Errorf("fdb: automatic compaction: %v", err)
			kinemetrics.CompactTotal.WithLabelValues(kinemetrics.ResultError).Inc()
		}
		return target
	}
	revs := ret.(revisions)
	safe := safeCompactRevision(target, revs.current, b.cfg.CompactMinRetain)
	if safe > revs.compact || revs.compacted < revs.compact {
		if _, err := b.compactTo(ctx, max(safe, revs.compact)); err != nil {
			if ctx.Err() == nil {
				logrus.Errorf("fdb: automatic compaction: %v", err)
				kinemetrics.CompactTotal.WithLabelValues(kinemetrics.ResultError).Inc()
			}
			return target
		}
	}
	kinemetrics.CompactTotal.WithLabelValues(kinemetrics.ResultSuccess).Inc()
	return revs.current
}
