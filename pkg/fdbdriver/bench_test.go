// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Benchmarks for the common Kubernetes access patterns: creates, compare-and-
// swap updates under contention and without it, point reads, and lists.

package fdbdriver

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

var benchValue = make([]byte, 2048) // a typical small Kubernetes object

func BenchmarkCreate(b *testing.B) {
	be := newTestBackend(b, testOptions{})
	ctx := testCtx(b)
	var n atomic.Int64
	b.SetBytes(int64(len(benchValue)))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := be.Create(ctx, fmt.Sprintf("/bench/create/%d", n.Add(1)), benchValue, 0); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkUpdateDisjoint updates many different keys concurrently, like
// kubelets renewing their node leases.
func BenchmarkUpdateDisjoint(b *testing.B) {
	be := newTestBackend(b, testOptions{})
	ctx := testCtx(b)
	const keys = 256
	revs := make([]atomic.Int64, keys)
	for i := range keys {
		revs[i].Store(mustCreate(b, be, fmt.Sprintf("/bench/lease/%d", i), "v"))
	}
	var n atomic.Int64
	b.SetBytes(int64(len(benchValue)))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := int(n.Add(1) % keys)
			for {
				rev, kv, ok, err := be.Update(ctx, fmt.Sprintf("/bench/lease/%d", i), benchValue, revs[i].Load(), 0)
				if err != nil {
					b.Fatal(err)
				}
				if ok {
					revs[i].Store(rev)
					break
				}
				revs[i].Store(kv.ModRevision)
			}
		}
	})
}

func BenchmarkGet(b *testing.B) {
	be := newTestBackend(b, testOptions{})
	ctx := testCtx(b)
	for i := range 100 {
		if _, err := be.Create(ctx, fmt.Sprintf("/bench/get/%d", i), benchValue, 0); err != nil {
			b.Fatal(err)
		}
	}
	var n atomic.Int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, kv, err := be.Get(ctx, fmt.Sprintf("/bench/get/%d", n.Add(1)%100), 0, false); err != nil || kv == nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkList500 lists one page of 500 objects, the apiserver's default.
func BenchmarkList500(b *testing.B) {
	be := newTestBackend(b, testOptions{})
	ctx := testCtx(b)
	for i := range 2000 {
		if _, err := be.Create(ctx, fmt.Sprintf("/bench/list/%05d", i), benchValue, 0); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, kvs, err := be.List(ctx, "/bench/list/", "/bench/list0", 501, 0, false); err != nil || len(kvs) != 501 {
				b.Fatalf("list returned %d items: %v", len(kvs), err)
			}
		}
	})
}

func BenchmarkCount2000(b *testing.B) {
	be := newTestBackend(b, testOptions{})
	ctx := testCtx(b)
	for i := range 2000 {
		if _, err := be.Create(ctx, fmt.Sprintf("/bench/count/%05d", i), benchValue, 0); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for b.Loop() {
		if _, n, err := be.Count(ctx, "/bench/count/", "/bench/count0", 0); err != nil || n != 2000 {
			b.Fatalf("count = %d: %v", n, err)
		}
	}
}

// BenchmarkWatchDelivery measures the delay between a write committing and
// its event reaching a watcher, with the fallback poll disabled so only the
// FoundationDB watch on the revision counter wakes the poller.
func BenchmarkWatchDelivery(b *testing.B) {
	be := newTestBackend(b, testOptions{pollInterval: time.Hour})
	writer := newTestBackend(b, testOptions{namespace: be.namespace, pollInterval: time.Hour})
	ctx := testCtx(b)
	wr := be.Watch(ctx, 0)
	<-wr.Eventc
	var total time.Duration
	b.ResetTimer()
	for i := range b.N {
		key := fmt.Sprintf("/bench/watch/%d", i)
		rev, err := writer.Create(ctx, key, benchValue, 0)
		if err != nil {
			b.Fatal(err)
		}
		committed := time.Now()
		for found := false; !found; {
			batch := <-wr.Eventc
			for _, ev := range batch.Events {
				found = found || ev.KV.ModRevision == rev
			}
		}
		total += time.Since(committed)
	}
	b.ReportMetric(float64(total.Microseconds())/float64(b.N)/1000, "ms/delivery")
}
