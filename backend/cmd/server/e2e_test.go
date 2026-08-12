package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/Alexnex31/Norite/backend/internal/platform/database"
)

// This file is the direct demonstration of Milestone M1's "done when" criteria. It builds the real
// binary and runs it as a separate process against a real Postgres, rather than calling the packages it
// is made of, because the criteria are about the *binary's* behavior: that it starts, that it blocks on a
// pending migration until that migration completes, that it connects to Postgres, and that /healthz then
// returns 200.
//
// Everything below is skipped by `go test -short`, which needs no container runtime.

// TestServerStartupBlocksOnMigrationThenReportsHealthy walks the whole startup sequence.
func TestServerStartupBlocksOnMigrationThenReportsHealthy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in -short mode")
	}

	ctx := context.Background()
	dsn := startPostgres(t)
	binary := buildServer(t)
	addr := reserveLocalAddr(t)
	healthzURL := "http://" + addr + "/api/v1/healthz"

	// Stand in for a second instance already mid-migration, by holding the migration advisory lock.
	// While it is held, the server under test must not report itself ready.
	holder, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer func() { _ = holder.Close(context.Background()) }()

	var acquired bool
	require.NoError(t, holder.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", database.MigrationAdvisoryLockKey).Scan(&acquired))
	require.True(t, acquired)

	server := startServer(t, binary, addr, dsn)

	// The endpoint has to be *reachable* — a booting instance answers, it just answers "not yet". That
	// is the whole reason startup listens before migrating: a probe can tell "still starting" from
	// "crashed", which connection-refused cannot.
	waitFor(t, 30*time.Second, "the server to start listening", func() bool {
		status, _, err := getHealth(healthzURL)
		return err == nil && status != 0
	})

	// Hold the lock for a while and confirm the answer stays "starting" the entire time.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, body, err := getHealth(healthzURL)
		require.NoError(t, err)
		require.Equal(t, http.StatusServiceUnavailable, status, "must not report ready while migrations are blocked")
		require.Equal(t, "starting", body.Status)
		time.Sleep(200 * time.Millisecond)
	}

	// Nothing may have been migrated yet either — "not ready" has to mean the schema really is untouched.
	assert.False(t, schemaMigrationsTableExists(t, dsn),
		"the blocked instance must not have applied a migration")

	// Release the lock: the first "instance" has finished.
	_, err = holder.Exec(ctx, "SELECT pg_advisory_unlock($1)", database.MigrationAdvisoryLockKey)
	require.NoError(t, err)

	waitFor(t, 60*time.Second, "/healthz to return 200", func() bool {
		status, body, err := getHealth(healthzURL)
		return err == nil && status == http.StatusOK && body.Status == "ok"
	})

	// The schema is migrated by the time readiness flips — that ordering is the guarantee.
	assert.True(t, schemaMigrationsTableExists(t, dsn))

	// Spot-check the cross-cutting middleware on a real response, not just in unit tests.
	resp, err := http.Get(healthzURL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	assert.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))
	assert.NotEmpty(t, resp.Header.Get("X-Request-Id"))

	// SIGTERM must drain and exit cleanly, not be ignored until something sends SIGKILL.
	require.NoError(t, server.Process.Signal(syscall.SIGTERM))
	select {
	case err := <-server.wait:
		assert.NoError(t, err, "the server should exit zero on SIGTERM; output:\n%s", server.output())
	case <-time.After(30 * time.Second):
		t.Fatalf("the server did not shut down on SIGTERM; output:\n%s", server.output())
	}
}

// TestServerFailsFastWhenPostgresIsUnreachable covers the other half of "connects to Postgres": a server
// that cannot must exit rather than sit there serving nothing.
// It needs no container — an unreachable address is the whole point — so it runs in -short too.
func TestServerFailsFastWhenPostgresIsUnreachable(t *testing.T) {
	binary := buildServer(t)
	addr := reserveLocalAddr(t)

	server := startServer(t, binary, addr, "postgres://norite:norite@127.0.0.1:1/norite?sslmode=disable")

	select {
	case err := <-server.wait:
		require.Error(t, err, "the server must exit non-zero when it cannot reach Postgres")
		assert.Contains(t, server.output(), "could not connect to Postgres")
	case <-time.After(60 * time.Second):
		t.Fatalf("the server neither started nor exited; output:\n%s", server.output())
	}
}

