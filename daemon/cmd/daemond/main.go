// Command daemond is the Norite background daemon.
//
// One process per OS user account, normally started by that user's service manager (systemd user unit,
// launchd agent, or Windows scheduled task) rather than by hand — `norite daemon install` writes the
// definition. Running it directly in a terminal is supported and is the easiest way to watch it start.
//
// This file owns process lifetime and exit codes and nothing else; the daemon itself lives in
// internal/daemonproc. See docs/architecture.md §3 and ADR 0010.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/daemon/internal/daemonproc"
)

// Version is the build's version string, overridden at link time by goreleaser.
var Version = "dev"

// Exit codes. Distinguished so a service manager and a human get different signals from the same event:
// "already running" is a normal, expected outcome that should not be reported as a crash.
// A signal-initiated stop deliberately exits 0, not 128+signum: a daemon asked to stop by its service
// manager has succeeded, and reporting 143 would make systemd count every ordinary stop as a failure and
// restart it.
const (
	exitFailure   = 1
	exitAlreadyUp = 3
)

func main() {
	var (
		debug       = flag.Bool("debug", false, "log at debug level")
		showVersion = flag.Bool("version", false, "print the version and exit")
		logFile     = flag.String("log-file", "", "write the rotating log here instead of the default in the state directory")
		// On by default: run in a terminal and you expect to see output, and journald captures stderr, which
		// is what makes `systemctl --user status` useful. The launchd backend turns it off, because there the
		// service manager writes stderr to a plain file it never rotates — mirroring into it would duplicate
		// the rotated log into an unbounded one.
		stderrLog = flag.Bool("stderr-log", true, "also write log lines to stderr")
	)
	flag.Parse()

	if *showVersion {
		// The write error is unreportable — there is nowhere left to report it to — and irrelevant: the
		// caller who closed the pipe already stopped reading.
		_, _ = fmt.Fprintln(os.Stdout, Version)
		return
	}

	// NotifyContext rather than a signal channel: the whole shutdown path is a context cancellation
	// already, so this makes the signal indistinguishable from any other reason to stop. stop() restores
	// the default disposition, so a second SIGTERM arriving during a hung shutdown kills the process
	// outright instead of being swallowed — the operator pressing Ctrl-C twice must always be obeyed.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	level := zerolog.InfoLevel
	if *debug {
		level = zerolog.DebugLevel
	}

	var stderr io.Writer
	if *stderrLog {
		stderr = os.Stderr
	}

	err := daemonproc.Run(ctx, daemonproc.Options{
		Version:  Version,
		LogLevel: level,
		LogFile:  *logFile,
		Stderr:   stderr,
	})

	switch {
	case err == nil:
		return
	case errors.Is(err, daemonproc.ErrAlreadyRunning):
		// Not a failure worth alarming anyone over — say so plainly and exit with a code the caller can
		// tell apart from a real problem. `norite daemon status` is the way to see the running one.
		fmt.Fprintln(os.Stderr, "norite-daemon: already running for this user; nothing to do")
		os.Exit(exitAlreadyUp)
	default:
		fmt.Fprintf(os.Stderr, "norite-daemon: %v\n", err)
		os.Exit(exitFailure)
	}
}
