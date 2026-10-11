// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Watch support: a single poller per backend follows the revision change log,
// woken by a FoundationDB watch on the revision counter, and fans events out
// to subscribers. Subscribers that start in the past or fall behind read the
// log directly until they catch up, so no subscriber is dropped for being slow.

package fdbdriver

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/k3s-io/kine/pkg/server"
	"github.com/sirupsen/logrus"

	"github.com/avkcode/kine-fdb/internal/record"
)

const (
	// subscriberBuffer is the number of batches queued per subscriber before
	// it is switched to reading the log directly.
	subscriberBuffer = 128
	// eventPageBytes bounds the values read by one event transaction.
	eventPageBytes = 8 << 20
	// eventResolveBatch is how many log entries are resolved together while
	// filling a page, so the byte budget is checked as values arrive.
	eventResolveBatch = 64
)

// errEventsLost means the requested events were removed by compaction
// before they could be read.
var errEventsLost = errors.New("fdb: events were compacted before they could be delivered")

// eventPage is a run of consecutive change-log events.
type eventPage struct {
	events []*server.Event
	// last is the newest revision covered: from-1 when nothing was read.
	last    int64
	current int64
	// watch, when set, fires once the revision counter changes.
	watch fdb.FutureNil
}

// readEvents reads the events with revisions in [from, to] (to is capped at
// the current revision), at most limit of them. With arm, and when the page
// reaches the current revision, it registers a FoundationDB watch on the
// revision counter in the same transaction so that no later write is missed.
func (s *store) readEvents(ctx context.Context, from, to, limit int64, arm bool) (eventPage, error) {
	ret, err := s.run(ctx, txnOptions{op: "watch", write: arm}, func(tr fdb.Transaction, _ *attempt) (any, error) {
		revs, err := s.readRevisions(tr).get()
		if err != nil {
			return nil, err
		}
		page := eventPage{last: from - 1, current: revs.current}
		if from < revs.compacted {
			return page, errEventsLost
		}
		if upper := min(to, revs.current); from <= upper {
			entries, err := tr.GetRange(s.ks.logRange(from, upper+1), fdb.RangeOptions{Limit: int(limit), Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
			if err != nil {
				return nil, err
			}
			if len(entries) == 0 {
				return nil, fmt.Errorf("fdb: change log has no entries for revisions %d..%d", from, upper)
			}
			all, err := s.resolveEvents(tr, entries, revs.compact, &page)
			if err != nil {
				return nil, err
			}
			if all && len(entries) < int(limit) {
				page.last = upper
			}
		}
		if arm && page.last >= revs.current {
			page.watch = tr.Watch(s.ks.metaRev)
		}
		return page, nil
	})
	page, _ := ret.(eventPage)
	return page, err
}

// resolveEvents turns change-log entries into events, stopping early once
// the page holds eventPageBytes of values. It reports whether every entry
// was used.
func (s *store) resolveEvents(tr fdb.Transaction, entries []fdb.KeyValue, compact int64, page *eventPage) (bool, error) {
	type item struct {
		rev     int64
		log     record.LogEntry
		cur     fdb.FutureByteSlice
		prev    fdb.FutureByteSlice
		curV    *version
		prevV   *version
		curVal  valueFuture
		prevVal valueFuture
	}
	bytes := 0
	for start := 0; start < len(entries); start += eventResolveBatch {
		chunk := entries[start:min(start+eventResolveBatch, len(entries))]
		items := make([]item, len(chunk))
		for i, e := range chunk {
			rev, err := s.ks.logRevision(e.Key)
			if err != nil {
				return false, err
			}
			l, err := record.DecodeLog(e.Value)
			if err != nil {
				return false, fmt.Errorf("fdb: decode change log at %d: %w", rev, err)
			}
			it := &items[i]
			it.rev, it.log = rev, l
			key := string(l.Key)
			if !l.Deleted {
				it.cur = s.exact(tr, key, rev)
			}
			// etcd reports no previous value for creates, nor for events at
			// or below the compact revision.
			if l.PrevRevision > 0 && !l.Create && rev > compact {
				it.prev = s.exact(tr, key, l.PrevRevision)
			}
		}
		for i := range items {
			it := &items[i]
			key := string(it.log.Key)
			if it.cur != nil {
				v, err := s.decodeExact(key, it.rev, it.cur)
				if err != nil {
					return false, err
				}
				if v == nil {
					return false, fmt.Errorf("fdb: change log names %q@%d, which has no history entry", key, it.rev)
				}
				it.curV, it.curVal = v, s.loadValue(tr, v)
			}
			if it.prev != nil {
				v, err := s.decodeExact(key, it.log.PrevRevision, it.prev)
				if err != nil {
					return false, err
				}
				if v != nil && !v.entry.Deleted {
					it.prevV, it.prevVal = v, s.loadValue(tr, v)
				}
			}
		}
		for i := range items {
			it := &items[i]
			ev := &server.Event{Create: it.log.Create, Delete: it.log.Deleted}
			if it.curV != nil {
				value, err := it.curVal.get()
				if err != nil {
					return false, err
				}
				ev.KV = it.curV.keyValue(value)
				bytes += len(value)
			} else {
				ev.KV = &server.KeyValue{Key: string(it.log.Key), ModRevision: it.rev}
			}
			if it.prevV != nil {
				value, err := it.prevVal.get()
				if err != nil {
					return false, err
				}
				ev.PrevKV = it.prevV.keyValue(value)
				bytes += len(value)
			}
			page.events = append(page.events, ev)
			page.last = it.rev
		}
		if bytes >= eventPageBytes {
			return start+len(chunk) == len(entries), nil
		}
	}
	return true, nil
}

// hub follows the change log and fans events out to subscribers.
type hub struct {
	b    *backend
	wake chan struct{}

	mu      sync.Mutex
	subs    map[*subscriber]struct{}
	polled  int64 // newest revision delivered to live subscribers
	stopped bool
}

// subscriber is one Watch call. All fields are guarded by hub.mu.
type subscriber struct {
	// next is the first revision the subscriber has not been given yet.
	next int64
	// catchUp means the hub no longer queues batches for the subscriber;
	// its goroutine reads the log directly from next until it is current.
	catchUp bool
	failed  error
	closed  bool
	live    chan server.EventBatch
	kick    chan struct{}
}

func (s *subscriber) poke() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func newHub(b *backend) *hub {
	return &hub{b: b, wake: make(chan struct{}, 1), subs: make(map[*subscriber]struct{})}
}

// poke wakes the poller, for example after a local write.
func (h *hub) poke() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (h *hub) start(ctx context.Context, wg *sync.WaitGroup) error {
	ret, err := h.b.read(ctx, "watch-start", func(tr fdb.Transaction) (any, error) {
		r, err := h.b.readRevisions(tr).get()
		return r.current, err
	})
	if err != nil {
		return err
	}
	h.polled = ret.(int64)
	h.b.observeRevision(h.polled)
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.loop(ctx)
	}()
	return nil
}

func (h *hub) loop(ctx context.Context) {
	defer h.shutdown()
	ticker := time.NewTicker(h.b.cfg.PollInterval)
	defer ticker.Stop()

	var (
		armed     fdb.FutureNil
		fired     chan error
		failures  int
		lastError time.Time
	)
	defer func() {
		if armed != nil {
			armed.Cancel()
		}
	}()

	for {
		h.mu.Lock()
		from := h.polled + 1
		h.mu.Unlock()

		page, err := h.b.readEvents(ctx, from, math.MaxInt64, h.b.cfg.PollBatchSize, true)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errEventsLost) {
			logrus.Errorf("fdb: watch poller fell behind compaction at revision %d; cancelling all watches", from)
			h.failAll(server.ErrCompacted, page.current)
			continue
		}
		if err != nil {
			failures++
			if time.Since(lastError) > 30*time.Second {
				logrus.Errorf("fdb: watch poll failed (%d consecutive failures): %v", failures, err)
				lastError = time.Now()
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(min(time.Duration(failures)*200*time.Millisecond, 5*time.Second)):
			}
			continue
		}
		failures = 0
		h.b.observeRevision(page.current)
		h.b.metrics.currentRevision(page.current)
		if len(page.events) > 0 {
			h.publish(page.events, page.last)
		}
		if page.last < page.current {
			continue
		}

		if page.watch != nil {
			if armed != nil {
				armed.Cancel()
			}
			armed = page.watch
			fired = make(chan error, 1)
			go func(f fdb.FutureNil, done chan<- error) { done <- f.Get() }(armed, fired)
		}
		select {
		case <-ctx.Done():
			return
		case <-h.wake:
		case <-ticker.C:
		case err := <-fired:
			armed, fired = nil, nil
			if err != nil {
				// A failing watch must not turn into a busy loop.
				select {
				case <-ctx.Done():
					return
				case <-h.wake:
				case <-ticker.C:
				}
			}
		}
	}
}

