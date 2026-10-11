// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Prometheus metrics for transactions, retries, revisions, watchers, and
// compaction, labelled by namespace.

package fdbdriver

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/k3s-io/kine/pkg/server"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	txnTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kine_fdb_transactions_total",
		Help: "FoundationDB transactions by operation and result (success, rejected, canceled, error).",
	}, []string{"namespace", "operation", "result"})

	txnDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kine_fdb_transaction_duration_seconds",
		Help:    "Duration of FoundationDB operations including retries.",
		Buckets: []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"namespace", "operation"})

	txnRetries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kine_fdb_transaction_retries_total",
		Help: "FoundationDB transaction retries by operation and FoundationDB error code.",
	}, []string{"namespace", "operation", "code"})

	revisionGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kine_fdb_revision",
		Help: "Revisions of the namespace: current, and compact (oldest readable).",
	}, []string{"namespace", "kind"})

	watcherGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kine_fdb_watchers",
		Help: "Active watch subscriptions.",
	}, []string{"namespace"})

	compactedVersions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kine_fdb_compacted_versions_total",
		Help: "Key versions removed by compaction.",
	}, []string{"namespace"})

	allCollectors = []prometheus.Collector{txnTotal, txnDuration, txnRetries, revisionGauge, watcherGauge, compactedVersions}
)

type metrics struct {
	namespace string
}

func newMetrics(namespace string) *metrics {
	return &metrics{namespace: namespace}
}

// register adds the driver's collectors to reg. Collectors are shared by all
// namespaces in the process, so repeated registration is not an error.
func (m *metrics) register(reg prometheus.Registerer) {
	if reg == nil {
		return
	}
	for _, c := range allCollectors {
		if err := reg.Register(c); err != nil {
			var already prometheus.AlreadyRegisteredError
			if !errors.As(err, &already) {
				panic(err)
			}
		}
	}
}

func (m *metrics) observeTxn(op string, start time.Time, err error) {
	if m == nil {
		return
	}
	txnDuration.WithLabelValues(m.namespace, op).Observe(time.Since(start).Seconds())
	txnTotal.WithLabelValues(m.namespace, op, resultLabel(err)).Inc()
}

func resultLabel(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	case errors.Is(err, server.ErrKeyExists), errors.Is(err, server.ErrCompacted), errors.Is(err, server.ErrFutureRev),
		errors.Is(err, errEventsLost), errors.Is(err, ErrNamespaceNotFound):
		return "rejected"
	case status.Code(err) == codes.InvalidArgument:
		return "rejected"
	default:
		return "error"
	}
}

func (m *metrics) retry(op string, code int) {
	if m == nil {
		return
	}
	txnRetries.WithLabelValues(m.namespace, op, strconv.Itoa(code)).Inc()
}

func (m *metrics) currentRevision(rev int64) {
	if m == nil {
		return
	}
	revisionGauge.WithLabelValues(m.namespace, "current").Set(float64(rev))
}

func (m *metrics) compactRevision(rev int64) {
	if m == nil {
		return
	}
	revisionGauge.WithLabelValues(m.namespace, "compact").Set(float64(rev))
}

func (m *metrics) watchers(n int) {
	if m == nil {
		return
	}
	watcherGauge.WithLabelValues(m.namespace).Set(float64(n))
}

func (m *metrics) compacted(n int64) {
	if m == nil {
		return
	}
	compactedVersions.WithLabelValues(m.namespace).Add(float64(n))
}
