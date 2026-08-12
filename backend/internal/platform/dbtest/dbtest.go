// Package dbtest provides a real Postgres for integration tests.
//
// Integration tests here run against actual Postgres in a container rather than a mock. The behavior under
// test — advisory locks, transaction semantics, unique constraints, `citext` comparison, golang-migrate's
// bookkeeping — is entirely Postgres behavior, and a mock would only assert that the test author's mental
// model matches itself.
//
// # Why this is a normal package rather than a _test.go file
//
// A helper defined in one package's tests cannot be used by another's. This started life inside the
// database package's own tests and is extracted here because every domain package from M4 onward needs the
// same thing, and the alternative — a copy per package — is a hundred lines of container plumbing that
// drifts.
//
// It deliberately does *not* depend on internal/platform/database: that package's own tests use this one,
// and a dependency in the other direction would be a cycle. Callers run migrations themselves.
package dbtest

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// adminDSN points at the shared container's default database. Tests create their own databases through it.
var adminDSN string

// Main starts one Postgres container for the calling package, runs its tests, and tears the container down.
//
// A package that needs a database calls this from its own TestMain. One container per package rather than
// per test: starting Postgres costs seconds, and per-test *databases* inside it already give the isolation
// that matters.
//
// Under `-short` no container is started at all, so `go test -short ./...` stays usable on a machine with
// no container runtime — which is what `just test-short` relies on.
func Main(m *testing.M) {
	// testing.Short() reads a flag, so flags must be parsed before consulting it from TestMain.
	flag.Parse()

	if testing.Short() {
		os.Exit(m.Run())
	}

	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("norite_test"),
		postgres.WithUsername("norite"),
		postgres.WithPassword("norite"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dbtest: could not start Postgres container: %v\n", err)
		fmt.Fprintln(os.Stderr, "dbtest: run with -short to skip container-backed tests")
		os.Exit(1)
	}

	adminDSN, err = container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "dbtest: could not build connection string: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	if err := testcontainers.TerminateContainer(container); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest: could not terminate container: %v\n", err)
	}
	os.Exit(code)
}

// RequireContainer skips a test when container-backed tests are disabled.
func RequireContainer(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping container-backed test in -short mode")
	}
}

// FreshDatabase creates an empty database inside the shared container and returns its DSN.
//
// Per-test databases rather than per-test schemas: golang-migrate's bookkeeping table and the migration
// advisory lock are both database-scoped, so sharing one database would make tests interfere in precisely
// the way some of them exist to test.
func FreshDatabase(t *testing.T) string {
	t.Helper()
	RequireContainer(t)

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminDSN)
	requireNoErr(t, err, "connecting to the container's admin database")
	defer func() { _ = admin.Close(ctx) }()

	name := databaseNameFor(t)
	// Identifiers cannot be parameterized in DDL. The name is derived from the test name and filtered to
	// [a-z0-9_] by sanitizeIdentifier, so nothing arbitrary reaches this string.
	_, err = admin.Exec(ctx, `DROP DATABASE IF EXISTS "`+name+`"`)
	requireNoErr(t, err, "dropping any leftover test database")
	_, err = admin.Exec(ctx, `CREATE DATABASE "`+name+`"`)
	requireNoErr(t, err, "creating the test database")

	t.Cleanup(func() {
		// A context detached from the test's, with its own timeout: cleanup must still run when the test
		// failed because its context was canceled.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		conn, err := pgx.Connect(cleanupCtx, adminDSN)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(cleanupCtx) }()
		// FORCE terminates any connection still holding the database open, which a pool the test forgot to
		// close would otherwise do — turning a leak into a hang rather than a dropped database.
		_, _ = conn.Exec(cleanupCtx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
	})

	return replaceDatabaseName(adminDSN, name)
}

// databaseNameFor derives a valid, unique database name from the test name.
func databaseNameFor(t *testing.T) string {
	t.Helper()

	name := sanitizeIdentifier(strings.ToLower(t.Name()))
	// Postgres truncates identifiers at 63 bytes; keep the tail, which is the part that differs between
	// sibling subtests.
	const maxLen = 50
	if len(name) > maxLen {
		name = name[len(name)-maxLen:]
	}
	return "t_" + name
}

// sanitizeIdentifier replaces everything that is not a lowercase letter, digit, or underscore.
func sanitizeIdentifier(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// replaceDatabaseName swaps the database path segment of a DSN.
func replaceDatabaseName(dsn, name string) string {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		panic("dbtest: unparseable admin DSN: " + err.Error())
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, name)
}

// requireNoErr fails the test with context. Kept local so this package does not force testify on importers.
func requireNoErr(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("dbtest: %s: %v", what, err)
	}
}
