// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Parsing and validation of the fdb:// datastore endpoint and the kine flags
// that tune compaction and watch polling.

package fdbdriver

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/k3s-io/kine/pkg/drivers"
)

const (
	// Scheme is the endpoint scheme this driver registers with kine.
	Scheme = "fdb"

	// DefaultNamespace is used when the endpoint does not name one.
	DefaultNamespace = "/kine/"
	// DefaultAPIVersion is the FoundationDB API version selected by default.
	// It is also the highest version the bundled Go bindings support.
	DefaultAPIVersion = 730

	defaultPollInterval   = time.Second
	defaultTxnTimeout     = 15 * time.Second
	defaultStartupTimeout = 2 * time.Minute
	defaultCompactTimeout = 5 * time.Second
	defaultCompactBatch   = 1000
	defaultPollBatch      = 500
	minCompactBatch       = 100
)

// Config is the fully resolved driver configuration.
type Config struct {
	// ClusterFile is the FoundationDB cluster file. Empty selects the client
	// library default ($FDB_CLUSTER_FILE, ./fdb.cluster, then the platform path).
	ClusterFile string
	// Namespace isolates one etcd keyspace from others in the same cluster.
	// It is normalized to the form "/name/".
	Namespace string
	// APIVersion is the FoundationDB API version to select.
	APIVersion int
	// PollInterval bounds how long watchers wait when a FoundationDB watch
	// notification is missed. Changes normally arrive within milliseconds.
	PollInterval time.Duration
	// TxnTimeout bounds every transaction, including retries.
	TxnTimeout time.Duration
	// StartupTimeout bounds how long the driver waits for the cluster to
	// become reachable when it starts.
	StartupTimeout time.Duration

	// TLS configures client TLS to the FoundationDB cluster. Empty fields fall
	// back to the client library's FDB_TLS_* environment variables.
	TLS TLSConfig
	// TraceDir enables FoundationDB client trace files in this directory.
	TraceDir string
	// TraceFormat selects "xml" or "json" trace files.
	TraceFormat string

	// CompactInterval is the period of automatic compaction. Zero disables
	// it, in which case compaction only happens on explicit requests.
	CompactInterval time.Duration
	// CompactIntervalJitter is a +/- percentage applied to CompactInterval.
	CompactIntervalJitter int
	// CompactTimeout bounds each compaction transaction.
	CompactTimeout time.Duration
	// CompactMinRetain is the number of most recent revisions never compacted.
	CompactMinRetain int64
	// CompactBatchSize is the number of revisions cleaned per transaction.
	CompactBatchSize int64
	// PollBatchSize is the maximum number of revisions read per watch poll.
	PollBatchSize int64
}

// TLSConfig holds FoundationDB client TLS settings.
type TLSConfig struct {
	CertFile    string
	KeyFile     string
	CAFile      string
	VerifyPeers string
	Password    string
}

func (t TLSConfig) enabled() bool {
	return t.CertFile != "" || t.KeyFile != "" || t.CAFile != "" || t.VerifyPeers != "" || t.Password != ""
}

var knownParams = map[string]bool{
	"cluster_file":     true,
	"namespace":        true,
	"api_version":      true,
	"poll_interval":    true,
	"timeout":          true,
	"startup_timeout":  true,
	"tls_cert_file":    true,
	"tls_key_file":     true,
	"tls_ca_file":      true,
	"tls_verify_peers": true,
	"tls_password":     true,
	"trace_dir":        true,
	"trace_format":     true,
}

// ParseEndpoint parses an endpoint of the form
//
//	fdb://[cluster-file-path][?param=value&...]
//
// The path after "fdb://" is the cluster file, so fdb:///etc/foundationdb/fdb.cluster
// names an absolute path and fdb://fdb.cluster a relative one. The
// cluster_file parameter overrides the path.
func ParseEndpoint(endpoint string) (Config, error) {
	scheme, rest, ok := strings.Cut(endpoint, "://")
	if !ok || scheme != Scheme {
		return Config{}, fmt.Errorf("fdb: endpoint must start with %s://", Scheme)
	}
	path, rawQuery, _ := strings.Cut(rest, "?")
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return Config{}, fmt.Errorf("fdb: parse endpoint parameters: %w", err)
	}
	var unknown []string
	for name := range query {
		if !knownParams[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return Config{}, fmt.Errorf("fdb: unknown endpoint parameter(s): %s", strings.Join(unknown, ", "))
	}

	pathFile, err := url.PathUnescape(path)
	if err != nil {
		return Config{}, fmt.Errorf("fdb: parse cluster file path: %w", err)
	}

	cfg := Config{
		ClusterFile:      strings.TrimSpace(pathFile),
		Namespace:        DefaultNamespace,
		APIVersion:       DefaultAPIVersion,
		PollInterval:     defaultPollInterval,
		TxnTimeout:       defaultTxnTimeout,
		StartupTimeout:   defaultStartupTimeout,
		CompactTimeout:   defaultCompactTimeout,
		CompactBatchSize: defaultCompactBatch,
		PollBatchSize:    defaultPollBatch,
		TraceFormat:      "xml",
	}
	if v := strings.TrimSpace(query.Get("cluster_file")); v != "" {
		cfg.ClusterFile = v
	}
	if query.Has("namespace") {
		ns, err := NormalizeNamespace(query.Get("namespace"))
		if err != nil {
			return Config{}, err
		}
		cfg.Namespace = ns
	}
	if v := query.Get("api_version"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("fdb: parse api_version %q: %w", v, err)
		}
		cfg.APIVersion = n
	}
	for name, dst := range map[string]*time.Duration{
		"poll_interval":   &cfg.PollInterval,
		"timeout":         &cfg.TxnTimeout,
		"startup_timeout": &cfg.StartupTimeout,
	} {
		if v := query.Get(name); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return Config{}, fmt.Errorf("fdb: parse %s %q: %w", name, v, err)
			}
			*dst = d
		}
	}
	cfg.TLS = TLSConfig{
		CertFile:    query.Get("tls_cert_file"),
		KeyFile:     query.Get("tls_key_file"),
		CAFile:      query.Get("tls_ca_file"),
		VerifyPeers: query.Get("tls_verify_peers"),
		Password:    query.Get("tls_password"),
	}
	cfg.TraceDir = query.Get("trace_dir")
	if v := query.Get("trace_format"); v != "" {
		cfg.TraceFormat = v
	}
	return cfg, cfg.validate()
}

