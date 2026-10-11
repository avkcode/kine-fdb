// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Process-wide FoundationDB client setup: API version selection, network
// options such as TLS and tracing, and a cache of database handles.

package fdbdriver

import (
	"fmt"
	"sync"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
)

// The FoundationDB client is a process-wide singleton: the API version and
// network options can be chosen once, before the network thread starts.
// Database handles are cached per cluster file and intentionally never
// closed, because the Go bindings share one handle per cluster file and
// closing it would invalidate every other user of that handle.
var client struct {
	sync.Mutex
	networkSet bool
	network    networkSettings
	dbs        map[string]fdb.Database
}

type networkSettings struct {
	tls         TLSConfig
	traceDir    string
	traceFormat string
}

func openDatabase(cfg Config) (fdb.Database, error) {
	client.Lock()
	defer client.Unlock()

	if err := selectAPIVersion(cfg.APIVersion); err != nil {
		return fdb.Database{}, err
	}

	want := networkSettings{tls: cfg.TLS, traceDir: cfg.TraceDir}
	if cfg.TraceDir != "" {
		want.traceFormat = cfg.TraceFormat
	}
	if !client.networkSet {
		if err := applyNetworkSettings(want); err != nil {
			return fdb.Database{}, err
		}
		client.network = want
		client.networkSet = true
	} else if client.network != want {
		return fdb.Database{}, fmt.Errorf("fdb: TLS and trace options must be identical for every endpoint in one process")
	}

	if db, ok := client.dbs[cfg.ClusterFile]; ok {
		return db, nil
	}
	db, err := fdb.OpenDatabase(cfg.ClusterFile)
	if err != nil {
		return fdb.Database{}, fmt.Errorf("fdb: open database: %w", err)
	}
	if client.dbs == nil {
		client.dbs = make(map[string]fdb.Database)
	}
	client.dbs[cfg.ClusterFile] = db
	return db, nil
}

func selectAPIVersion(version int) error {
	if fdb.IsAPIVersionSelected() {
		selected := fdb.MustGetAPIVersion()
		if selected != version {
			return fmt.Errorf("fdb: API version %d is already selected in this process; cannot use %d", selected, version)
		}
		return nil
	}
	if err := fdb.APIVersion(version); err != nil {
		return fmt.Errorf("fdb: select API version %d: %w", version, err)
	}
	return nil
}

func applyNetworkSettings(s networkSettings) error {
	opts := fdb.Options()
	set := func(name string, err error) error {
		if err != nil {
			return fmt.Errorf("fdb: set network option %s: %w", name, err)
		}
		return nil
	}
	steps := []struct {
		name  string
		value string
		apply func(string) error
	}{
		{"tls_cert_file", s.tls.CertFile, opts.SetTLSCertPath},
		{"tls_key_file", s.tls.KeyFile, opts.SetTLSKeyPath},
		{"tls_ca_file", s.tls.CAFile, opts.SetTLSCaPath},
		{"tls_password", s.tls.Password, opts.SetTLSPassword},
		{"tls_verify_peers", s.tls.VerifyPeers, func(v string) error { return opts.SetTLSVerifyPeers([]byte(v)) }},
		{"trace_dir", s.traceDir, opts.SetTraceEnable},
		{"trace_format", s.traceFormat, opts.SetTraceFormat},
	}
	for _, step := range steps {
		if step.value == "" {
			continue
		}
		if err := set(step.name, step.apply(step.value)); err != nil {
			return err
		}
	}
	if s.traceDir != "" {
		if err := set("trace_log_group", opts.SetTraceLogGroup("kine-fdb")); err != nil {
			return err
		}
	}
	return nil
}
