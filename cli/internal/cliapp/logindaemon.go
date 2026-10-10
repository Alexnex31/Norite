// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package cliapp

import (
	"context"
	"io"

	"github.com/Alexnex31/Norite/cli/internal/daemonctl"
	"github.com/Alexnex31/Norite/cli/internal/login"
)

// loginDaemon is the daemon's service as `norite login` sees it. It lives here because this package is
// the one that imports both: login knows what it needs of a daemon, daemonctl knows how a service is
// managed, and neither has a reason to know the other.
type loginDaemon struct{ setup daemonSetup }

// daemonSetup is the part of daemonctl.Setup a login needs, as an interface so the mapping below is
// tested without a service manager.
type daemonSetup interface {
	Standing(ctx context.Context) (running, installed bool, err error)
	InstallAndStart(ctx context.Context, out io.Writer) error
	Start(ctx context.Context) error
}

var _ daemonSetup = daemonctl.Setup{}

func (d loginDaemon) State(ctx context.Context) login.DaemonState {
	running, installed, err := d.setup.Standing(ctx)
	switch {
	case err != nil:
		// A platform with no service to write, or a service manager that would not answer. The login
		// says the command to run and claims nothing.
		return login.DaemonUnknown
	case running:
		return login.DaemonRunning
	case installed:
		return login.DaemonStopped
	default:
		return login.DaemonAbsent
	}
}

func (d loginDaemon) Install(ctx context.Context, out io.Writer) error {
	return d.setup.InstallAndStart(ctx, out)
}

func (d loginDaemon) Start(ctx context.Context) error { return d.setup.Start(ctx) }
