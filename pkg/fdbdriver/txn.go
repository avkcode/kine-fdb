// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Context-aware transaction runner: bounded retries that honor request
// cancellation and deadlines, plus bookkeeping for commits whose outcome is
// unknown so that writes can be retried idempotently.

package fdbdriver

import (
	"context"
	"errors"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
)

// FoundationDB error codes the driver reacts to.
const (
	errCodeTransactionTooOld     = 1007
	errCodeNotCommitted          = 1020
	errCodeCommitUnknownResult   = 1021
	errCodeTransactionCancelled  = 1025
	errCodeTransactionTimedOut   = 1031
	errCodeClusterVersionChanged = 1039
)

// attempt carries state across the retries of one transaction.
type attempt struct {
	// n is the zero-based attempt number.
	n int
	// maybeCommitted reports that the previous attempt's commit ended with
	// an error that does not rule out success (commit_unknown_result).
	maybeCommitted bool
}

type txnOptions struct {
	// op names the operation for metrics and errors.
	op string
	// write commits the transaction; read-only transactions are discarded.
	write bool
	// timeout overrides the configured transaction timeout.
	timeout time.Duration
	// batchPriority runs the transaction at batch priority.
	batchPriority bool
}

type txnFunc func(tr fdb.Transaction, at *attempt) (any, error)

// run executes fn in a transaction, retrying retryable FoundationDB errors
// until the operation succeeds, fails with a non-retryable error, times out,
// or ctx is done. Errors returned by fn that are not FoundationDB errors end
// the operation immediately; run then returns fn's result alongside them.
func (s *store) run(ctx context.Context, o txnOptions, fn txnFunc) (any, error) {
	start := time.Now()
	ret, err := s.runAttempts(ctx, o, fn)
	s.metrics.observeTxn(o.op, start, err)
	return ret, err
}

func (s *store) runAttempts(ctx context.Context, o txnOptions, fn txnFunc) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	timeout := o.timeout
	if timeout <= 0 {
		timeout = s.cfg.TxnTimeout
	}
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}
	if timeout < time.Millisecond {
		return nil, context.DeadlineExceeded
	}

	tr, err := s.db.CreateTransaction()
	if err != nil {
		return nil, wrapFDBError(o.op, err)
	}
	if err := tr.Options().SetTimeout(timeout.Milliseconds()); err != nil {
		return nil, wrapFDBError(o.op, err)
	}
	if o.batchPriority {
		if err := tr.Options().SetPriorityBatch(); err != nil {
			return nil, wrapFDBError(o.op, err)
		}
	}
	// Cancelling the transaction fails every outstanding future, which
	// unblocks fn or the commit as soon as the caller gives up.
	stop := context.AfterFunc(ctx, tr.Cancel)
	defer stop()

	at := &attempt{}
	for {
		ret, err := callTxn(tr, at, fn)
		if err == nil && o.write {
			err = tr.Commit().Get()
		}
		if err == nil {
			return ret, nil
		}

		var fe fdb.Error
		if !errors.As(err, &fe) {
			return ret, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		maybeCommitted := o.write &&
			(fe.Code == errCodeCommitUnknownResult || fe.Code == errCodeClusterVersionChanged)
		if onErr := tr.OnError(fe).Get(); onErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, wrapFDBError(o.op, onErr)
		}
		at.maybeCommitted = maybeCommitted
		at.n++
		s.metrics.retry(o.op, fe.Code)
	}
}

// callTxn runs fn, converting the fdb.Error panics raised by MustGet-style
// helpers into ordinary errors so the retry loop can handle them.
func callTxn(tr fdb.Transaction, at *attempt, fn txnFunc) (ret any, err error) {
	defer func() {
		if r := recover(); r != nil {
			fe, ok := r.(fdb.Error)
			if !ok {
				panic(r)
			}
			err = fe
		}
	}()
	return fn(tr, at)
}

// read runs a read-only transaction.
func (s *store) read(ctx context.Context, op string, fn func(tr fdb.Transaction) (any, error)) (any, error) {
	return s.run(ctx, txnOptions{op: op}, func(tr fdb.Transaction, _ *attempt) (any, error) {
		return fn(tr)
	})
}
