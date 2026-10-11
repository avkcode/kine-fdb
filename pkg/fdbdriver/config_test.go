// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Unit tests for endpoint parsing, kine flag mapping, and namespace names.

package fdbdriver

import (
	"strings"
	"testing"
	"time"

	"github.com/k3s-io/kine/pkg/drivers"
	"github.com/k3s-io/kine/pkg/tls"
)

func TestParseEndpointDefaults(t *testing.T) {
	cfg, err := ParseEndpoint("fdb://")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClusterFile != "" || cfg.Namespace != DefaultNamespace || cfg.APIVersion != DefaultAPIVersion ||
		cfg.PollInterval != time.Second || cfg.TxnTimeout != 15*time.Second {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestParseEndpointClusterFile(t *testing.T) {
	cases := map[string]string{
		"fdb:///etc/foundationdb/fdb.cluster":                        "/etc/foundationdb/fdb.cluster",
		"fdb://fdb.cluster":                                          "fdb.cluster",
		"fdb://?cluster_file=/tmp/a.cluster":                         "/tmp/a.cluster",
		"fdb://lab?cluster_file=%2Fetc%2Ffoundationdb%2Ffdb.cluster": "/etc/foundationdb/fdb.cluster",
		"fdb:///path%20with%20space/fdb.cluster":                     "/path with space/fdb.cluster",
	}
	for endpoint, want := range cases {
		cfg, err := ParseEndpoint(endpoint)
		if err != nil {
			t.Fatalf("%s: %v", endpoint, err)
		}
		if cfg.ClusterFile != want {
			t.Fatalf("%s: cluster file %q, want %q", endpoint, cfg.ClusterFile, want)
		}
	}
}

func TestParseEndpointParameters(t *testing.T) {
	cfg, err := ParseEndpoint("fdb:///c?namespace=tenants/a&api_version=720&poll_interval=250ms&timeout=3s&startup_timeout=1m&tls_ca_file=/ca.pem&trace_dir=/tmp/t&trace_format=json")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Namespace != "/tenants/a/" || cfg.APIVersion != 720 || cfg.PollInterval != 250*time.Millisecond ||
		cfg.TxnTimeout != 3*time.Second || cfg.StartupTimeout != time.Minute || cfg.TLS.CAFile != "/ca.pem" ||
		cfg.TraceDir != "/tmp/t" || cfg.TraceFormat != "json" {
		t.Fatalf("parsed = %+v", cfg)
	}
}

func TestParseEndpointRejectsBadInput(t *testing.T) {
	for _, endpoint := range []string{
		"http://localhost",
		"fdb",
		"fdb://?namespce=typo",
		"fdb://?api_version=abc",
		"fdb://?api_version=740",
		"fdb://?api_version=520",
		"fdb://?poll_interval=0s",
		"fdb://?timeout=-1s",
		"fdb://?trace_format=yaml",
		"fdb://?namespace=/a/../b",
		"fdb://?namespace=/a\x01",
	} {
		if _, err := ParseEndpoint(endpoint); err == nil {
			t.Errorf("ParseEndpoint(%q) succeeded", endpoint)
		}
	}
	_, err := ParseEndpoint("fdb://?namespce=x&zz=1")
	if err == nil || !strings.Contains(err.Error(), "namespce, zz") {
		t.Fatalf("unknown parameter error should list them: %v", err)
	}
}

func TestNormalizeNamespace(t *testing.T) {
	for in, want := range map[string]string{
		"":             "/",
		"/":            "/",
		"a":            "/a/",
		"/a":           "/a/",
		"a/":           "/a/",
		"//a//b//":     "/a/b/",
		" /tenant-a/ ": "/tenant-a/",
	} {
		got, err := NormalizeNamespace(in)
		if err != nil || got != want {
			t.Errorf("NormalizeNamespace(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestNewConfigUsesKineFlags(t *testing.T) {
	cfg, err := NewConfig(&drivers.Config{
		Endpoint:              "fdb:///c?tls_cert_file=/dsn.pem",
		CompactInterval:       time.Minute,
		CompactIntervalJitter: 10,
		CompactTimeout:        7 * time.Second,
		CompactMinRetain:      50,
		CompactBatchSize:      300,
		PollBatchSize:         200,
		BackendTLSConfig:      tls.Config{CertFile: "/flag.pem", KeyFile: "/flag.key", CAFile: "/flag-ca.pem", SkipVerify: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CompactInterval != time.Minute || cfg.CompactIntervalJitter != 10 || cfg.CompactTimeout != 7*time.Second ||
		cfg.CompactMinRetain != 50 || cfg.CompactBatchSize != 300 || cfg.PollBatchSize != 200 {
		t.Fatalf("kine flags not applied: %+v", cfg)
	}
	// The endpoint wins over kine's TLS flags; flags fill the gaps.
	if cfg.TLS.CertFile != "/dsn.pem" || cfg.TLS.KeyFile != "/flag.key" || cfg.TLS.CAFile != "/flag-ca.pem" || cfg.TLS.VerifyPeers != "Check.Valid=0" {
		t.Fatalf("TLS = %+v", cfg.TLS)
	}

	if _, err := NewConfig(&drivers.Config{Endpoint: "fdb://", CompactBatchSize: 10}); err == nil {
		t.Fatal("compact batch size below the minimum was accepted")
	}
	if cfg, err := NewConfig(&drivers.Config{}); err != nil || cfg.Namespace != DefaultNamespace {
		t.Fatalf("empty endpoint (fdb as the default driver) = %+v, %v", cfg, err)
	}
}
