// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Administrative operations used by kine-fdb-admin: listing namespaces,
// collecting statistics, compacting, deleting, and verifying the internal
// consistency of a namespace.

package fdbdriver

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"

	"github.com/avkcode/kine-fdb/internal/record"
)

// scanPageKeys bounds each transaction of a full-namespace scan.
const scanPageKeys = 5000

// Admin performs maintenance on namespaces without running a server.
type Admin struct {
	cfg  Config
	s    *store
	ping *store
}

// OpenAdmin connects to the cluster named by endpoint. Methods that take a
// namespace use the endpoint's namespace when given "".
func OpenAdmin(endpoint string) (*Admin, error) {
	cfg, err := ParseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	db, err := openDatabase(cfg)
	if err != nil {
		return nil, err
	}
	return &Admin{cfg: cfg, s: &store{cfg: cfg, db: db}}, nil
}

func (a *Admin) open(ctx context.Context, namespace string) (*store, error) {
	cfg := a.cfg
	if namespace != "" {
		ns, err := NormalizeNamespace(namespace)
		if err != nil {
			return nil, err
		}
		cfg.Namespace = ns
	}
	cfg.StartupTimeout = cfg.TxnTimeout
	return openStore(ctx, cfg, false)
}

// Ping checks that the cluster is reachable and the namespace exists, with
// a single small read. It is meant for readiness probes.
func (a *Admin) Ping(ctx context.Context) error {
	if a.ping == nil {
		s, err := a.open(ctx, "")
		if err != nil {
			return err
		}
		a.ping = s
	}
	_, err := a.ping.read(ctx, "ping", func(tr fdb.Transaction) (any, error) {
		return a.ping.readCurrent(tr).get()
	})
	return err
}

// Namespaces lists every namespace in the cluster, sorted.
func (a *Admin) Namespaces(ctx context.Context) ([]string, error) {
	ret, err := a.s.read(ctx, "namespaces", func(tr fdb.Transaction) (any, error) {
		exists, err := directory.Exists(tr, []string{DirectoryRoot})
		if err != nil || !exists {
			return []string(nil), err
		}
		return directory.List(tr, []string{DirectoryRoot})
	})
	if err != nil {
		return nil, err
	}
	names := ret.([]string)
	sort.Strings(names)
	return names, nil
}

// Stats describes the contents of a namespace.
type Stats struct {
	Namespace         string `json:"namespace"`
	Format            int64  `json:"format"`
	CurrentRevision   int64  `json:"currentRevision"`
	CompactRevision   int64  `json:"compactRevision"`
	CompactedRevision int64  `json:"compactedRevision"`
	// Keys and DeletedKeys count index rows for live and deleted keys.
	Keys        int64 `json:"keys"`
	DeletedKeys int64 `json:"deletedKeys"`
	// LiveValueBytes is the total size of the current values of live keys.
	LiveValueBytes int64 `json:"liveValueBytes"`
	Versions       int64 `json:"versions"`
	ValueChunks    int64 `json:"valueChunks"`
	LogEntries     int64 `json:"logEntries"`
	// TotalKeyBytes and TotalValueBytes are the exact sizes of every
	// FoundationDB key and value in the namespace.
	TotalKeyBytes   int64 `json:"totalKeyBytes"`
	TotalValueBytes int64 `json:"totalValueBytes"`
	// EstimatedBytes is FoundationDB's sampled size estimate, the figure
	// reported to etcd clients as the database size.
	EstimatedBytes int64 `json:"estimatedBytes"`
}

