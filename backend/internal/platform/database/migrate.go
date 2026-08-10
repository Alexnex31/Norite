package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx/v5" database/sql driver golang-migrate needs
	"github.com/rs/zerolog"
)

// MigrationAdvisoryLockKey is the Postgres advisory lock ID this package serializes migrations on.
//
// The value is arbitrary but must never change: it is the whole coordination mechanism between processes.
// The bytes spell "NORITE" followed by a slot number, so it is recognizable in pg_locks output when an
// operator is trying to work out what is holding things up.
//
// This is *our* lock, distinct from the one golang-migrate's own Postgres driver takes internally (keyed
// off the database and schema name). Both exist and they do not conflict: ours is always acquired first
// and covers the entire run — version check included — where the driver's covers only its own statements.
const MigrationAdvisoryLockKey int64 = 0x4E4F524954450001

// lockPollInterval is how often acquireMigrationLock re-checks a lock held by another process.
const lockPollInterval = 250 * time.Millisecond

// MigrateOptions configures Migrate.
type MigrateOptions struct {
	// DatabaseURL is the Postgres connection string.
	DatabaseURL string
	// Source holds the migration files, normally the go:embed'd backend/migrations FS.
	Source fs.FS
	// SourceDir is the path within Source to read migrations from ("." for the FS root).
	SourceDir string
	// LockTimeout bounds how long to wait for the advisory lock before giving up. Waiting is normal —
	// another process mid-migration — so this only needs to be long enough to outlast a real migration.
	LockTimeout time.Duration
	// Logger receives progress lines. Optional.
	Logger *zerolog.Logger
}

// Migrate applies all pending migrations, serialized across processes by a Postgres advisory lock.
//
// It is deliberately blocking and is called before the server reports itself ready: a self-hosted
// instance must never serve against a not-yet-migrated schema (docs/architecture.md §2, "Cross-cutting"). The advisory
// lock is what makes that safe when two processes start at once — a self-hoster briefly running two
// copies of the binary, or a restart racing its predecessor — since without it both could try to apply
// the same migration concurrently.
//
// The lock lives on a dedicated connection, so a crashed process releases it automatically when its
// session ends rather than wedging every future startup.
//
// Canceling ctx aborts the wait for the lock, and stops a multi-migration run at the next boundary. It
// does NOT interrupt a statement already executing: golang-migrate's pgx driver runs each migration on
// its own context.Background(). A stop mid-run is reported as an error, never as success, so the caller
// cannot mark an instance ready against a partially-migrated schema.
//
// The flagship reaches the same guarantee through a Helm pre-upgrade Job running this same code path
// (docs/architecture.md §12); only the trigger differs.
func Migrate(ctx context.Context, opts MigrateOptions) error {
	logger := logFrom(opts.Logger)

	sourceDir := opts.SourceDir
	if sourceDir == "" {
		sourceDir = "."
	}

	lockConn, err := pgx.Connect(ctx, opts.DatabaseURL)
	if err != nil {
		return fmt.Errorf("migrate: could not connect to Postgres to take the migration lock: %w", err)
	}
	defer func() {
		// Detached context: the lock connection must still be closed cleanly even when ctx is already
		// canceled (shutdown mid-migration).
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = lockConn.Close(closeCtx)
	}()

	if err := acquireMigrationLock(ctx, lockConn, opts.LockTimeout, logger); err != nil {
		return err
	}
	defer releaseMigrationLock(ctx, lockConn, logger)

	return applyMigrations(ctx, opts.DatabaseURL, opts.Source, sourceDir, logger)
}

// acquireMigrationLock blocks until the advisory lock is held, timeout elapses, or ctx is canceled.
//
// It polls pg_try_advisory_lock rather than calling the blocking pg_advisory_lock so that waiting stays
// observable (one log line explaining the delay, rather than a silently hung startup) and so ctx
// cancellation is honored promptly.
func acquireMigrationLock(ctx context.Context, conn *pgx.Conn, timeout time.Duration, logger *zerolog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	waiting := false
	for {
		var acquired bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", MigrationAdvisoryLockKey).Scan(&acquired); err != nil {
			return fmt.Errorf("migrate: could not take the migration advisory lock: %w", err)
		}
		if acquired {
			if waiting {
				logger.Info().Msg("migration lock acquired")
			}
			return nil
		}

		if !waiting {
			waiting = true
			logger.Info().
				Int64("lock_key", MigrationAdvisoryLockKey).
				Dur("timeout", timeout).
				Msg("another process is running migrations — waiting for the migration lock")
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("migrate: timed out after %s waiting for the migration advisory lock "+
				"(another process may be stuck mid-migration; check pg_locks for objid %d): %w",
				timeout, MigrationAdvisoryLockKey, ctx.Err())
		case <-time.After(lockPollInterval):
		}
	}
}

