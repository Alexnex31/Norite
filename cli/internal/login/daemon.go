// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package login

import (
	"context"
	"io"
	"strings"

	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// DaemonState is how the background daemon stands when a sign-in finishes.
type DaemonState int

const (
	// DaemonUnknown: it could not be found out, or this platform has no service this program can write.
	DaemonUnknown DaemonState = iota
	// DaemonRunning: one is running for this account. It watches the credential store, so it picks the
	// sign-in up without being told.
	DaemonRunning
	// DaemonStopped: one is installed as a service and not running.
	DaemonStopped
	// DaemonAbsent: none is running and none is installed.
	DaemonAbsent
)

// Daemon is what a sign-in needs of the daemon's service: how it stands, and the two ways to bring it up.
// cli/internal/daemonctl implements it; this package does not import that one, so a login is testable
// without a service manager.
type Daemon interface {
	State(ctx context.Context) DaemonState
	// Install registers the daemon to start at login and starts it now, writing what it did to out.
	Install(ctx context.Context, out io.Writer) error
	// Start starts an installed daemon.
	Start(ctx context.Context) error
}

// daemonAfterSignIn is the end of a login: the credential is stored, and nothing is connected until a
// daemon reads it.
//
// This is what "the daemon auto-installs" means (M23): it is offered here, at the moment somebody has a
// reason to want it, and never done unasked. A daemon is a program that starts at every login from then
// on, and installing one is a person's decision. With nobody to ask, the commands are printed.
//
// Nothing here fails the login. The sign-in is stored and good whatever happens to the offer.
func (r *Runner) daemonAfterSignIn(ctx context.Context) {
	const byHand = "Start the background daemon with `norite daemon start` if it is not running already.\n"
	if r.Daemon == nil {
		r.printf(byHand)
		return
	}
	switch r.Daemon.State(ctx) {
	case DaemonRunning:
		r.printf("The daemon is running and picks this sign-in up by itself.\n")

	case DaemonStopped:
		if !r.Interactive || !r.yes("The daemon is installed and not running. Start it now? [Y/n] ", true) {
			r.printf("Start the daemon with `norite daemon start`.\n")
			return
		}
		if err := r.Daemon.Start(ctx); err != nil {
			r.printf("The daemon could not be started: %s\n", termsafe.Text(err.Error()))
			return
		}
		r.printf("The daemon is running.\n")

	case DaemonAbsent:
		const commands = "When you want it: `norite daemon install`, then `norite daemon start`.\n"
		if !r.Interactive {
			r.printf("No daemon is installed, and it is what keeps you connected. " + commands)
			return
		}
		r.printf("\nNo daemon is installed on this machine. It is the program that keeps you connected,\n" +
			"and once installed it starts each time you log in to this computer.\n")
		if !r.yes("Install and start it now? [y/N] ", false) {
			r.printf(commands)
			return
		}
		if err := r.Daemon.Install(ctx, r.Out); err != nil {
			// The service manager's own words, which name commands and paths.
			r.printf("The daemon could not be installed: %s\n", termsafe.Text(err.Error()))
			r.printf(commands)
		}

	default:
		r.printf(byHand)
	}
}

// yes asks a question whose answer does not decide whether the login succeeded. An empty answer is
// byDefault, and a read that fails is no.
func (r *Runner) yes(question string, byDefault bool) bool {
	if r.ReadLine == nil {
		return false
	}
	line, err := r.ReadLine(question)
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return byDefault
	case "y", "yes":
		return true
	}
	return false
}
