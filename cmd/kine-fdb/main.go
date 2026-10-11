// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// kine-fdb: the kine etcd API server with the FoundationDB driver compiled
// in and selected by default, plus HTTP health endpoints for probes. All
// other flags are kine's.

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/k3s-io/kine/pkg/app"
	"github.com/k3s-io/kine/pkg/drivers"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"

	"github.com/avkcode/kine-fdb/pkg/fdbdriver"
	"github.com/avkcode/kine-fdb/pkg/version"
)

func main() {
	// kine's built-in drivers register sqlite as the default; without an
	// --endpoint this binary should use FoundationDB instead.
	drivers.SetDefault(fdbdriver.Scheme)

	a := app.New()
	a.Name = "kine-fdb"
	a.Usage = "etcd v3 API for Kubernetes, stored in FoundationDB"
	a.Version = version.String()
	a.Flags = append(a.Flags, &cli.StringFlag{
		Name:    "health-bind-address",
		Usage:   "address for the /healthz and /readyz HTTP endpoints; empty or 0 disables them",
		Value:   ":8081",
		EnvVars: []string{"KINE_FDB_HEALTH_BIND_ADDRESS"},
	})
	run := a.Action
	a.Action = func(c *cli.Context) error {
		if addr := c.String("health-bind-address"); addr != "" && addr != "0" {
			go serveHealth(c.Context, addr, c.String("endpoint"))
		}
		return run(c)
	}
	if err := a.Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "kine-fdb: %v\n", err)
		os.Exit(1)
	}
}

// serveHealth answers /healthz while the process runs and /readyz when the
// FoundationDB namespace can be read.
func serveHealth(ctx context.Context, addr, endpoint string) {
	if endpoint == "" {
		endpoint = fdbdriver.Scheme + "://"
	}
	ready := readiness(endpoint)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := ready(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logrus.Errorf("kine-fdb: health endpoint: %v", err)
	}
}

// readiness returns a check for endpoint. Endpoints of other kine drivers
// are always ready once the process serves.
func readiness(endpoint string) func(context.Context) error {
	if !strings.HasPrefix(endpoint, fdbdriver.Scheme+"://") {
		return func(context.Context) error { return nil }
	}
	var (
		mu    sync.Mutex
		admin *fdbdriver.Admin
	)
	return func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if admin == nil {
			a, err := fdbdriver.OpenAdmin(endpoint)
			if err != nil {
				return err
			}
			admin = a
		}
		return admin.Ping(ctx)
	}
}
