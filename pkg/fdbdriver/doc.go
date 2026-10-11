// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Package documentation for the FoundationDB kine driver.

// Package fdbdriver is a kine storage driver backed by FoundationDB.
//
// Importing the package registers the "fdb" endpoint scheme with kine:
//
//	import _ "github.com/avkcode/kine-fdb/pkg/fdbdriver"
//
// An endpoint names a FoundationDB cluster file and a namespace:
//
//	fdb:///etc/foundationdb/fdb.cluster?namespace=/clusters/prod-a
//
// Each namespace is an independent etcd keyspace with its own revision
// sequence, stored in its own FoundationDB directory, so many Kubernetes
// control planes can share one FoundationDB cluster.
//
// Every write is one FoundationDB transaction that takes the next revision,
// so revisions are dense and totally ordered within a namespace. Each change
// writes a history entry, the key's latest-state index row, and a change-log
// entry; watches follow the change log, and compaction deletes history that
// can no longer be read. See docs/architecture.md for the full layout.
package fdbdriver