// publish hands a page of events to every live subscriber.
func (h *hub) publish(events []*server.Event, last int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if s.catchUp || s.failed != nil || s.closed {
			continue
		}
		evs := eventsFrom(events, s.next)
		if len(evs) == 0 {
			continue
		}
		select {
		case s.live <- server.EventBatch{Events: evs, CurrentRev: last}:
			s.next = last + 1
		default:
			s.catchUp = true
			s.poke()
		}
	}
	h.polled = last
}

// failAll ends every subscription with err and skips ahead to current.
func (h *hub) failAll(err error, current int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		s.failed = err
		s.poke()
	}
	if current > h.polled {
		h.polled = current
	}
}

func (h *hub) shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = true
	for s := range h.subs {
		s.closed = true
		s.poke()
	}
}

func (h *hub) remove(s *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		h.b.metrics.watchers(len(h.subs))
	}
}

// eventsFrom returns the events with revision >= rev, as a slice with no
// spare capacity: kine appends to the slices it receives.
func eventsFrom(events []*server.Event, rev int64) []*server.Event {
	i := 0
	for i < len(events) && events[i].KV.ModRevision < rev {
		i++
	}
	return events[i:len(events):len(events)]
}

// Watch streams every change with revision >= revision (zero means changes
// after the current revision). kine filters events by key itself.
func (b *backend) Watch(ctx context.Context, revision int64) server.WatchResult {
	return b.hub.watch(ctx, revision)
}