// Stats scans a namespace. The scan spans many transactions, so on a busy
// namespace the counts are approximate.
func (a *Admin) Stats(ctx context.Context, namespace string) (Stats, error) {
	s, err := a.open(ctx, namespace)
	if err != nil {
		return Stats{}, err
	}
	st := Stats{Namespace: s.ks.namespace}
	ret, err := s.read(ctx, "stats", func(tr fdb.Transaction) (any, error) {
		revs, err := s.readRevisions(tr).get()
		if err != nil {
			return nil, err
		}
		raw, err := tr.Get(s.ks.metaFormat).Get()
		if err != nil {
			return nil, err
		}
		format, err := record.DecodeInt64(raw)
		if err != nil {
			return nil, err
		}
		est, err := tr.GetEstimatedRangeSizeBytes(s.ks.all()).Get()
		return [5]int64{revs.current, revs.compact, revs.compacted, format, est}, err
	})
	if err != nil {
		return Stats{}, err
	}
	v := ret.([5]int64)
	st.CurrentRevision, st.CompactRevision, st.CompactedRevision, st.Format, st.EstimatedBytes = v[0], v[1], v[2], v[3], v[4]

	historyPrefix := s.ks.history.Bytes()
	valuesPrefix := s.ks.values.Bytes()
	logPrefix := s.ks.log.Bytes()
	err = s.scan(ctx, s.ks.all(), func(kv fdb.KeyValue) error {
		st.TotalKeyBytes += int64(len(kv.Key))
		st.TotalValueBytes += int64(len(kv.Value))
		switch {
		case bytes.HasPrefix(kv.Key, s.ks.index):
			idx, err := record.DecodeIndex(kv.Value)
			if err != nil {
				return err
			}
			if idx.Deleted {
				st.DeletedKeys++
			} else {
				st.Keys++
				st.LiveValueBytes += int64(idx.ValueSize)
			}
		case bytes.HasPrefix(kv.Key, historyPrefix):
			st.Versions++
		case bytes.HasPrefix(kv.Key, valuesPrefix):
			st.ValueChunks++
		case bytes.HasPrefix(kv.Key, logPrefix):
			st.LogEntries++
		}
		return nil
	})
	return st, err
}

