// Command server is the Norite backend composition root.
//
// It owns process lifetime and wiring, and nothing else: every piece of behavior lives in an internal
// package, and this file only decides what is constructed, in what order, and how the process starts and
// stops.
//
// Startup order is deliberate (docs/architecture.md §2, "Cross-cutting"):
//
//  1. Configuration is loaded and validated — a bad DSN or rate string fails here, before anything opens
//     a socket.
//  2. The connection pool is opened and verified with a real round trip.
//  3. The listen address is bound (synchronously, so a port conflict fails before anything touches the
//     schema) and the server starts serving, answering /healthz with 503.
//  4. Migrations run to completion, guarded by a Postgres advisory lock.
//  5. Only then does /healthz report 200 and the instance count as ready.
//
// Steps 3 and 4 are in that order on purpose. The requirement is that the instance never *serves* against
// a not-yet-migrated schema, not that it refuse TCP connections while migrating — and a readiness probe
// that gets an explicit "starting" is far more diagnosable than one that gets connection-refused and
// cannot tell a booting instance from a crashed one.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/internal/config"
	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/database"
	"github.com/Alexnex31/Norite/backend/internal/platform/logging"
	"github.com/Alexnex31/Norite/backend/migrations"
)

func main() {
	if err := run(); err != nil {
		// Logging may not be up yet — and if it is, the error was already logged in context. stderr is
		// the one channel guaranteed to exist either way.
		fmt.Fprintf(os.Stderr, "norite: fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	migrateOnly := flag.Bool("migrate-only", false,
		"apply pending migrations and exit, without starting the HTTP server "+
			"(used by `just db-migrate` and, for the flagship, by the Helm pre-upgrade Job)")
	configPath := flag.String("config", "",
		"path to the instance config file written by `norite instance init`; overrides "+
			config.ConfigFileEnvVar+" and the default location. Optional — NORITE_* environment "+
			"variables alone are a fully supported configuration, and they override this file either way")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	logger, err := logging.New(logging.Options{Level: cfg.LogLevel, Format: cfg.LogFormat})
	if err != nil {
		return err
	}

	// Which file (if any) was read is the first thing anyone debugging a surprising setting needs, and it
	// is not otherwise discoverable from outside the process. The path only — never the values, several of
	// which are credentials (CLAUDE.md rule 8).
	if cfg.SourcePath != "" {
		logger.Info().Str("path", cfg.SourcePath).Msg("loaded instance config file")
	} else {
		logger.Info().Msg("no instance config file found — using environment variables and defaults")
	}

	// A canceled context is how SIGINT/SIGTERM reaches everything below, so an operator who asks a stuck
	// startup to stop does not have to send SIGKILL. For migrations that means aborting the wait for the
	// advisory lock and stopping between migrations; a statement already executing runs to completion,
	// because golang-migrate's driver does not accept a context (see database.Migrate).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	migrateOpts := database.MigrateOptions{
		DatabaseURL: cfg.DatabaseURL,
		Source:      migrations.FS,
		SourceDir:   ".",
		LockTimeout: cfg.MigrateLockTimeout,
		Logger:      &logger,
	}

	if *migrateOnly {
		logger.Info().Msg("running migrations only")
		if err := database.Migrate(ctx, migrateOpts); err != nil {
			logger.Error().Err(err).Msg("migrations failed")
			return err
		}
		return nil
	}

	pool, err := database.New(ctx, database.PoolOptions{
		DatabaseURL:    cfg.DatabaseURL,
		MaxConns:       cfg.DBMaxConns,
		MinConns:       cfg.DBMinConns,
		ConnectTimeout: cfg.DBConnectTimeout,
	})
	if err != nil {
		logger.Error().Err(err).Msg("could not connect to Postgres")
		return err
	}
	defer pool.Close()

	logger.Info().
		Int32("max_conns", cfg.DBMaxConns).
		Int32("min_conns", cfg.DBMinConns).
		Msg("connected to Postgres")

	health := newHealth(db.New(pool))

	router, err := newRouter(routerOptions{
		Config: cfg,
		Logger: logger,
		Health: health,
	})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: router,
		// Bound the slow-client attack surface. WriteTimeout is deliberately absent: the WebSocket
		// gateway (Milestone M18) mounts on this same server and holds long-lived connections, which a
		// write deadline would sever. ReadHeaderTimeout is what actually closes the Slowloris hole.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Bind before migrating, and synchronously, so an address already in use — a restart racing its
	// predecessor, a stray dev process — fails here. ListenAndServe would surface that same error only on
	// the goroutine, unread until migrations finish, by which point the process would have written to the
	// production schema and marked itself ready for a server that can never accept a connection.
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		logger.Error().Err(err).Str("addr", cfg.ListenAddr).Msg("could not bind listen address")
		return fmt.Errorf("could not bind %s: %w", cfg.ListenAddr, err)
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info().Str("addr", listener.Addr().String()).Str("env", string(cfg.Env)).Msg("norite backend listening")
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	// Blocking, before the instance reports ready. See the package comment.
	if err := database.Migrate(ctx, migrateOpts); err != nil {
		logger.Error().Err(err).Msg("migrations failed — shutting down without serving")
		shutdown(srv, cfg.ShutdownTimeout, &logger)
		return err
	}

	health.MarkReady()
	logger.Info().Msg("migrations complete — instance is ready")

	select {
	case err := <-serverErr:
		if err != nil {
			logger.Error().Err(err).Msg("http server failed")
			return err
		}
	case <-ctx.Done():
		logger.Info().Msg("shutdown signal received")
	}

	// Stop reporting ready before draining, so a load balancer stops sending new work while in-flight
	// requests finish.
	health.MarkStopping()
	shutdown(srv, cfg.ShutdownTimeout, &logger)
	logger.Info().Msg("norite backend stopped")
	return nil
}

// shutdown drains in-flight requests, giving up after timeout.
func shutdown(srv *http.Server, timeout time.Duration, logger *zerolog.Logger) {
	// Detached from the signal context, which is already canceled by the time we get here.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logger.Error().Err(err).Dur("timeout", timeout).Msg("graceful shutdown did not finish in time")
	}
}
