// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Integration tests for Watch: event contents, starting in the past or at
// the present, behaviour around compaction, and slow consumers.

package fdbdriver

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/k3s-io/kine/pkg/server"
)

// collect reads events from wr until n have arrived or the timeout passes.
// It also checks the batch invariants kine relies on: CurrentRev increases
// strictly across batches that carry events, and covers their events.
func collect(t testing.TB, wr server.WatchResult, n int, timeout time.Duration) []*server.Event {
	t.Helper()
	var events []*server.Event
	var lastRev int64
	deadline := time.After(timeout)
	for len(events) < n {
		select {
		case batch, ok := <-wr.Eventc:
			if !ok {
				select {
				case err := <-wr.Errorc:
					t.Fatalf("watch closed after %d events: %v", len(events), err)
				default:
					t.Fatalf("watch closed after %d events", len(events))
				}
			}
			if len(batch.Events) > 0 {
				if batch.CurrentRev <= lastRev {
					t.Fatalf("batch revision %d does not advance past %d", batch.CurrentRev, lastRev)
				}
				if last := batch.Events[len(batch.Events)-1].KV.ModRevision; last > batch.CurrentRev {
					t.Fatalf("batch revision %d is below its last event %d", batch.CurrentRev, last)
				}
				lastRev = batch.CurrentRev
			}
			events = append(events, batch.Events...)
		case <-deadline:
			t.Fatalf("timed out after %d of %d events", len(events), n)
		}
	}
	return events
}

func TestWatchEvents(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx, cancel := context.WithCancel(testCtx(t))
	defer cancel()

	start, _ := b.CurrentRevision(ctx)
	wr := b.Watch(ctx, 0)
	if wr.CompactRevision != 0 {
		t.Fatalf("CompactRevision = %d on a healthy watch", wr.CompactRevision)
	}
	r1 := mustCreate(t, b, "/w/k", "v1")
	r2 := mustUpdate(t, b, "/w/k", "v2", r1)
	r3 := mustDelete(t, b, "/w/k", r2)

	events := collect(t, wr, 3, 10*time.Second)
	create, update, del := events[0], events[1], events[2]
	if !create.Create || create.Delete || create.KV.ModRevision != r1 || create.KV.CreateRevision != r1 || string(create.KV.Value) != "v1" || create.PrevKV != nil {
		t.Fatalf("create event = %+v kv=%+v", create, create.KV)
	}
	if update.Create || update.Delete || update.KV.ModRevision != r2 || update.KV.CreateRevision != r1 || update.KV.Version != 2 ||
		string(update.KV.Value) != "v2" || update.PrevKV == nil || string(update.PrevKV.Value) != "v1" || update.PrevKV.ModRevision != r1 {
		t.Fatalf("update event = %+v kv=%+v prev=%+v", update, update.KV, update.PrevKV)
	}
	if !del.Delete || del.KV.ModRevision != r3 || del.KV.Key != "/w/k" || del.PrevKV == nil || string(del.PrevKV.Value) != "v2" {
		t.Fatalf("delete event = %+v kv=%+v prev=%+v", del, del.KV, del.PrevKV)
	}
	if r1 != start+1 {
		t.Fatalf("revisions are not dense: start %d, first write %d", start, r1)
	}
}

