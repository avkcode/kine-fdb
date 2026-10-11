// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Shared helpers for tests that run against a real FoundationDB cluster,
// selected with KINE_FDB_TEST_CLUSTER_FILE.

package fdbdriver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/k3s-io/kine/pkg/drivers"
	"github.com/k3s-io/kine/pkg/server"
	"github.com/sirupsen/logrus"
)

func TestMain(m *testing.M) {
	// kine and the driver log at info level on every start and stop.
	if os.Getenv("KINE_FDB_TEST_LOG") == "" {
		logrus.SetLevel(logrus.WarnLevel)
	}
	os.Exit(m.Run())
}

// testClusterFile returns the cluster file for integration tests, skipping
// the test when none is configured.
func testClusterFile(t testing.TB) string {
	t.Helper()
	cf := os.Getenv("KINE_FDB_TEST_CLUSTER_FILE")
	if cf == "" {
		t.Skip("KINE_FDB_TEST_CLUSTER_FILE is not set; skipping FoundationDB integration test")
	}
	return cf
}

var nonNameChars = regexp.MustCompile(`[^a-z0-9-]+`)

// testNamespace returns a fresh namespace name for t.
func testNamespace(t testing.TB) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	name := nonNameChars.ReplaceAllString(strings.ToLower(t.Name()), "-")
	if len(name) > 40 {
		name = name[:40]
	}
	return fmt.Sprintf("/test/%s-%s/", name, hex.EncodeToString(b[:]))
}

type testOptions struct {
	namespace       string
	compactInterval time.Duration
	minRetain       int64
	pollInterval    time.Duration
	noStart         bool
}

type testBackend struct {
	*backend
	ctx       context.Context
	cancel    context.CancelFunc
	wg        *sync.WaitGroup
	endpoint  string
	namespace string
}

func testEndpoint(t testing.TB, namespace string, poll time.Duration) string {
	q := url.Values{}
	q.Set("namespace", namespace)
	if poll > 0 {
		q.Set("poll_interval", poll.String())
	}
	q.Set("timeout", "10s")
	return "fdb://" + testClusterFile(t) + "?" + q.Encode()
}

// newTestBackend starts a backend on a fresh namespace and removes the
// namespace when the test ends.
func newTestBackend(t testing.TB, opts testOptions) *testBackend {
	t.Helper()
	ns := opts.namespace
	if ns == "" {
		ns = testNamespace(t)
	}
	endpoint := testEndpoint(t, ns, opts.pollInterval)
	ctx, cancel := context.WithCancel(context.Background())
	wg := &sync.WaitGroup{}
	_, be, err := New(ctx, wg, &drivers.Config{
		Endpoint:         endpoint,
		CompactInterval:  opts.compactInterval,
		CompactMinRetain: opts.minRetain,
		CompactBatchSize: 1000,
		CompactTimeout:   5 * time.Second,
		PollBatchSize:    500,
	})
	if err != nil {
		cancel()
		t.Fatalf("New: %v", err)
	}
	tb := &testBackend{backend: be.(*backend), ctx: ctx, cancel: cancel, wg: wg, endpoint: endpoint, namespace: ns}
	t.Cleanup(func() {
		tb.stop()
		if opts.namespace == "" {
			deleteNamespace(t, endpoint)
		}
	})
	if !opts.noStart {
		if err := tb.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}
	return tb
}

func (tb *testBackend) stop() {
	tb.cancel()
	tb.wg.Wait()
}

func deleteNamespace(t testing.TB, endpoint string) {
	t.Helper()
	a, err := OpenAdmin(endpoint)
	if err != nil {
		t.Errorf("OpenAdmin: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.Delete(ctx, ""); err != nil && !errors.Is(err, ErrNamespaceNotFound) {
		t.Errorf("delete namespace: %v", err)
	}
}

func testCtx(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// mustCreate creates key and returns its revision.
func mustCreate(t testing.TB, b server.Backend, key, value string) int64 {
	t.Helper()
	rev, err := b.Create(testCtx(t), key, []byte(value), 0)
	if err != nil {
		t.Fatalf("Create(%s): %v", key, err)
	}
	return rev
}

// mustUpdate updates key at revision and returns the new revision.
func mustUpdate(t testing.TB, b server.Backend, key, value string, revision int64) int64 {
	t.Helper()
	rev, _, ok, err := b.Update(testCtx(t), key, []byte(value), revision, 0)
	if err != nil || !ok {
		t.Fatalf("Update(%s@%d) = %d, ok=%v, err=%v", key, revision, rev, ok, err)
	}
	return rev
}

// mustDelete deletes key at revision and returns the new revision.
func mustDelete(t testing.TB, b server.Backend, key string, revision int64) int64 {
	t.Helper()
	rev, _, ok, err := b.Delete(testCtx(t), key, revision)
	if err != nil || !ok {
		t.Fatalf("Delete(%s@%d) = %d, ok=%v, err=%v", key, revision, rev, ok, err)
	}
	return rev
}

func keysOf(kvs []*server.KeyValue) []string {
	out := make([]string, 0, len(kvs))
	for _, kv := range kvs {
		out = append(out, kv.Key)
	}
	return out
}

func mustVerify(t testing.TB, b *testBackend) {
	t.Helper()
	rep, err := b.verify(testCtx(t))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("verify found %d problem(s):\n%s", len(rep.Problems), strings.Join(rep.Problems, "\n"))
	}
}