// scan calls fn for every key-value pair in rng, a page per transaction.
func (s *store) scan(ctx context.Context, rng fdb.KeyRange, fn func(fdb.KeyValue) error) error {
	begin := rng.Begin.FDBKey()
	end := rng.End.FDBKey()
	for {
		ret, err := s.read(ctx, "scan", func(tr fdb.Transaction) (any, error) {
			return tr.GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{Limit: scanPageKeys, Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
		})
		if err != nil {
			return err
		}
		kvs := ret.([]fdb.KeyValue)
		for _, kv := range kvs {
			if err := fn(kv); err != nil {
				return err
			}
		}
		if len(kvs) < scanPageKeys {
			return nil
		}
		begin = keyAfter(kvs[len(kvs)-1].Key)
	}
}

// Compact compacts a namespace to revision, or with retain > 0 to the
// current revision minus retain, and deletes everything that becomes
// unreachable. It also finishes any cleanup left by an interrupted run.
func (a *Admin) Compact(ctx context.Context, namespace string, revision, retain int64) (CompactStats, error) {
	s, err := a.open(ctx, namespace)
	if err != nil {
		return CompactStats{}, err
	}
	ret, err := s.read(ctx, "compact-check", func(tr fdb.Transaction) (any, error) {
		return s.readRevisions(tr).get()
	})
	if err != nil {
		return CompactStats{}, err
	}
	revs := ret.(revisions)
	if retain > 0 {
		revision = max(0, revs.current-retain)
	}
	if revision > revs.current {
		return CompactStats{}, fmt.Errorf("fdb: revision %d is newer than the current revision %d", revision, revs.current)
	}
	return s.compactTo(ctx, max(revision, revs.compact))
}

// Delete removes a namespace and all of its data. A kine-fdb server still
// using the namespace fails its requests until it is restarted.
func (a *Admin) Delete(ctx context.Context, namespace string) error {
	ns := a.cfg.Namespace
	if namespace != "" {
		var err error
		if ns, err = NormalizeNamespace(namespace); err != nil {
			return err
		}
	}
	_, err := a.s.run(ctx, txnOptions{op: "delete-namespace", write: true}, func(tr fdb.Transaction, _ *attempt) (any, error) {
		removed, err := directory.Root().Remove(tr, namespacePath(ns))
		if err != nil {
			return nil, err
		}
		if !removed {
			return nil, fmt.Errorf("%w: %s", ErrNamespaceNotFound, ns)
		}
		return nil, nil
	})
	return err
}

// VerifyReport lists inconsistencies found by Verify.
type VerifyReport struct {
	Namespace string   `json:"namespace"`
	Checked   int64    `json:"checked"`
	Problems  []string `json:"problems"`
	// Truncated is set when more problems were found than reported.
	Truncated bool `json:"truncated,omitempty"`
}

// OK reports whether no problems were found.
func (r VerifyReport) OK() bool { return len(r.Problems) == 0 }

const maxReportedProblems = 100

func (r *VerifyReport) addf(format string, args ...any) {
	if len(r.Problems) >= maxReportedProblems {
		r.Truncated = true
		return
	}
	r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
}

// Verify checks the invariants the driver maintains: every index row points
// at the newest history entry of its key, the change log is complete from
// the cleanup point to the current revision, chunked values are whole, and
// compaction left nothing behind that it should have removed. It holds the
// namespace's version map in memory, and concurrent writes can produce false
// reports, so run it on a quiet namespace.
func (a *Admin) Verify(ctx context.Context, namespace string) (VerifyReport, error) {
	s, err := a.open(ctx, namespace)
	if err != nil {
		return VerifyReport{}, err
	}
	return s.verify(ctx)
}

func (s *store) verify(ctx context.Context) (VerifyReport, error) {
	rep := VerifyReport{Namespace: s.ks.namespace, Problems: []string{}}
	ret, err := s.read(ctx, "verify", func(tr fdb.Transaction) (any, error) {
		return s.readRevisions(tr).get()
	})
	if err != nil {
		return rep, err
	}
	revs := ret.(revisions)
	if revs.compacted > revs.compact || revs.compact > revs.current {
		rep.addf("metadata out of order: compacted=%d compact=%d current=%d", revs.compacted, revs.compact, revs.current)
	}

	type versionKey struct {
		key string
		rev int64
	}
	type keyHistory struct {
		last        int64
		lastDeleted bool
		indexed     bool
	}
	histories := map[string]*keyHistory{}
	versions := map[versionKey]int{} // chunk count, -1 for inline values and tombstones

	var group string
	var groupRevs []int64
	var groupDeleted []bool
	flush := func() {
		if len(groupRevs) == 0 {
			return
		}
		for i := 0; i+1 < len(groupRevs); i++ {
			if groupRevs[i+1] < revs.compacted {
				rep.addf("version %q@%d should have been compacted: superseded at %d, below the cleanup point %d", group, groupRevs[i], groupRevs[i+1], revs.compacted)
			}
		}
		n := len(groupRevs) - 1
		if groupDeleted[n] && groupRevs[n] < revs.compacted {
			rep.addf("deletion %q@%d should have been compacted: below the cleanup point %d", group, groupRevs[n], revs.compacted)
		}
		histories[group] = &keyHistory{last: groupRevs[n], lastDeleted: groupDeleted[n]}
		groupRevs, groupDeleted = groupRevs[:0], groupDeleted[:0]
	}
	historyRange := fdb.KeyRange{Begin: fdb.Key(s.ks.history.Bytes()), End: fdb.Key(strinc(s.ks.history.Bytes()))}
	err = s.scan(ctx, historyRange, func(kv fdb.KeyValue) error {
		rep.Checked++
		v, err := s.decodeVersion(kv)
		if err != nil {
			rep.addf("%v", err)
			return nil
		}
		if v.key != group {
			flush()
			group = v.key
		}
		groupRevs = append(groupRevs, v.rev)
		groupDeleted = append(groupDeleted, v.entry.Deleted)
		if v.rev > revs.current {
			rep.addf("version %q@%d is newer than the current revision %d", v.key, v.rev, revs.current)
		}
		chunks := -1
		if v.entry.Chunks > 0 {
			chunks = v.entry.Chunks
		}
		versions[versionKey{v.key, v.rev}] = chunks
		return nil
	})
	if err != nil {
		return rep, err
	}
	flush()

	err = s.scan(ctx, fdb.KeyRange{Begin: fdb.Key(s.ks.index), End: fdb.Key(s.ks.indexEnd)}, func(kv fdb.KeyValue) error {
		rep.Checked++
		key, err := s.ks.keyFromIndex(kv.Key)
		if err != nil {
			rep.addf("%v", err)
			return nil
		}
		idx, err := record.DecodeIndex(kv.Value)
		if err != nil {
			rep.addf("index of %q: %v", key, err)
			return nil
		}
		h := histories[key]
		switch {
		case h == nil:
			rep.addf("index of %q points at revision %d but the key has no history", key, idx.ModRevision)
			return nil
		case h.last != idx.ModRevision:
			rep.addf("index of %q points at revision %d but its newest version is %d", key, idx.ModRevision, h.last)
		case h.lastDeleted != idx.Deleted:
			rep.addf("index of %q disagrees with history about deletion at revision %d", key, idx.ModRevision)
		}
		if idx.Deleted && idx.ModRevision < revs.compacted {
			rep.addf("deleted key %q@%d should have left the index: below the cleanup point %d", key, idx.ModRevision, revs.compacted)
		}
		h.indexed = true
		return nil
	})
	if err != nil {
		return rep, err
	}
	for key, h := range histories {
		if !h.indexed {
			rep.addf("history of %q (newest %d) has no index row", key, h.last)
		}
	}

	chunkCounts := map[versionKey]int{}
	valuesRange := fdb.KeyRange{Begin: fdb.Key(s.ks.values.Bytes()), End: fdb.Key(strinc(s.ks.values.Bytes()))}
	err = s.scan(ctx, valuesRange, func(kv fdb.KeyValue) error {
		rep.Checked++
		t, err := s.ks.values.Unpack(kv.Key)
		if err != nil || len(t) != 3 {
			rep.addf("malformed chunk key %q", kv.Key)
			return nil
		}
		key, ok1 := t[0].([]byte)
		rev, ok2 := t[1].(int64)
		if !ok1 || !ok2 {
			rep.addf("malformed chunk key %q", kv.Key)
			return nil
		}
		chunkCounts[versionKey{string(key), rev}]++
		return nil
	})
	if err != nil {
		return rep, err
	}
	for vk, n := range chunkCounts {
		want, ok := versions[vk]
		switch {
		case !ok:
			rep.addf("%d orphaned value chunks for %q@%d", n, vk.key, vk.rev)
		case want != n:
			rep.addf("value of %q@%d has %d chunks, its entry expects %d", vk.key, vk.rev, n, want)
		}
	}
	for vk, want := range versions {
		if want > 0 && chunkCounts[vk] == 0 {
			rep.addf("value chunks of %q@%d are missing", vk.key, vk.rev)
		}
	}

	expected := max(revs.compacted, 1)
	logRange := fdb.KeyRange{Begin: fdb.Key(s.ks.log.Bytes()), End: fdb.Key(strinc(s.ks.log.Bytes()))}
	err = s.scan(ctx, logRange, func(kv fdb.KeyValue) error {
		rep.Checked++
		rev, err := s.ks.logRevision(kv.Key)
		if err != nil {
			rep.addf("%v", err)
			return nil
		}
		l, err := record.DecodeLog(kv.Value)
		if err != nil {
			rep.addf("change log at %d: %v", rev, err)
			return nil
		}
		switch {
		case rev < revs.compacted:
			rep.addf("change log entry %d should have been compacted: below the cleanup point %d", rev, revs.compacted)
		case rev != expected:
			rep.addf("change log jumps from %d to %d", expected-1, rev)
		}
		expected = rev + 1
		if _, ok := versions[versionKey{string(l.Key), rev}]; !ok && rev >= revs.compacted {
			rep.addf("change log entry %d names %q, which has no version at that revision", rev, l.Key)
		}
		return nil
	})
	if err != nil {
		return rep, err
	}
	if revs.current >= max(revs.compacted, 1) && expected != revs.current+1 {
		rep.addf("change log ends at %d but the current revision is %d", expected-1, revs.current)
	}
	return rep, nil
}