// releaseMigrationLock drops the advisory lock. A failure here is logged, not returned: the migration
// itself already succeeded or failed on its own terms, and the lock is released regardless the moment
// this connection's session ends.
func releaseMigrationLock(ctx context.Context, conn *pgx.Conn, logger *zerolog.Logger) {
	unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if _, err := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", MigrationAdvisoryLockKey); err != nil {
		logger.Warn().Err(err).Msg("could not release the migration advisory lock (it frees on session end)")
	}
}

// applyMigrations runs golang-migrate to the latest version over its own short-lived connection.
func applyMigrations(ctx context.Context, databaseURL string, source fs.FS, sourceDir string, logger *zerolog.Logger) error {
	src, err := iofs.New(source, sourceDir)
	if err != nil {
		return fmt.Errorf("migrate: could not read embedded migrations from %q: %w", sourceDir, err)
	}

	// A dedicated database/sql handle rather than the application pool: golang-migrate wants a
	// database/sql connection, migrations are long-running DDL that has no business occupying an
	// application connection, and a single connection keeps every statement on the same session.
	sqlDB, err := sql.Open("pgx/v5", databaseURL)
	if err != nil {
		return fmt.Errorf("migrate: could not open a migration connection: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	defer func() { _ = sqlDB.Close() }()

	// StatementTimeout is deliberately left at zero (no limit). A migration that takes minutes — a large
	// index build — is legitimate, and killing it partway is exactly the dirty-schema state that wedges
	// every subsequent start. Operators who want a ceiling should set one per-migration in SQL.
	driver, err := migratepgx.WithInstance(sqlDB, &migratepgx.Config{})
	if err != nil {
		return fmt.Errorf("migrate: could not initialize the migration driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		return fmt.Errorf("migrate: could not initialize the migrator: %w", err)
	}
	defer func() {
		if srcErr, dbErr := m.Close(); srcErr != nil || dbErr != nil {
			logger.Warn().AnErr("source", srcErr).AnErr("database", dbErr).Msg("error closing the migrator")
		}
	}()

	// Relay ctx cancellation into golang-migrate's stop channel, so SIGTERM during a multi-migration run
	// stops it cleanly at the next boundary instead of being ignored until every migration has run.
	//
	// This stops *between* migrations, not mid-statement: golang-migrate's pgx driver executes each
	// migration on context.Background() internally, so a statement already running (a large CREATE INDEX,
	// say) still runs to completion. That is a property of the driver, not a choice made here — see the
	// StatementTimeout note below.
	stopRelay := make(chan struct{})
	defer close(stopRelay)
	go func() {
		select {
		case <-ctx.Done():
			select {
			case m.GracefulStop <- true:
			case <-stopRelay:
			}
		case <-stopRelay:
		}
	}()

	if before, dirty, err := m.Version(); err == nil && dirty {
		return fmt.Errorf("migrate: schema is marked dirty at version %d — a previous migration failed "+
			"partway through and must be resolved manually before the server can start", before)
	}

	start := time.Now()
	err = m.Up()

	// A graceful stop is NOT reported as an error: golang-migrate simply stops feeding migrations, so Up
	// returns nil exactly as it would on success. Checking ctx is the only way to tell the two apart, and
	// getting it wrong would be the worst possible outcome here — the caller would mark the instance ready
	// against a half-migrated schema. When both are true, treat it as stopped: aborting startup on a
	// schema that is actually complete costs one restart, the reverse costs correctness.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("migrate: stopped before completing (shutdown requested); the schema may be "+
			"partially migrated, so this instance must not serve: %w", ctxErr)
	}

	switch {
	case err == nil:
		version, _, verr := m.Version()
		if verr != nil {
			logger.Info().Dur("took", time.Since(start)).Msg("migrations applied")
			break
		}
		logger.Info().Uint("version", version).Dur("took", time.Since(start)).Msg("migrations applied")
	case errors.Is(err, migrate.ErrNoChange):
		version, _, verr := m.Version()
		if verr != nil {
			logger.Info().Msg("schema already up to date")
			break
		}
		logger.Info().Uint("version", version).Msg("schema already up to date")
	default:
		return fmt.Errorf("migrate: applying migrations failed: %w", err)
	}

	return nil
}
