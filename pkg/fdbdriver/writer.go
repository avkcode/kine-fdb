// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Group commit: every write of a process goes through one committer that
// packs the queued writes into a single FoundationDB transaction, giving each
// its own consecutive revision. Writes to a namespace must be serialized to
// keep revisions dense; batching them turns that serialization from one
// commit per write into one commit per batch.

package fdbdriver

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/sirupsen/logrus"
)

const (
	// maxBatchOps and maxBatchBytes bound one write transaction, well below
	// FoundationDB's 10 MB transaction limit.
	maxBatchOps   = 256
	maxBatchBytes = 4 << 20
	writeQueue    = 4096
)

// decideFunc decides the outcome of one write given the key's state. It
// returns the result for the caller and, when the write happens, the
// mutation to apply at revision next. It may read from tr (for example a
// chunked value) but must not write; errors that are not FoundationDB errors
// fail only this write.
type decideFunc func(tr fdb.Transaction, st writeState, next int64) (writeResult, *mutation, error)

// writeState is what a write decides on.
type writeState struct {
	// latest is the newest version of the key, including writes earlier in
	// the same batch; it may be a deletion.
	latest *version
	// earlier holds versions that earlier attempts of this write may have
	// committed (see writeOp.candidates).
	earlier []*version
	// current is the namespace revision before this write.
	current int64
}

type writeOp struct {
	ctx    context.Context
	op     string
	key    string
	size   int
	decide decideFunc
	start  time.Time
	done   chan writeOutcome

	// assigned is the revision this write took in the current attempt.
	assigned int64
	// candidates are revisions earlier attempts assigned to this write
	// before their commit ended with an unknown result. If one of them holds
	// exactly this write, the write already happened and must not be
	// applied again.
	candidates []int64
	outcome    writeOutcome
}

type writeOutcome struct {
	res writeResult
	err error
}

// writer is the per-backend committer.
type writer struct {
	s     *store
	queue chan *writeOp
	// committed is called with the newest revision of each committed batch.
	committed func(rev int64)

	stopOnce sync.Once
	stopped  chan struct{}
}

func newWriter(s *store, committed func(int64)) *writer {
	return &writer{s: s, queue: make(chan *writeOp, writeQueue), committed: committed, stopped: make(chan struct{})}
}

func (w *writer) start(ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.run(ctx)
	}()
}

var errWriterStopped = errors.New("fdb: shutting down")

// submit queues a write and waits for its outcome. When ctx ends first the
// write may still be committed, as with any request whose reply is lost.
func (w *writer) submit(ctx context.Context, op, key string, size int, decide decideFunc) (writeResult, error) {
	o := &writeOp{ctx: ctx, op: op, key: key, size: size, decide: decide, start: time.Now(), done: make(chan writeOutcome, 1)}
	select {
	case w.queue <- o:
	case <-ctx.Done():
		return writeResult{}, ctx.Err()
	case <-w.stopped:
		return writeResult{}, errWriterStopped
	}
	var out writeOutcome
	select {
	case out = <-o.done:
	case <-ctx.Done():
		out.err = ctx.Err()
	case <-w.stopped:
		select {
		case out = <-o.done:
		default:
			out.err = errWriterStopped
		}
	}
	w.s.metrics.observeTxn(op, o.start, out.err)
	return out.res, out.err
}

func (w *writer) run(ctx context.Context) {
	defer w.stop()
	for {
		var first *writeOp
		select {
		case first = <-w.queue:
		case <-ctx.Done():
			return
		}
		batch := []*writeOp{first}
		bytes := first.size
	fill:
		for len(batch) < maxBatchOps && bytes < maxBatchBytes {
			select {
			case o := <-w.queue:
				batch = append(batch, o)
				bytes += o.size
			default:
				break fill
			}
		}
		w.commit(ctx, batch)
	}
}

// stop fails everything still queued once the committer exits.
func (w *writer) stop() {
	w.stopOnce.Do(func() { close(w.stopped) })
	for {
		select {
		case o := <-w.queue:
			o.done <- writeOutcome{err: errWriterStopped}
		default:
			return
		}
	}
}

// commit applies a batch in one transaction and reports each write's
// outcome. Writes whose callers have already given up are skipped.
func (w *writer) commit(ctx context.Context, batch []*writeOp) {
	live := batch[:0]
	for _, o := range batch {
		if err := o.ctx.Err(); err != nil {
			o.done <- writeOutcome{err: err}
			continue
		}
		live = append(live, o)
	}
	if len(live) == 0 {
		return
	}

	_, err := w.s.run(ctx, txnOptions{op: "write", write: true}, func(tr fdb.Transaction, at *attempt) (any, error) {
		return nil, w.apply(tr, at, live)
	})
	var newest int64
	for _, o := range live {
		out := o.outcome
		if err != nil {
			out = writeOutcome{err: err}
		}
		if out.err == nil && out.res.ok {
			newest = max(newest, out.res.rev)
		}
		o.done <- out
	}
	if newest > 0 {
		w.committed(newest)
	}
	if err != nil && ctx.Err() == nil {
		logrus.Debugf("fdb: write batch of %d failed: %v", len(live), err)
	}
}

// apply runs one attempt of a batch: it reads the state of every key
// together, then decides and applies the writes in order, each at the next
// revision.
func (w *writer) apply(tr fdb.Transaction, at *attempt, batch []*writeOp) error {
	for _, o := range batch {
		if at.maybeCommitted && o.assigned != 0 {
			o.candidates = append(o.candidates, o.assigned)
		}
		o.assigned = 0
		o.outcome = writeOutcome{}
	}

	cur := w.s.readCurrent(tr)
	type reads struct {
		latest     fdb.RangeResult
		candidates []fdb.FutureByteSlice
	}
	pending := make([]reads, len(batch))
	for i, o := range batch {
		pending[i].latest = w.s.floor(tr, o.key, latestRevision)
		for _, rev := range o.candidates {
			pending[i].candidates = append(pending[i].candidates, w.s.exact(tr, o.key, rev))
		}
	}
	next, err := cur.get()
	if err != nil {
		return err
	}

	// Writes earlier in the batch are visible to later writes of the same key.
	written := map[string]*version{}
	for i, o := range batch {
		st := writeState{current: next}
		if v, ok := written[o.key]; ok {
			st.latest = v
		} else if st.latest, err = w.s.decodeFloor(pending[i].latest); err != nil {
			if isFDBError(err) {
				return err
			}
			o.outcome = writeOutcome{err: err}
			continue
		}
		for j, f := range pending[i].candidates {
			v, err := w.s.decodeExact(o.key, o.candidates[j], f)
			if err != nil {
				return err
			}
			if v != nil {
				st.earlier = append(st.earlier, v)
			}
		}

		res, m, err := o.decide(tr, st, next+1)
		if isFDBError(err) {
			return err
		}
		o.outcome = writeOutcome{res: res, err: err}
		if err != nil || m == nil {
			continue
		}
		w.s.apply(tr, *m)
		next = m.rev
		o.assigned = m.rev
		written[o.key] = &version{key: o.key, rev: m.rev, entry: m.entry}
	}
	return nil
}

func isFDBError(err error) bool {
	var fe fdb.Error
	return errors.As(err, &fe)
}