func (h *hub) watch(ctx context.Context, revision int64) server.WatchResult {
	eventc := make(chan server.EventBatch, 16)
	errc := make(chan error, 1)

	ret, err := h.b.read(ctx, "watch-open", func(tr fdb.Transaction) (any, error) {
		return h.b.readRevisions(tr).get()
	})
	if err != nil {
		if ctx.Err() == nil {
			errc <- err
		}
		close(eventc)
		return server.WatchResult{Eventc: eventc, Errorc: errc}
	}
	revs := ret.(revisions)
	if revision > 0 && revision < revs.compact {
		close(eventc)
		return server.WatchResult{CurrentRevision: revs.current, CompactRevision: revs.compact, Eventc: eventc, Errorc: errc}
	}

	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		close(eventc)
		return server.WatchResult{CurrentRevision: revs.current, Eventc: eventc, Errorc: errc}
	}
	snapshot := h.polled
	start := revision
	if start <= 0 {
		start = snapshot + 1
	}
	s := &subscriber{
		next:    start,
		catchUp: start <= snapshot,
		live:    make(chan server.EventBatch, subscriberBuffer),
		kick:    make(chan struct{}, 1),
	}
	h.subs[s] = struct{}{}
	h.b.metrics.watchers(len(h.subs))
	h.mu.Unlock()

	go h.serve(ctx, s, snapshot, eventc, errc)
	return server.WatchResult{CurrentRevision: snapshot, Eventc: eventc, Errorc: errc}
}

// serve runs one subscription: it forwards batches queued by the hub and,
// whenever the subscriber is behind, reads the change log directly.
func (h *hub) serve(ctx context.Context, s *subscriber, snapshot int64, eventc chan<- server.EventBatch, errc chan<- error) {
	defer close(eventc)
	defer h.remove(s)

	send := func(batch server.EventBatch) bool {
		select {
		case eventc <- batch:
			return true
		case <-ctx.Done():
			return false
		}
	}

	h.mu.Lock()
	initialCatchUp := s.catchUp
	h.mu.Unlock()
	if !initialCatchUp {
		// An empty first batch at the snapshot revision lets kine treat the
		// watch as synced right away, as with kine's other backends.
		if !send(server.EventBatch{CurrentRev: snapshot}) {
			return
		}
	}

	for {
		select {
		case batch := <-s.live:
			if !send(batch) {
				return
			}
			continue
		default:
		}

		h.mu.Lock()
		failed, closed := s.failed, s.closed
		from, target := s.next, h.polled
		if s.catchUp && from > target {
			s.catchUp = false
		}
		catchUp := s.catchUp
		h.mu.Unlock()

		switch {
		case failed != nil:
			errc <- failed
			return
		case closed:
			return
		case !catchUp:
			select {
			case batch := <-s.live:
				if !send(batch) {
					return
				}
			case <-s.kick:
			case <-ctx.Done():
				return
			}
			continue
		}

		page, err := h.b.readEvents(ctx, from, target, h.b.cfg.PollBatchSize, false)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errEventsLost) {
			errc <- server.ErrCompacted
			return
		}
		if err != nil {
			logrus.Warnf("fdb: watch catch-up from revision %d failed, retrying: %v", from, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		if len(page.events) > 0 {
			evs := page.events[:len(page.events):len(page.events)]
			if !send(server.EventBatch{Events: evs, CurrentRev: page.last}) {
				return
			}
		}
		h.mu.Lock()
		if page.last >= s.next {
			s.next = page.last + 1
		}
		h.mu.Unlock()
	}
}
