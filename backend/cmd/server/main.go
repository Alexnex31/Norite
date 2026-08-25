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

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/config"
	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/mail"
	"github.com/Alexnex31/Norite/backend/internal/platform/database"
	"github.com/Alexnex31/Norite/backend/internal/platform/logging"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
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

	// Node 0: the ID scheme reserves 10 bits for a node identifier so a future multi-node deployment needs
	// only a config value rather than an ID migration (ADR 0003). A single-process monolith has exactly one.
	ids, err := snowflake.NewGenerator(0)
	if err != nil {
		logger.Error().Err(err).Msg("could not initialize the ID generator")
		return err
	}

	issuer, err := auth.NewTokenIssuer([]byte(cfg.JWTSecret))
	if err != nil {
		// The only failure is a key below the length floor, and the message must not echo the key itself.
		logger.Error().Err(err).Msg("the configured JWT signing key is unusable")
		return err
	}

	// The mail queue exists whether or not SMTP is configured: with no relay it reports itself disabled
	// and refuses politely, so nothing downstream has to nil-check it (ADR 0020 — an instance without a
	// relay is a working instance, with password reset simply unavailable).
	mailer, err := newMailQueue(cfg, logger)
	if err != nil {
		logger.Error().Err(err).Msg("the configured SMTP relay is unusable")
		return err
	}
	defer func() {
		// Drained after the HTTP server has stopped, so nothing new is queued while this runs. Bounded by
		// the same shutdown timeout as everything else: a wedged relay must not hold the process open.
		ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := mailer.Shutdown(ctx); err != nil {
			logger.Warn().Err(err).Msg("mail queue did not drain before the shutdown deadline")
		}
	}()

	// A provider with no credentials is simply absent from this set, so an instance that configured none
	// offers no OAuth sign-in and every entry point reports an unknown provider.
	oauthProviders := auth.NewOAuthProviders(auth.OAuthOptions{
		PublicBaseURL:      cfg.PublicBaseURL,
		GoogleClientID:     cfg.GoogleClientID,
		GoogleClientSecret: cfg.GoogleClientSecret,
		GitHubClientID:     cfg.GitHubClientID,
		GitHubClientSecret: cfg.GitHubClientSecret,
	})
	if names := oauthProviders.Names(); len(names) > 0 {
		// Names only. A client ID is not secret but is not useful in a log either, and the secret beside
		// it must never appear in one (CLAUDE.md rule 8).
		logger.Info().Strs("providers", names).Msg("oauth sign-in enabled")
	}

	authService, err := auth.NewService(auth.ServiceOptions{
		Pool:             pool,
		IDs:              ids,
		Issuer:           issuer,
		RegistrationMode: auth.RegistrationMode(cfg.RegistrationMode),
		Mailer:           mailer,
		PublicBaseURL:    cfg.PublicBaseURL,
		OAuth:            oauthProviders,
	})
	if err != nil {
		logger.Error().Err(err).Msg("could not initialize the auth service")
		return err
	}

	router, err := newRouter(routerOptions{
		Config:  cfg,
		Logger:  logger,
		Health:  health,
		Auth:    auth.NewHandler(authService),
		AuthSvc: authService,
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

	// Said once, at startup, because it is a security posture rather than a missing feature and nothing
	// else will mention it again. Without a relay an address cannot be confirmed, so accounts are
	// usable on creation and registration cannot hide whether an address is already taken — see
	// auth.VerificationRequired, which is the one place that decides this.
	if !authService.VerificationRequired() {
		logger.Warn().Msg("no SMTP relay: new accounts skip address confirmation, and registration " +
			"cannot hide whether an address already has an account")
	}

	// Started after migrations, because it deletes from tables migrations may have just created, and after
	// readiness, because nothing waits on it. It stops when ctx is canceled by the signal handler, so a
	// shutdown never waits on a sweep interval; a sweep already in flight is canceled with its query and
	// the next process picks the rows up.
	sweeperDone := make(chan struct{})
	go func() {
		defer close(sweeperDone)
		authService.RunSweeper(logging.WithContext(ctx, logger))
	}()

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

	// The sweeper observes the same canceled context, so this is a join rather than a wait. Joined at all
	// so the process does not exit with a DELETE still in flight against a pool that is about to close.
	<-sweeperDone

	logger.Info().Msg("norite backend stopped")
	return nil
}

// newMailQueue builds the outbound mail queue from configuration.
//
// A disabled queue is returned as a real object rather than nil, so callers ask Enabled() instead of
// nil-checking a dependency — the difference between "this instance cannot send mail" and "someone forgot
// to wire the mailer" stays visible.
func newMailQueue(cfg config.Config, logger zerolog.Logger) (*mail.Queue, error) {
	if !cfg.SMTPEnabled {
		logger.Info().Msg("SMTP is not configured — password reset is unavailable on this instance")
		return mail.NewQueue(mail.Options{Logger: logger}), nil
	}

	sender, err := mail.NewSMTPSender(mail.SMTPOptions{
		Host:        cfg.SMTPHost,
		Port:        cfg.SMTPPort,
		Username:    cfg.SMTPUsername,
		Password:    cfg.SMTPPassword,
		Encryption:  mail.Encryption(cfg.SMTPEncryption),
		FromAddress: cfg.SMTPFromAddress,
		FromName:    cfg.SMTPFromName,
	})
	if err != nil {
		return nil, err
	}

	// Host, port and mode only. The username is an identity and the password is a credential, and neither
	// belongs in a log line (CLAUDE.md rule 8).
	logger.Info().
		Str("host", cfg.SMTPHost).
		Int("port", cfg.SMTPPort).
		Str("encryption", cfg.SMTPEncryption).
		Msg("outbound email enabled")

	return mail.NewQueue(mail.Options{Sender: sender, Logger: logger}), nil
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
