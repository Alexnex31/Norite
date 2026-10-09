// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package cliapp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/Alexnex31/Norite/cli/internal/login"
)

type fakeSetup struct {
	running, installed bool
	err                error
	installs, starts   int
}

func (f *fakeSetup) Standing(context.Context) (bool, bool, error) {
	return f.running, f.installed, f.err
}
func (f *fakeSetup) InstallAndStart(_ context.Context, out io.Writer) error {
	f.installs++
	_, _ = io.WriteString(out, "installed")
	return nil
}
func (f *fakeSetup) Start(context.Context) error { f.starts++; return nil }

// What a login is told decides what it offers, so each answer of the service's has exactly one meaning.
func TestWhatALoginIsToldAboutTheDaemon(t *testing.T) {
	cases := []struct {
		name  string
		setup fakeSetup
		want  login.DaemonState
	}{
		{"running under the service", fakeSetup{running: true, installed: true}, login.DaemonRunning},
		// Not "absent": offering to install over a daemon that is serving clients would start a second.
		{"running, started by hand", fakeSetup{running: true}, login.DaemonRunning},
		{"installed and stopped", fakeSetup{installed: true}, login.DaemonStopped},
		{"neither", fakeSetup{}, login.DaemonAbsent},
		// Not "absent" either: where the service manager cannot be asked, nothing is offered.
		{"could not be asked", fakeSetup{err: errors.New("no service manager")}, login.DaemonUnknown},
		{"could not be asked, whatever else it said", fakeSetup{running: true, installed: true,
			err: errors.New("no service manager")}, login.DaemonUnknown},
	}
	for _, tc := range cases {
		setup := tc.setup
		if got := (loginDaemon{setup: &setup}).State(t.Context()); got != tc.want {
			t.Errorf("%s: state %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestALoginsInstallAndStartAreTheServices(t *testing.T) {
	setup := &fakeSetup{}
	d := loginDaemon{setup: setup}
	var out bytes.Buffer
	if err := d.Install(t.Context(), &out); err != nil || setup.installs != 1 || out.String() != "installed" {
		t.Errorf("install: %v, %d, %q", err, setup.installs, out.String())
	}
	if err := d.Start(t.Context()); err != nil || setup.starts != 1 {
		t.Errorf("start: %v, %d", err, setup.starts)
	}
}
