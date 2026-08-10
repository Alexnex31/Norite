package database

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// These tests run against a real Postgres in a container rather than a mock. The behavior under test —
// advisory locks, transaction semantics, golang-migrate's bookkeeping — is entirely Postgres behavior, so
// a mock would only assert that the test author's mental model matches itself.
//
// One container is shared by the whole package; each test gets its own freshly-created database inside it
// so migration state cannot leak between tests.

// adminDSN points at the container's default database. Tests create their own databases through it.
var adminDSN string

func TestMain(m *testing.M) {
	// testing.Short() reads a flag, so the flags have to be parsed first when consulting it from
	// TestMain rather than from a test function.
	flag.Parse()

	if testing.Short() {
		// -short skips everything Docker-dependent, so `go test -short ./...` stays usable on a machine
		// without a container runtime.
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
		fmt.Fprintf(os.Stderr, "database tests: could not start Postgres container: %v\n", err)
		fmt.Fprintln(os.Stderr, "database tests: run with -short to skip container-backed tests")
		os.Exit(1)
	}

	adminDSN, err = container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "database tests: could not build connection string: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	if err := testcontainers.TerminateContainer(container); err != nil {
		fmt.Fprintf(os.Stderr, "database tests: could not terminate container: %v\n", err)
	}
	os.Exit(code)
}

// requireContainer skips a test when container-backed tests are disabled.
func requireContainer(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping container-backed test in -short mode")
	}
}

// freshDatabase creates an empty database inside the shared container and returns its DSN.
//
// Per-test databases rather than per-test schemas: golang-migrate's bookkeeping table and our advisory
// lock are both database-scoped, so sharing one database would make these tests interfere in exactly the
// way they are meant to be testing.
func freshDatabase(t *testing.T) string {
	t.Helper()
	requireContainer(t)

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminDSN)
	require.NoError(t, err)
	defer func() { _ = admin.Close(ctx) }()

	name := databaseNameFor(t)
	// Identifiers cannot be parameterized in DDL. The name is derived from the test name and filtered to
	// [a-z0-9_] below, so nothing caller-controlled reaches this string — see sanitizeIdentifier.
	_, err = admin.Exec(ctx, `DROP DATABASE IF EXISTS "`+name+`"`)
	require.NoError(t, err)
	_, err = admin.Exec(ctx, `CREATE DATABASE "`+name+`"`)
	require.NoError(t, err)

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		conn, err := pgx.Connect(cleanupCtx, adminDSN)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(cleanupCtx) }()
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

// sanitizeIdentifier strips everything that is not a lowercase letter, digit, or underscore.
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
		panic("test setup: unparseable admin DSN: " + err.Error())
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, name)
}
