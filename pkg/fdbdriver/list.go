// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Range reads: List, ListStream, and Count. Large ranges are read in pages,
// each in its own transaction, pinned to one revision so that results stay
// consistent without exceeding FoundationDB's five-second transaction limit.

package fdbdriver

import (
	"context"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/k3s-io/kine/pkg/server"

	"github.com/avkcode/kine-fdb/internal/record"
)

const (
	// listPageKeys and listPageBytes bound one page transaction that returns
	// values; metadataPageKeys bounds pages answered from the index alone.
	listPageKeys     = 1000
	listPageBytes    = 8 << 20
	metadataPageKeys = 10_000
)

// rangeQuery is one List, ListStream, or Count request.
type rangeQuery struct {
	key, end  string
	limit     int64
	revision  int64
	keysOnly  bool
	countOnly bool
}

func (q rangeQuery) metadataOnly() bool { return q.keysOnly || q.countOnly }

// cursor walks a range page by page at a fixed revision.
type cursor struct {
	q     rangeQuery
	end   fdb.Key
	begin fdb.Key
	empty bool

	started bool
	// current is the namespace revision seen by the first page; it is the
	// header revision of the response.
	current int64
	// target is the revision every page reads at.
	target int64
	// found counts live keys returned so far, for the limit.
	found int64
	done  bool
}

func (b *backend) newCursor(q rangeQuery) *cursor {
	rng, ok := b.ks.indexRange(q.key, q.end)
	c := &cursor{q: q, empty: !ok}
	if ok {
		c.begin, c.end = rng.Begin.(fdb.Key), rng.End.(fdb.Key)
	}
	return c
}

// page is the outcome of one page transaction.
type page struct {
	current int64
	target  int64
	kvs     []*server.KeyValue
	count   int64
	// next is where the following page starts; nil when the range is done.
	next fdb.Key
}

// nextPage reads one page and advances the cursor. Count queries report
// the number of live keys instead of returning them.
func (b *backend) nextPage(ctx context.Context, c *cursor) ([]*server.KeyValue, int64, error) {
	if c.done {
		return nil, 0, nil
	}
	ret, err := b.read(ctx, "list", func(tr fdb.Transaction) (any, error) {
		return b.readPage(tr, c)
	})
	p, _ := ret.(*page)
	if !c.started && p != nil {
		c.current = p.current
	}
	if err != nil {
		return nil, 0, err
	}
	if !c.started {
		c.started = true
		c.target = p.target
	}
	c.found += int64(len(p.kvs)) + p.count
	if p.next == nil || (c.q.limit > 0 && c.found >= c.q.limit) {
		c.done = true
	} else {
		c.begin = p.next
	}
	return p.kvs, p.count, nil
}

// pendingRow is an index row whose state at the target revision is being read.
type pendingRow struct {
	key   string
	idx   record.Index
	exact fdb.FutureByteSlice // the version the index row points at
	floor *fdb.RangeResult    // the newest version at or below the target
}