// A watch from revision 0 first receives an empty batch at the current
// revision, so kine considers it synced and can send progress notifications.
func TestWatchFromNowStartsSynced(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx, cancel := context.WithCancel(testCtx(t))
	defer cancel()
	cur, _ := b.CurrentRevision(ctx)
	wr := b.Watch(ctx, 0)
	select {
	case batch := <-wr.Eventc:
		if len(batch.Events) != 0 || batch.CurrentRev != cur || wr.CurrentRevision != cur {
			t.Fatalf("first batch = %v, watch revision %d, want empty batch at %d", batch, wr.CurrentRevision, cur)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no initial batch")
	}
}

func TestWatchFromPastRevision(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx, cancel := context.WithCancel(testCtx(t))
	defer cancel()
	r1 := mustCreate(t, b, "/p/a", "1")
	mustCreate(t, b, "/p/b", "2")
	r3 := mustUpdate(t, b, "/p/a", "3", r1)

	wr := b.Watch(ctx, r1)
	events := collect(t, wr, 3, 10*time.Second)
	if events[0].KV.ModRevision != r1 || events[2].KV.ModRevision != r3 || string(events[2].PrevKV.Value) != "1" {
		t.Fatalf("replayed events: %v", events)
	}
	// Live events follow the replay without gaps or duplicates.
	r4 := mustCreate(t, b, "/p/c", "4")
	if ev := collect(t, wr, 1, 10*time.Second); ev[0].KV.ModRevision != r4 {
		t.Fatalf("live event after replay = %v", ev)
	}
}

// The lab driver reported the namespace's compact revision on every watch.
// kine cancels any watch whose result carries a compact revision, so after
// the first compaction every new watch, including kine's TTL watcher, died.
func TestWatchAfterCompaction(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx, cancel := context.WithCancel(testCtx(t))
	defer cancel()
	r1 := mustCreate(t, b, "/c/k", "1")
	r2 := mustUpdate(t, b, "/c/k", "2", r1)
	r3 := mustUpdate(t, b, "/c/k", "3", r2)
	if _, err := b.Compact(ctx, r2); err != nil {
		t.Fatal(err)
	}

	for _, rev := range []int64{0, r2, r3} {
		wr := b.Watch(ctx, rev)
		if wr.CompactRevision != 0 {
			t.Fatalf("watch from %d after compaction to %d reported CompactRevision %d", rev, r2, wr.CompactRevision)
		}
	}
	// Watching from the compact revision itself replays from there; the
	// event at the compact revision has no previous value, as in etcd.
	events := collect(t, b.Watch(ctx, r2), 2, 10*time.Second)
	if events[0].KV.ModRevision != r2 || events[0].PrevKV != nil || events[1].KV.ModRevision != r3 || string(events[1].PrevKV.Value) != "2" {
		t.Fatalf("events from compact revision: %+v / %+v", events[0], events[1])
	}

	// Below the compact revision the watch is refused with the revisions.
	wr := b.Watch(ctx, r1)
	cur, _ := b.CurrentRevision(ctx)
	if wr.CompactRevision != r2 || wr.CurrentRevision != cur {
		t.Fatalf("compacted watch = compact %d current %d, want %d and %d", wr.CompactRevision, wr.CurrentRevision, r2, cur)
	}
}

// A subscriber that stops reading is switched to reading the change log
// directly and still receives every event once it resumes.
func TestSlowWatcherCatchesUp(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx, cancel := context.WithCancel(testCtx(t))
	defer cancel()
	wr := b.Watch(ctx, 0)
	<-wr.Eventc // initial batch

	const n = 600
	first := int64(0)
	for i := range n {
		rev := mustCreate(t, b, fmt.Sprintf("/slow/%04d", i), "v")
		if i == 0 {
			first = rev
		}
		if i%50 == 0 {
			// Let the poller publish many small batches while nobody reads.
			time.Sleep(20 * time.Millisecond)
		}
	}
	events := collect(t, wr, n, 30*time.Second)
	for i, ev := range events {
		if ev.KV.ModRevision != first+int64(i) {
			t.Fatalf("event %d has revision %d, want %d", i, ev.KV.ModRevision, first+int64(i))
		}
	}
}

func TestWatchFutureRevision(t *testing.T) {
	b := newTestBackend(t, testOptions{})
	ctx, cancel := context.WithCancel(testCtx(t))
	defer cancel()
	cur, _ := b.CurrentRevision(ctx)
	wr := b.Watch(ctx, cur+3)
	mustCreate(t, b, "/f/1", "v")
	mustCreate(t, b, "/f/2", "v")
	r3 := mustCreate(t, b, "/f/3", "v")
	for {
		select {
		case batch := <-wr.Eventc:
			for _, ev := range batch.Events {
				if ev.KV.ModRevision < cur+3 {
					t.Fatalf("received event %d before the requested start %d", ev.KV.ModRevision, cur+3)
				}
				if ev.KV.ModRevision == r3 {
					return
				}
			}
		case <-time.After(10 * time.Second):
			t.Fatal("event at the requested revision never arrived")
		}
	}
}

// Writes made through another backend on the same namespace reach this
// backend's watchers through the FoundationDB watch, without polling delay.
func TestWatchSeesWritesFromOtherServers(t *testing.T) {
	a := newTestBackend(t, testOptions{pollInterval: time.Hour})
	other := newTestBackend(t, testOptions{namespace: a.namespace, pollInterval: time.Hour})
	ctx, cancel := context.WithCancel(testCtx(t))
	defer cancel()
	wr := a.Watch(ctx, 0)
	<-wr.Eventc
	time.Sleep(200 * time.Millisecond)
	rev := mustCreate(t, other, "/remote", "v")
	ev := collect(t, wr, 1, 5*time.Second)
	if ev[0].KV.ModRevision != rev {
		t.Fatalf("got %v", ev[0])
	}
}
