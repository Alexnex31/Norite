// Command norite is the Norite CLI.
//
// This file is deliberately thin: it owns process lifetime and nothing else. The command tree lives in
// internal/cliapp so it can be built and exercised without spawning a process.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	"github.com/Alexnex31/Norite/cli/internal/cliapp"
	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/termsafe"
)

func main() {
	// Commands that can honor cancellation take it from here.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// A context is not enough on its own, though. The wizard blocks on terminal reads that no context can
	// interrupt, so without the handler below Ctrl-C would be captured, cancel a context nobody is
	// watching, and leave the process sitting there unkillable by ordinary means.
	//
	// Exiting has to put the terminal back first: a password prompt turns echo off, and dying mid-prompt
	// would leave the operator's shell silently swallowing everything they type.
	restoreTerminal := captureTerminalState(os.Stdin)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-signals
		restoreTerminal()
		fmt.Fprintln(os.Stderr)
		os.Exit(exitCodeFor(sig))
	}()

	if err := cliapp.New(os.Stdout, os.Stderr).Run(ctx, os.Args); err != nil {
		code, message := report(err)
		if message != "" {
			fmt.Fprintf(os.Stderr, "%s\n", message)
		}
		os.Exit(code)
	}
}

// report decides how a failed run is announced: the exit code, and the line to print on stderr (empty for
// nothing at all).
//
// Split out of main so it can be tested. It could not be before, and the cost of that showed up at M10:
// the terminal branch below was a list of the packages that declared their own sentinel, `norite instance
// bootstrap` was missing from it, and nothing failed — the command simply exited 1 with the prefix that
// makes a usage problem read like a crash, which only a manual run noticed.
func report(err error) (code int, message string) {
	// A command that needs a terminal and hasn't got one is a usage problem, not a crash: say what to do
	// about it without the "norite:" prefix. One sentinel, matched once — see clierr for why it is one.
	if errors.Is(err, clierr.ErrNoTerminal) {
		return 2, errorText(err)
	}

	// A command that reports its result through the exit code rather than through output —
	// `norite daemon status` is the first — returns a cli.ExitCoder. cliapp disables urfave/cli's own
	// handling of these precisely so the decision lands here. An empty message means the code *is* the
	// message and there is nothing to print: a status that exits 1 has already said, on stdout, that
	// the daemon is stopped, and "norite: " in front of nothing would be noise.
	var exit cli.ExitCoder
	if errors.As(err, &exit) {
		if msg := errorText(exit); msg != "" {
			return exit.ExitCode(), "norite: " + msg
		}
		return exit.ExitCode(), ""
	}

	return 1, "norite: " + errorText(err)
}

// errorText renders an error for a terminal.
//
// The backstop for CLAUDE.md rule 19, applied where every command's failure converges rather than inside
// each command. An error message is assembled from whatever the failure had to hand — a server's response,
// a filename, a tool's output — and the sanitizing that happens at those boundaries is only as complete as
// the last person to add one remembered to make it. This costs nothing on text that is already clean, and
// means no future command can put an escape sequence on a terminal by forgetting.
//
// Block rather than Text: some errors are deliberately multi-line, and a newline in one is the program's
// own, not a stranger's — the values that arrive from outside are sanitized to a single line before they
// are ever formatted into it.
func errorText(err error) string { return termsafe.Block(err.Error()) }

// captureTerminalState snapshots the terminal's settings, returning a function that restores them. Both
// are no-ops when the stream is not a terminal, which is every piped and scripted run.
func captureTerminalState(in *os.File) func() {
	fd := int(in.Fd())
	if !term.IsTerminal(fd) {
		return func() {}
	}
	state, err := term.GetState(fd)
	if err != nil {
		return func() {}
	}
	return func() { _ = term.Restore(fd, state) }
}

// exitCodeFor follows the shell convention of 128 plus the signal number, so a caller can tell an
// interrupted run (130) from a failed one (1).
func exitCodeFor(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 1
}
