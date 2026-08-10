package database

import (
	"context"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/migrations"
)

func TestMigrateAppliesTheEmbeddedMigrations(t *testing.T) {
	dsn := freshDatabase(t)

	require.NoError(t, Migrate(context.Background(), migrateOptions(dsn)))

	version, dirty := schemaVersion(t, dsn)
	assert.Equal(t, int64(1), version)
	assert.False(t, dirty)
}

func TestMigrateIsIdempotent(t *testing.T) {
	dsn := freshDatabase(t)
	ctx := context.Background()

	require.NoError(t, Migrate(ctx, migrateOptions(dsn)))
	// Every restart re-runs this; a second run must be a no-op, not an error.
	require.NoError(t, Migrate(ctx, migrateOptions(dsn)))

	version, dirty := schemaVersion(t, dsn)
	assert.Equal(t, int64(1), version)
	assert.False(t, dirty)
}

// The M1 done-when criterion, and the reason the advisory lock exists: two processes starting at once
// must not both try to apply the same migration. The second one waits.
func TestMigrateBlocksWhileAnotherProcessHoldsTheLock(t *testing.T) {
	dsn := freshDatabase(t)
	ctx := context.Background()

	// Stand in for a second server process mid-migration: hold the same advisory lock on its own
	// session.
	holder, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer func() { _ = holder.Close(context.Background()) }()

	var acquired bool
	require.NoError(t, holder.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", MigrationAdvisoryLockKey).Scan(&acquired))
	require.True(t, acquired)

	done := make(chan error, 1)
	go func() { done <- Migrate(context.Background(), migrateOptions(dsn)) }()

	// While the lock is held, Migrate must still be waiting — and crucially must not have applied
	// anything.
	select {
	case err := <-done:
		t.Fatalf("Migrate returned while the lock was held: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
	assert.False(t, schemaMigrationsExists(t, dsn), "no migration may be applied while another process holds the lock")

	// Release it, standing in for the first process finishing.
	_, err = holder.Exec(ctx, "SELECT pg_advisory_unlock($1)", MigrationAdvisoryLockKey)
	require.NoError(t, err)

	select {
	case err := <-done:
		require.NoError(t, err, "Migrate must proceed once the lock is free")
	case <-time.After(30 * time.Second):
		t.Fatal("Migrate did not proceed after the lock was released")
	}

	version, dirty := schemaVersion(t, dsn)
	assert.Equal(t, int64(1), version)
	assert.False(t, dirty)
}

// A lock that is never released must fail startup with an actionable message rather than hanging forever.
func TestMigrateGivesUpWhenTheLockIsNeverReleased(t *testing.T) {
	dsn := freshDatabase(t)
	ctx := context.Background()

	holder, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer func() { _ = holder.Close(context.Background()) }()

	var acquired bool
	require.NoError(t, holder.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", MigrationAdvisoryLockKey).Scan(&acquired))
	require.True(t, acquired)

	opts := migrateOptions(dsn)
	opts.LockTimeout = time.Second

	err = Migrate(ctx, opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
	assert.Contains(t, err.Error(), "pg_locks", "the error should tell an operator where to look")
}

// Canceling the context (SIGTERM during startup) must abort the wait promptly.
func TestMigrateHonorsContextCancellationWhileWaiting(t *testing.T) {
	dsn := freshDatabase(t)

	holder, err := pgx.Connect(context.Background(), dsn)
	require.NoError(t, err)
	defer func() { _ = holder.Close(context.Background()) }()

	var acquired bool
	require.NoError(t, holder.QueryRow(context.Background(), "SELECT pg_try_advisory_lock($1)", MigrationAdvisoryLockKey).Scan(&acquired))
	require.True(t, acquired)

	ctx, cancel := context.WithCancel(context.Background())
	opts := migrateOptions(dsn)
	opts.LockTimeout = time.Minute

	done := make(chan error, 1)
	go func() { done <- Migrate(ctx, opts) }()

	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(10 * time.Second):
		t.Fatal("Migrate ignored context cancellation")
	}
}

// The advisory lock must be released when Migrate returns, or the next startup would block on a lock
// nobody is using.
func TestMigrateReleasesTheLock(t *testing.T) {
	dsn := freshDatabase(t)
	ctx := context.Background()

	require.NoError(t, Migrate(ctx, migrateOptions(dsn)))

	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()

	var acquired bool
	require.NoError(t, conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", MigrationAdvisoryLockKey).Scan(&acquired))
	assert.True(t, acquired, "the migration lock should be free once Migrate has returned")
}

// A schema left dirty by a half-applied migration needs a human, not another automatic attempt.
func TestMigrateRefusesToRunAgainstADirtySchema(t *testing.T) {
	dsn := freshDatabase(t)
	ctx := context.Background()

	require.NoError(t, Migrate(ctx, migrateOptions(dsn)))

	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()
	_, err = conn.Exec(ctx, `UPDATE schema_migrations SET dirty = true`)
	require.NoError(t, err)

	err = Migrate(ctx, migrateOptions(dsn))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dirty")
	assert.Contains(t, err.Error(), "manually")
}

func TestMigrateReportsBrokenMigrationSQL(t *testing.T) {
	dsn := freshDatabase(t)

	opts := migrateOptions(dsn)
	opts.Source = fstest.MapFS{
		"000001_broken.up.sql":   &fstest.MapFile{Data: []byte("CREATE TABLE (;")},
		"000001_broken.down.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
	}

	err := Migrate(context.Background(), opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "applying migrations failed")
}

func migrateOptions(dsn string) MigrateOptions {
	return MigrateOptions{
		DatabaseURL: dsn,
		Source:      migrations.FS,
		SourceDir:   ".",
		LockTimeout: 30 * time.Second,
	}
}

func schemaVersion(t *testing.T, dsn string) (version int64, dirty bool) {
	t.Helper()

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()

	require.NoError(t, conn.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty))
	return version, dirty
}

func schemaMigrationsExists(t *testing.T, dsn string) bool {
	t.Helper()

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()

	var exists bool
	require.NoError(t, conn.QueryRow(ctx, `SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&exists))
	return exists
}