// NewConfig builds a Config from a kine driver configuration: the endpoint
// supplies FoundationDB settings, kine's flags supply compaction and polling
// settings, and kine's --ca-file/--cert-file/--key-file fill any TLS paths
// the endpoint leaves unset.
func NewConfig(dc *drivers.Config) (Config, error) {
	endpoint := dc.Endpoint
	if endpoint == "" {
		// kine passes an empty endpoint when this is the default driver.
		endpoint = Scheme + "://"
	}
	cfg, err := ParseEndpoint(endpoint)
	if err != nil {
		return Config{}, err
	}
	cfg.CompactInterval = dc.CompactInterval
	cfg.CompactIntervalJitter = dc.CompactIntervalJitter
	if dc.CompactTimeout > 0 {
		cfg.CompactTimeout = dc.CompactTimeout
	}
	cfg.CompactMinRetain = dc.CompactMinRetain
	if dc.CompactBatchSize > 0 {
		cfg.CompactBatchSize = dc.CompactBatchSize
	}
	if dc.PollBatchSize > 0 {
		cfg.PollBatchSize = dc.PollBatchSize
	}
	if cfg.TLS.CertFile == "" {
		cfg.TLS.CertFile = dc.BackendTLSConfig.CertFile
	}
	if cfg.TLS.KeyFile == "" {
		cfg.TLS.KeyFile = dc.BackendTLSConfig.KeyFile
	}
	if cfg.TLS.CAFile == "" {
		cfg.TLS.CAFile = dc.BackendTLSConfig.CAFile
	}
	if dc.BackendTLSConfig.SkipVerify && cfg.TLS.VerifyPeers == "" {
		cfg.TLS.VerifyPeers = "Check.Valid=0"
	}
	return cfg, cfg.validate()
}

func (c Config) validate() error {
	switch {
	case c.APIVersion < 630 || c.APIVersion > DefaultAPIVersion:
		return fmt.Errorf("fdb: api_version %d is not supported; use a value between 630 and %d", c.APIVersion, DefaultAPIVersion)
	case c.PollInterval <= 0:
		return fmt.Errorf("fdb: poll_interval must be positive")
	case c.TxnTimeout <= 0:
		return fmt.Errorf("fdb: timeout must be positive")
	case c.StartupTimeout <= 0:
		return fmt.Errorf("fdb: startup_timeout must be positive")
	case c.TraceFormat != "xml" && c.TraceFormat != "json":
		return fmt.Errorf("fdb: trace_format must be xml or json")
	case c.CompactInterval < 0:
		return fmt.Errorf("fdb: compact interval must not be negative")
	case c.CompactIntervalJitter < 0 || c.CompactIntervalJitter > 100:
		return fmt.Errorf("fdb: compact interval jitter must be between 0 and 100")
	case c.CompactMinRetain < 0:
		return fmt.Errorf("fdb: compact min retain must not be negative")
	case c.CompactBatchSize < minCompactBatch:
		return fmt.Errorf("fdb: compact batch size %d too small: must be at least %d", c.CompactBatchSize, minCompactBatch)
	case c.PollBatchSize <= 0:
		return fmt.Errorf("fdb: poll batch size must be positive")
	}
	return nil
}

// NormalizeNamespace returns the canonical "/a/b/" form of a namespace name.
// Leading, trailing, and repeated slashes are insignificant, so "tenant-a",
// "/tenant-a" and "/tenant-a/" all name the same namespace.
func NormalizeNamespace(ns string) (string, error) {
	var parts []string
	for _, p := range strings.Split(strings.TrimSpace(ns), "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "/", nil
	}
	for _, p := range parts {
		if p == "." || p == ".." {
			return "", fmt.Errorf("fdb: namespace %q must not contain %q segments", ns, p)
		}
		for _, r := range p {
			if r < 0x20 || r == 0x7f {
				return "", fmt.Errorf("fdb: namespace %q contains control characters", ns)
			}
		}
	}
	return "/" + strings.Join(parts, "/") + "/", nil
}

// String renders the configuration without secrets, for logs.
func (c Config) String() string {
	cluster := c.ClusterFile
	if cluster == "" {
		cluster = "<default>"
	}
	return fmt.Sprintf("cluster_file=%s namespace=%s api_version=%d tls=%t", cluster, c.Namespace, c.APIVersion, c.TLS.enabled())
}