// TestMigrateOnlyAppliesMigrationsAndExits covers the mode `just db-migrate` uses, and that the flagship's
// Helm pre-upgrade Job will use.
func TestMigrateOnlyAppliesMigrationsAndExits(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in -short mode")
	}

	dsn := startPostgres(t)
	binary := buildServer(t)

	cmd := exec.Command(binary, "-migrate-only")
	cmd.Env = append(os.Environ(),
		"NORITE_DATABASE_URL="+dsn,
		// Required to boot from M4 on, exactly like the DSN.
		"NORITE_JWT_SECRET=test-signing-key-of-at-least-32-bytes",
		"NORITE_LOG_FORMAT=json",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "output:\n%s", out)

	assert.True(t, schemaMigrationsTableExists(t, dsn))
	assert.Contains(t, string(out), "running migrations only")
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type runningServer struct {
	*exec.Cmd
	wait chan error
	logs *syncBuffer
}

func (s *runningServer) output() string { return s.logs.String() }

// startServer launches the binary and arranges for it to be killed when the test ends.
func startServer(t *testing.T, binary, addr, dsn string) *runningServer {
	t.Helper()

	logs := &syncBuffer{}
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(),
		"NORITE_LISTEN_ADDR="+addr,
		"NORITE_DATABASE_URL="+dsn,
		// Required to boot from M4 on, exactly like the DSN.
		"NORITE_JWT_SECRET=test-signing-key-of-at-least-32-bytes",
		"NORITE_LOG_FORMAT=json",
		"NORITE_LOG_LEVEL=debug",
		// Short enough that a hung test fails in reasonable time, long enough not to trip over a slow
		// container in CI.
		"NORITE_MIGRATE_LOCK_TIMEOUT=90s",
	)
	cmd.Stdout = logs
	cmd.Stderr = logs

	require.NoError(t, cmd.Start())

	// Closed after the single send, so a test that already consumed the exit status (the SIGTERM case)
	// and the cleanup below can both receive without one of them blocking forever.
	wait := make(chan error, 1)
	go func() {
		wait <- cmd.Wait()
		close(wait)
	}()

	t.Cleanup(func() {
		if cmd.Process == nil {
			return
		}
		_ = cmd.Process.Kill()
		select {
		case <-wait:
		case <-time.After(15 * time.Second):
			t.Errorf("server process did not exit after being killed")
		}
	})

	return &runningServer{Cmd: cmd, wait: wait, logs: logs}
}

// buildServer compiles the binary under test once per test binary invocation.
var serverBinary struct {
	path string
	err  error
	done bool
}

func buildServer(t *testing.T) string {
	t.Helper()

	if !serverBinary.done {
		serverBinary.done = true
		// Not t.TempDir(): the binary is shared across tests, so it must outlive whichever test built it.
		dir, err := os.MkdirTemp("", "norite-server-build")
		if err != nil {
			serverBinary.err = err
		} else {
			path := filepath.Join(dir, "norite-server")
			out, buildErr := exec.Command("go", "build", "-o", path, ".").CombinedOutput()
			if buildErr != nil {
				serverBinary.err = fmt.Errorf("go build failed: %w\n%s", buildErr, out)
			} else {
				serverBinary.path = path
			}
		}
	}

	require.NoError(t, serverBinary.err)
	return serverBinary.path
}

// startPostgres brings up a container for one test and returns its DSN.
func startPostgres(t *testing.T) string {
	t.Helper()

	ctx := context.Background()
	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("norite"),
		postgres.WithUsername("norite"),
		postgres.WithPassword("norite"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("could not terminate Postgres container: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	return dsn
}

// reserveLocalAddr picks a loopback address that is free right now.
//
// There is an unavoidable gap between closing this listener and the server binding it. Binding port 0 in
// the server itself would close the gap but leave the test with no way to learn the chosen port, so this
// is the lesser problem.
func reserveLocalAddr(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

func getHealth(url string) (int, healthResponse, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0, healthResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	var body healthResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return resp.StatusCode, healthResponse{}, err
	}
	return resp.StatusCode, body, nil
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

func schemaMigrationsTableExists(t *testing.T, dsn string) bool {
	t.Helper()

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()

	var exists bool
	require.NoError(t, conn.QueryRow(ctx, `SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&exists))
	return exists
}

// syncBuffer collects subprocess output safely across the goroutine writing it and the test reading it.
type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