func (b *backend) readPage(tr fdb.Transaction, c *cursor) (*page, error) {
	revsF := b.readRevisions(tr)
	pageKeys := listPageKeys
	if c.q.metadataOnly() {
		pageKeys = metadataPageKeys
	}
	if c.q.limit > 0 {
		// Deleted keys do not count against the limit, so read a little past
		// it; later pages pick up anything still missing.
		pageKeys = int(min(int64(pageKeys), c.q.limit-c.found+16))
	}
	var rowsRR fdb.RangeResult
	if !c.empty {
		rowsRR = tr.GetRange(fdb.KeyRange{Begin: c.begin, End: c.end}, fdb.RangeOptions{Limit: pageKeys, Mode: fdb.StreamingModeWantAll})
	}

	revs, err := revsF.get()
	if err != nil {
		return nil, err
	}
	p := &page{current: revs.current, target: c.target}
	if !c.started {
		if err := revs.checkRead(c.q.revision); err != nil {
			return p, err
		}
		p.target = revs.current
		if c.q.revision > 0 {
			p.target = c.q.revision
		}
	} else if p.target < revs.compact {
		return p, server.ErrCompacted
	}
	if c.empty {
		return p, nil
	}
	rows, err := rowsRR.GetSliceWithError()
	if err != nil {
		return nil, err
	}

	// Plan: decide for every row how its state at the target is found, and
	// start those reads together.
	budget := listPageBytes
	pending := make([]pendingRow, 0, len(rows))
	consumed := 0
	for _, row := range rows {
		if !c.q.metadataOnly() && budget <= 0 && len(pending) > 0 {
			break
		}
		consumed++
		key, err := b.ks.keyFromIndex(row.Key)
		if err != nil {
			return nil, err
		}
		idx, err := record.DecodeIndex(row.Value)
		if err != nil {
			return nil, fmt.Errorf("fdb: decode index of %q: %w", key, err)
		}
		r := pendingRow{key: key, idx: idx}
		switch {
		case idx.ModRevision <= p.target && idx.Deleted:
			continue
		case idx.ModRevision <= p.target && c.q.metadataOnly():
			// the index row is the answer
		case idx.ModRevision <= p.target:
			r.exact = b.exact(tr, key, idx.ModRevision)
			budget -= idx.ValueSize
		default:
			rr := b.floor(tr, key, p.target)
			r.floor = &rr
			budget -= idx.ValueSize
		}
		pending = append(pending, r)
	}
	if consumed < len(rows) || len(rows) == pageKeys {
		p.next = keyAfter(rows[consumed-1].Key)
	}

	// Resolve rows in key order, starting chunked-value reads as they are found.
	type liveRow struct {
		kv    *server.KeyValue
		v     *version
		value valueFuture
	}
	live := make([]liveRow, 0, len(pending))
	for _, r := range pending {
		if r.exact == nil && r.floor == nil {
			live = append(live, liveRow{kv: &server.KeyValue{
				Key:            r.key,
				CreateRevision: r.idx.CreateRevision,
				ModRevision:    r.idx.ModRevision,
				Version:        r.idx.Version,
				Lease:          r.idx.Lease,
			}})
			continue
		}
		var v *version
		if r.floor != nil {
			v, err = b.decodeFloor(*r.floor)
		} else {
			v, err = b.decodeExact(r.key, r.idx.ModRevision, r.exact)
			if err == nil && v == nil {
				err = fmt.Errorf("fdb: index of %q points at revision %d, which has no history entry", r.key, r.idx.ModRevision)
			}
		}
		if err != nil {
			return nil, err
		}
		if v == nil || v.entry.Deleted {
			continue
		}
		row := liveRow{v: v}
		if !c.q.metadataOnly() {
			row.value = b.loadValue(tr, v)
		}
		live = append(live, row)
	}

	for _, row := range live {
		if c.q.limit > 0 && c.found+int64(len(p.kvs))+p.count >= c.q.limit {
			break
		}
		if c.q.countOnly {
			p.count++
			continue
		}
		kv := row.kv
		if kv == nil {
			var value []byte
			if !c.q.keysOnly {
				if value, err = row.value.get(); err != nil {
					return nil, err
				}
			}
			kv = row.v.keyValue(value)
		}
		p.kvs = append(p.kvs, kv)
	}
	return p, nil
}

// keyAfter returns the smallest key greater than k.
func keyAfter(k fdb.Key) fdb.Key {
	return append(append(fdb.Key{}, k...), 0)
}

// List returns the live keys in range as of revision. kine passes key == ""
// for every key, end == "" for exactly key, and otherwise [key, end).
func (b *backend) List(ctx context.Context, key, end string, limit, revision int64, keysOnly bool) (int64, []*server.KeyValue, error) {
	c := b.newCursor(rangeQuery{key: key, end: end, limit: limit, revision: revision, keysOnly: keysOnly})
	var out []*server.KeyValue
	for !c.done {
		kvs, _, err := b.nextPage(ctx, c)
		if err != nil {
			return c.current, nil, err
		}
		out = append(out, kvs...)
	}
	return c.current, out, nil
}

// Count returns the number of live keys in range as of revision.
func (b *backend) Count(ctx context.Context, key, end string, revision int64) (int64, int64, error) {
	c := b.newCursor(rangeQuery{key: key, end: end, revision: revision, countOnly: true})
	var total int64
	for !c.done {
		_, n, err := b.nextPage(ctx, c)
		if err != nil {
			return c.current, 0, err
		}
		total += n
	}
	return c.current, total, nil
}

// ListStream is List delivered incrementally. The first page is read before
// returning so that validation errors and the header revision are known.
func (b *backend) ListStream(ctx context.Context, key, end string, limit, revision int64, keysOnly bool) server.ListResult {
	kvc := make(chan *server.KeyValue, 64)
	errc := make(chan error, 1)
	c := b.newCursor(rangeQuery{key: key, end: end, limit: limit, revision: revision, keysOnly: keysOnly})
	first, _, err := b.nextPage(ctx, c)
	if err != nil {
		errc <- err
		close(kvc)
		close(errc)
		return server.ListResult{CurrentRevision: c.current, KVc: kvc, Errorc: errc}
	}
	go func() {
		defer close(errc)
		defer close(kvc)
		kvs := first
		for {
			for _, kv := range kvs {
				select {
				case kvc <- kv:
				case <-ctx.Done():
					errc <- ctx.Err()
					return
				}
			}
			if c.done {
				return
			}
			if kvs, _, err = b.nextPage(ctx, c); err != nil {
				errc <- err
				return
			}
		}
	}()
	return server.ListResult{CurrentRevision: c.current, KVc: kvc, Errorc: errc}
}
