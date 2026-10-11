// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Drop-in replacement for the Kubernetes apiserver's embedded etcd test
// server that runs kine with the FoundationDB driver, giving every test its
// own namespace. It mirrors github.com/k3s-io/kine/pkg/testserver.

// Package testserver lets the k8s.io/apiserver etcd3 storage tests run
// against kine-fdb. Point it at a cluster with KINE_FDB_TEST_CLUSTER_FILE.
package testserver

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/k3s-io/kine/pkg/app"
	"github.com/k3s-io/kine/pkg/endpoint"
	"github.com/sirupsen/logrus"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/kubernetes"
	"go.etcd.io/etcd/server/v3/embed"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	storagetesting "k8s.io/apiserver/pkg/storage/testing"

	"github.com/avkcode/kine-fdb/pkg/fdbdriver"
)

// NewTestConfig returns a configuration listening on a private unix socket.
func NewTestConfig(t testing.TB) *embed.Config {
	cfg := embed.NewConfig()
	cfg.Dir = t.TempDir()
	cfg.ListenClientUrls = []url.URL{{Scheme: "unix", Path: cfg.Dir + "/kine.sock"}}
	cfg.WatchProgressNotifyInterval = 5 * time.Second
	_ = os.Chmod(cfg.Dir, 0o700)
	return cfg
}

// RunEtcd starts kine-fdb on a fresh namespace and returns a client for it.
// The namespace is deleted when the test ends.
func RunEtcd(t testing.TB, cfg *embed.Config) *kubernetes.Client {
	t.Helper()
	level := logrus.WarnLevel
	if v := os.Getenv("KINE_FDB_TEST_LOG_LEVEL"); v != "" {
		if l, err := logrus.ParseLevel(v); err == nil {
			level = l
		}
	}
	logrus.SetLevel(level)
	logrus.SetOutput(t.Output())

	if cfg == nil {
		cfg = NewTestConfig(t)
	}
	clusterFile := os.Getenv("KINE_FDB_TEST_CLUSTER_FILE")
	if clusterFile == "" {
		t.Fatal("KINE_FDB_TEST_CLUSTER_FILE must name the FoundationDB cluster file to test against")
	}
	q := url.Values{}
	q.Set("namespace", fmt.Sprintf("/apiserver-tests/%.8x/", sha256.Sum256([]byte(cfg.Dir))))
	q.Set("poll_interval", "100ms")
	ep := "fdb://" + clusterFile + "?" + q.Encode()

	ctx, cancel := context.WithCancel(t.Context())
	wg := &sync.WaitGroup{}
	t.Cleanup(func() {
		cancel()
		wg.Wait()
		deleteNamespace(t, ep)
	})

	config := app.Config(nil)
	config.WaitGroup = wg
	config.Listener = cfg.ListenClientUrls[0].String()
	config.NotifyInterval = cfg.WatchProgressNotifyInterval
	config.CompactInterval = 0
	config.CompactMinRetain = 0
	config.Endpoint = ep

	e, err := endpoint.Listen(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig, err := e.TLSConfig.ClientConfig()
	if err != nil {
		t.Fatal(err)
	}

	client, err := kubernetes.New(clientv3.Config{
		TLS:         tlsConfig,
		Endpoints:   e.Endpoints,
		DialTimeout: 10 * time.Second,
		DialOptions: []grpc.DialOption{grpc.WithBlock()},
		Logger:      zaptest.NewLogger(t, zaptest.Level(zapcore.ErrorLevel)).Named("etcd-client"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	kubernetesRecorder := storagetesting.NewKubernetesRecorder(client.Kubernetes)
	client.KV = storagetesting.NewKVRecorder(client.KV, kubernetesRecorder)
	client.Kubernetes = kubernetesRecorder
	return client
}

func deleteNamespace(t testing.TB, ep string) {
	a, err := fdbdriver.OpenAdmin(ep)
	if err != nil {
		t.Logf("cleanup: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.Delete(ctx, ""); err != nil {
		t.Logf("cleanup: delete test namespace: %v", err)
	}
}
