// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonctl

import (
	"context"
	"io"

	"github.com/Alexnex31/Norite/cli/internal/output"
)

// Setup brings the daemon up on behalf of a command that is not one of this group's: `norite login`, which
// ends by offering to install it (M23). It is the same Manager and the same steps as `norite daemon
// install` and `start`, so there is one way a service gets registered.
type Setup struct {
	// Version is this CLI's, for attaching to a daemon to ask whether one is there.
	Version string
}

// Standing reports whether a daemon is running for this account, however it was started, and whether one
// is installed as a service. An error is a platform with no service this program knows how to write, or a
// service manager that could not be asked.
func (s Setup) Standing(ctx context.Context) (running, installed bool, err error) {
	mgr, err := managerFor()
	if err != nil {
		return false, false, err
	}
	state, err := mgr.Status(ctx)
	if err != nil {
		return false, false, err
	}
	view := viewOfStatus(state, socketStatus(ctx, s.Version))
	return view.Running, view.Installed, nil
}

// InstallAndStart registers the daemon to start at login and starts it now, writing what it did to out.
func (s Setup) InstallAndStart(ctx context.Context, out io.Writer) error {
	mgr, err := managerFor()
	if err != nil {
		return err
	}
	binary, err := LocateDaemon("")
	if err != nil {
		return err
	}
	if err := mgr.Install(ctx, binary); err != nil {
		return err
	}
	// launchd starts the agent as part of loading it; the others register and wait to be told.
	if !mgr.StartsOnInstall() {
		if err := mgr.Start(ctx); err != nil {
			return err
		}
	}
	fprintf(out, "Installed and started %s. It starts by itself when you log in.\n", ServiceName)
	// Which program was registered, as `norite daemon install` says: it was found by looking, in the
	// environment, beside this binary and on PATH, and it now runs at every login holding the sign-in.
	// Somebody who said yes to a question should be shown what the yes registered.
	// Both are the environment's text, and a path may hold anything (rule 19).
	fprintf(out, "  executable: %s\n", output.Clean(binary))
	if path, err := mgr.DefinitionPath(); err == nil && path != "" {
		fprintf(out, "  definition: %s\n", output.Clean(path))
	}
	if lingerer, ok := mgr.(Lingerer); ok {
		// What happens at logout, which on a machine reached over SSH is the next thing to happen.
		return reportLinger(ctx, out, lingerer, false)
	}
	return nil
}

// Start starts an installed daemon.
func (s Setup) Start(ctx context.Context) error {
	mgr, err := managerFor()
	if err != nil {
		return err
	}
	return mgr.Start(ctx)
}
