// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Driver error values and the mapping of FoundationDB errors onto gRPC
// status codes that etcd clients understand.

package fdbdriver

import (
	"errors"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	// ErrNamespaceNotFound is returned by administrative operations on a
	// namespace that has never been created.
	ErrNamespaceNotFound = errors.New("fdb: namespace does not exist")

	// errNamespaceGone is returned when the namespace metadata disappears
	// while the driver is running, which happens when an operator deletes
	// the namespace underneath a live server.
	errNamespaceGone = status.Error(codes.FailedPrecondition,
		"fdb: namespace metadata is missing; it was deleted while in use, restart kine-fdb to recreate it")
)

// wrapFDBError annotates a FoundationDB error with the operation that failed
// and gives it a gRPC status code so clients can tell transient
// unavailability from bad requests.
func wrapFDBError(op string, err error) error {
	var fe fdb.Error
	if !errors.As(err, &fe) {
		return fmt.Errorf("fdb: %s: %w", op, err)
	}
	code := codes.Unavailable
	switch fe.Code {
	case 2101, 2102, 2103: // transaction_too_large, key_too_large, value_too_large
		code = codes.InvalidArgument
	case 2000, 2004, 2005, 2006, 2007, 2008, 2010, 2011, 2012, 2013, 2014, 2015, 2017:
		// client usage errors indicate a driver bug, not an outage
		code = codes.Internal
	}
	return status.Errorf(code, "fdb: %s failed: %v", op, fe)
}
