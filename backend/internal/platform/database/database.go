// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package database owns the backend's Postgres access plumbing: the pgx connection pool, the
// transaction helper every service writes through, and the advisory-lock-guarded migration runner that
// gates startup.
//
// It deliberately contains no queries. All SQL is sqlc-generated and parameterized by construction
// (CLAUDE.md rule 3, docs/architecture.md §14.1); this package only supplies the connection and
// transaction machinery those generated queries run on.
package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// PoolOptions configures New.
type PoolOptions struct {
	// DatabaseURL is the Postgres connection string.
	DatabaseURL string
	// MaxConns / MinConns size the pool. Kept deliberately small per replica — see
	// docs/architecture.md §11 "Database connection management" and config.Config's own commentary.
	MaxConns int32
	MinConns int32
	// ConnectTimeout bounds how long New waits for the first successful connection.
	ConnectTimeout time.Duration
}

// New opens a pgx connection pool and verifies it can actually reach Postgres.
//
// Verifying up front is the point: pgxpool connects lazily, so without the ping a misconfigured DSN would
// surface as a confusing failure on the first real request instead of as a clear startup error.
func New(ctx context.Context, opts PoolOptions) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(opts.DatabaseURL)
	if err != nil {
		// The DSN carries a password, so it must not be echoed into the error (CLAUDE.md rule 8).
		return nil, errors.New("database: could not parse the configured Postgres URL")
	}

	poolCfg.MaxConns = opts.MaxConns
	poolCfg.MinConns = opts.MinConns
	// Recycle connections periodically so a long-lived pool doesn't pin connections across a Postgres
	// restart or a failover behind a floating address.
	poolCfg.MaxConnLifetime = time.Hour
	poolCfg.MaxConnIdleTime = 30 * time.Minute
	poolCfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("database: could not create connection pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, opts.ConnectTimeout)
	defer cancel()

	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: could not reach Postgres within %s: %w", opts.ConnectTimeout, err)
	}

	return pool, nil
}

// RunInTx runs fn inside a transaction, committing on success and rolling back on any error or panic.
//
// Every mutating service method goes through this. It is what makes the project's
// mutation-and-audit-log-in-one-transaction rule (CLAUDE.md rule 2) expressible as a single fn, and what
// gives the gateway its "dispatch only after commit" ordering (rule 5): whatever must happen only once the
// change is durable is registered with AfterCommit on the ctx fn receives, and runs after Commit returns
// nil — never if the transaction rolls back.
//
// fn's ctx is the caller's with the transaction's hook list added. Closures conventionally name it ctx and
// shadow the outer one, so every call made inside fn carries the list without being edited; a call that
// reaches past the shadow to the outer ctx and registers a hook panics rather than losing it (see
// AfterCommit).
func RunInTx(ctx context.Context, pool *pgxpool.Pool, fn func(ctx context.Context, tx pgx.Tx) error) (err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database: begin transaction: %w", err)
	}
	hooks := &afterCommit{}
	txCtx := context.WithValue(ctx, afterCommitKey{}, hooks)

	defer func() {
		if p := recover(); p != nil {
			// Roll back before re-panicking, otherwise the connection goes back to the pool with an
			// open transaction on it.
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
		if err == nil {
			return
		}
		// Roll back with a context detached from the caller's: if fn failed *because* ctx was
		// canceled, a rollback on that same context would fail too and leave the transaction to be
		// cleaned up only when the connection is reaped.
		if rbErr := tx.Rollback(context.WithoutCancel(ctx)); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			err = errors.Join(err, fmt.Errorf("database: rollback: %w", rbErr))
		}
	}()

	if err = fn(txCtx, tx); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit transaction: %w", err)
	}

	// Detached from cancellation: the change is committed, and a client hanging up after that must not stop
	// the event describing it from being published or a revoked session's connection from being closed.
	hooks.run(context.WithoutCancel(ctx))
	return nil
}

// AfterCommit registers fn to run once the transaction ctx belongs to has committed, in registration order.
//
// It panics when ctx carries no transaction. The two quiet alternatives are both wrong in a way no test
// would notice: running fn at once is exactly the before-commit dispatch rule 5 forbids, and dropping it
// loses an event or leaves a revoked session's connection open. A panic is found the first time the path
// runs, which is always in a test.
func AfterCommit(ctx context.Context, fn func(ctx context.Context)) {
	hooks, ok := ctx.Value(afterCommitKey{}).(*afterCommit)
	if !ok {
		panic("database.AfterCommit called outside a RunInTx transaction")
	}
	hooks.fns = append(hooks.fns, fn)
}

type afterCommitKey struct{}

// afterCommit is one transaction's hook list. Not synchronized: fn runs on one goroutine, and registering
// from a goroutine fn started would be a transaction used concurrently, which pgx does not support either.
type afterCommit struct {
	fns []func(ctx context.Context)
}

// run calls every hook, even after one panics, then re-raises the first panic.
//
// Every hook runs because they are independent and the transaction is already committed: a panic while
// publishing one event must not be what stops a revoked session's connection from being closed. The panic
// still surfaces afterwards, because it is a bug and swallowing it would hide one.
func (a *afterCommit) run(ctx context.Context) {
	var first any
	for _, fn := range a.fns {
		func() {
			defer func() {
				if p := recover(); p != nil && first == nil {
					first = p
				}
			}()
			fn(ctx)
		}()
	}
	if first != nil {
		panic(first)
	}
}

// logFrom returns a logger for use inside this package, tolerating a nil one from callers that don't
// have logging wired yet (tests, mostly).
func logFrom(logger *zerolog.Logger) *zerolog.Logger {
	if logger != nil {
		return logger
	}
	nop := zerolog.Nop()
	return &nop
}
