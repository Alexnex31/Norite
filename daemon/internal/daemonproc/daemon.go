// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package daemonproc is the Norite background daemon's lifecycle.
//
// Milestone M3 scope is deliberately narrow: start cleanly, prove there is exactly one daemon per OS user,
// prepare the process for the handle count it will eventually hold, log what it did, and stop cleanly on a
// signal. It opens no sockets and talks to nothing — the gateway client and the dual IPC listeners arrive
// in Phase D (docs/roadmap.md M19-M22), and the plugin host at M88.
//
// What it is not is a placeholder to be thrown away. Every later milestone adds a component *inside* this
// startup and shutdown sequence, so the ordering it establishes — lock before anything observable, limits
// raised before the first handle, shutdown in reverse — is the part meant to survive.
//
// See docs/architecture.md §3 and ADR 0010.
package daemonproc

import (
	"context"
	"io"
	"os"
	"sync"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/daemon/credentials"
	"github.com/Alexnex31/Norite/daemon/internal/paths"
	"github.com/Alexnex31/Norite/daemon/internal/session"
)

// Options configures a daemon run.
type Options struct {
	// StateDir overrides where the lock and log live. Empty means the per-user default (paths.StateDir).
	// Tests set it; nothing in production does.
	StateDir string

	// LogFile overrides the rotating log's path. Empty means <StateDir>/daemon.log.
	//
	// The launchd backend sets this, so that on macOS the daemon's log lands in ~/Library/Logs where the
	// platform's users and Console.app look for it, rather than somewhere only Norite knows about. The lock
	// is not affected — it stays in the state directory, which is what makes it a reliable per-user
	// rendezvous point regardless of where logs were pointed.
	LogFile string

	// Version is reported in the startup log, so a support question about behavior can be tied to a build.
	Version string

	// LogLevel filters the structured log. Note the zero value is zerolog.DebugLevel, not Info — callers
	// state the level they want rather than relying on the zero value to be sensible.
	LogLevel zerolog.Level

	// Stderr, when non-nil, receives a copy of every log line in addition to the log file.
	//
	// This is what makes `systemctl --user status norite-daemon` and `launchctl print` show something
	// useful: both capture the process's stderr, and an operator debugging a daemon that will not start
	// reaches for those before they know a log file exists. The file remains the durable, rotated copy.
	Stderr io.Writer

	// Ready, when non-nil, is called once the daemon is fully started and about to begin waiting.
	//
	// A test hook. Without it a test would have to poll for a log line or sleep, and a sleep long enough to
	// be reliable on a loaded CI machine is long enough to make the suite unpleasant.
	Ready func()

	// SkipSession stops the daemon reaching for a stored credential at startup.
	//
	// For the tests that are about lifecycle rather than authentication: without it, every one of them
	// would touch the real machine's keyring on a developer's laptop and hit a D-Bus timeout on a CI box.
	// Nothing in production sets it — a daemon that never establishes a session has no reason to exist.
	SkipSession bool
}

// Run starts the daemon and blocks until ctx is canceled.
//
// Returns nil on a clean, signal-initiated stop — that is the expected way for this process to end, so it
// must not look like a failure to a service manager that would count a non-zero exit as a crash and restart
// it. ErrAlreadyRunning is returned, unwrapped, when another daemon holds this user's lock.
func Run(ctx context.Context, opts Options) error {
	stateDir := opts.StateDir
	if stateDir == "" {
		resolved, err := paths.StateDir()
		if err != nil {
			return err
		}
		stateDir = resolved
	}

	// The lock comes first, before the log file is opened and before any limit is touched. Two daemons
	// racing to start must not both write startup lines into one log, and the loser must do nothing at all
	// beyond failing — a second process that has already begun changing shared state before discovering it
	// is the second process is exactly what the lock exists to prevent.
	lock, err := acquireInstanceLock(paths.LockFile(stateDir))
	if err != nil {
		return err
	}
	defer func() { _ = lock.release() }()

	logPath := opts.LogFile
	if logPath == "" {
		logPath = paths.LogFile(stateDir)
	}
	logFile := newLogWriter(logPath)
	defer func() { _ = logFile.Close() }()

	var sink io.Writer = logFile
	if opts.Stderr != nil {
		sink = zerolog.MultiLevelWriter(logFile, opts.Stderr)
	}
	log := newLogger(sink, opts.LogLevel)

	log.Info().
		Int("pid", os.Getpid()).
		Str("version", opts.Version).
		Str("state_dir", stateDir).
		Str("log_file", logPath).
		Msg("daemon starting")

	// Raised before the first handle is opened, which is the whole point of doing it here rather than
	// lazily. A failure is logged and survived rather than returned — see raiseFileLimit.
	if limit, err := raiseFileLimit(); err != nil {
		log.Warn().Err(err).Msg("could not raise the open-file limit; continuing at the inherited limit")
	} else if limit > 0 {
		log.Debug().Uint64("open_file_limit", limit).Msg("open-file limit set")
	}

	// The session runs for the daemon's whole life, in its own goroutine, and "ready" does not wait for it.
	// M7 signed in before reporting ready so an attach client would find the session established; that
	// stopped being possible to promise at M19, when signing in became something that can take indefinitely
	// (a keyring that has not unlocked, an instance that is down) and can end at any moment (a revocation, a
	// logout). A client has to handle "not signed in" whenever it attaches, so it may as well at startup.
	var components sync.WaitGroup
	if opts.SkipSession {
		log.Debug().Msg("session establishment skipped")
	} else {
		store, err := credentials.OpenIn(stateDir)
		if err != nil {
			log.Error().Err(err).Msg("the credential store could not be opened")
		} else {
			// The store's own account of anything it could not finish — a credential left in a backend this
			// process cannot reach, most likely. Nobody is watching a daemon's terminal, so it goes to the
			// log at a level that gets read.
			store.Notify = func(msg string) { log.Warn().Msg(msg) }
			src := session.New(session.Options{Store: store, Log: log})
			components.Go(func() { src.Run(ctx) })
		}
	}

	log.Info().Msg("daemon ready")
	if opts.Ready != nil {
		opts.Ready()
	}

	<-ctx.Done()

	// Shutdown is the reverse of startup. Each component stops on the same cancellation and is waited for
	// here, before the log and the lock are released: a session mid-write to the credential store must
	// finish before a second daemon can take the lock and read it.
	log.Info().Msg("daemon stopping")
	components.Wait()
	log.Info().Msg("daemon stopped")
	return nil
}
