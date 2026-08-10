package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewConnectsToPostgres(t *testing.T) {
	pool := openPool(t, freshDatabase(t))

	require.NoError(t, pool.Ping(context.Background()))
	assert.Equal(t, int32(4), pool.Config().MaxConns)
}

func TestNewFailsFastWhenPostgresIsUnreachable(t *testing.T) {
	// Port 1 is reserved and never listening, so this exercises the connect path rather than an auth
	// or database-name failure.
	_, err := New(context.Background(), PoolOptions{
		DatabaseURL:    "postgres://norite:norite@127.0.0.1:1/norite?sslmode=disable",
		MaxConns:       4,
		MinConns:       0,
		ConnectTimeout: 2 * time.Second,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not reach Postgres")
}

// The DSN carries the database password, so a parse failure must not echo it back (CLAUDE.md rule 8).
func TestNewDoesNotLeakTheDSNOnParseFailure(t *testing.T) {
	_, err := New(context.Background(), PoolOptions{
		DatabaseURL:    "postgres://norite:hunter2@:::/bad-url",
		ConnectTimeout: time.Second,
	})

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "hunter2")
}

func TestRunInTxCommits(t *testing.T) {
	pool := openPool(t, freshDatabase(t))
	createScratchTable(t, pool)
	ctx := context.Background()

	err := RunInTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO scratch (note) VALUES ($1)`, "committed")
		return err
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"committed"}, scratchRows(t, pool))
}

func TestRunInTxRollsBackOnError(t *testing.T) {
	pool := openPool(t, freshDatabase(t))
	createScratchTable(t, pool)
	ctx := context.Background()

	sentinel := errors.New("business rule violated")
	err := RunInTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO scratch (note) VALUES ($1)`, "should vanish"); err != nil {
			return err
		}
		// This is the shape that matters for CLAUDE.md rule 2: a mutation and its audit-log write share
		// one transaction, so a failure after the first write must undo it.
		return sentinel
	})

	require.ErrorIs(t, err, sentinel)
	assert.Empty(t, scratchRows(t, pool), "the write must not survive a failed transaction")
}

func TestRunInTxRollsBackOnPanicAndRepanics(t *testing.T) {
	pool := openPool(t, freshDatabase(t))
	createScratchTable(t, pool)
	ctx := context.Background()

	require.Panics(t, func() {
		_ = RunInTx(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO scratch (note) VALUES ($1)`, "should vanish")
			require.NoError(t, err)
			panic("handler blew up mid-transaction")
		})
	})

	assert.Empty(t, scratchRows(t, pool))

	// The connection must go back to the pool usable, not stuck inside an open transaction.
	require.NoError(t, RunInTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO scratch (note) VALUES ($1)`, "after panic")
		return err
	}))
	assert.Equal(t, []string{"after panic"}, scratchRows(t, pool))
}

// A transaction whose context was canceled must still be rolled back — the rollback runs on a detached
// context precisely so it isn't canceled along with the work it is undoing.
func TestRunInTxRollsBackWhenContextIsCancelled(t *testing.T) {
	pool := openPool(t, freshDatabase(t))
	createScratchTable(t, pool)

	ctx, cancel := context.WithCancel(context.Background())

	err := RunInTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO scratch (note) VALUES ($1)`, "should vanish"); err != nil {
			return err
		}
		cancel()
		return ctx.Err()
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, scratchRows(t, pool))
}

func openPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()

	pool, err := New(context.Background(), PoolOptions{
		DatabaseURL:    dsn,
		MaxConns:       4,
		MinConns:       0,
		ConnectTimeout: 30 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	return pool
}

// createScratchTable sets up a throwaway table for the transaction tests.
//
// Hand-written DDL is fine here and does not conflict with CLAUDE.md rule 3: the rule governs application
// queries, which are sqlc-generated and parameterized. This is fixed test scaffolding with no interpolated
// input, and it deliberately does not live in migrations/ — no production schema exists until M4.
func createScratchTable(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	_, err := pool.Exec(context.Background(), `CREATE TABLE scratch (id bigserial PRIMARY KEY, note text NOT NULL)`)
	require.NoError(t, err)
}

func scratchRows(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()

	rows, err := pool.Query(context.Background(), `SELECT note FROM scratch ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()

	notes := []string{}
	for rows.Next() {
		var note string
		require.NoError(t, rows.Scan(&note))
		notes = append(notes, note)
	}
	require.NoError(t, rows.Err())

	if len(notes) == 0 {
		return nil
	}
	return notes
}
