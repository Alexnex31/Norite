// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonctl

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// withManager makes mgr the Manager every command and Setup uses, for the rest of the test.
func withManager(t *testing.T, mgr Manager, err error) {
	t.Helper()
	previous := managerFor
	managerFor = func() (Manager, error) { return mgr, err }
	t.Cleanup(func() { managerFor = previous })
}

func TestSetupReportsADaemonHoweverItWasStarted(t *testing.T) {
	cases := []struct {
		name               string
		state              State
		socket             socketAnswer
		running, installed bool
	}{
		{"the service, running", State{Installed: true, Running: true}, socketAnswer{Running: true, Attached: true}, true, true},
		{"started by hand", State{}, socketAnswer{Running: true, Attached: true}, true, false},
		{"installed and stopped", State{Installed: true}, socketAnswer{}, false, true},
		{"neither", State{}, socketAnswer{}, false, false},
	}
	for _, tc := range cases {
		withManager(t, &stubManager{state: tc.state}, nil)
		answerStatus(t, tc.socket)
		running, installed, err := Setup{Version: "test"}.Standing(t.Context())
		if err != nil || running != tc.running || installed != tc.installed {
			t.Errorf("%s: running=%v installed=%v err=%v", tc.name, running, installed, err)
		}
	}

	// A platform this program cannot write a service for is an error, not "nothing installed": the caller
	// must not offer to install where it cannot.
	withManager(t, nil, ErrUnsupportedPlatform)
	if _, _, err := (Setup{}).Standing(t.Context()); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Errorf("an unsupported platform: %v", err)
	}
}

func TestSetupInstallsAndStartsAsThePlatformNeeds(t *testing.T) {
	t.Setenv(DaemonBinaryEnvVar, writeExecutable(t, t.TempDir(), daemonBinaryName))

	// systemd and Task Scheduler register and wait to be told.
	mgr := &stubManager{}
	withManager(t, mgr, nil)
	var out bytes.Buffer
	if err := (Setup{}).InstallAndStart(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if len(mgr.installed) != 1 || mgr.starts != 1 || !strings.Contains(out.String(), "Installed and started") {
		t.Errorf("installed=%v starts=%d output=%q", mgr.installed, mgr.starts, out.String())
	}

	// launchd starts the agent by loading it, and a second start is not sent.
	mgr = &stubManager{startsOnInstall: true}
	withManager(t, mgr, nil)
	if err := (Setup{}).InstallAndStart(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if len(mgr.installed) != 1 || mgr.starts != 0 {
		t.Errorf("installed=%v starts=%d", mgr.installed, mgr.starts)
	}

	// A start that fails is the error, and nothing claims the daemon is running.
	out.Reset()
	mgr = &stubManager{startErr: errors.New("systemctl --user start failed")}
	withManager(t, mgr, nil)
	if err := (Setup{}).InstallAndStart(t.Context(), &out); err == nil || out.Len() != 0 {
		t.Errorf("a failed start: err=%v output=%q", err, out.String())
	}

	// On Linux it also says what happens at logout, as `norite daemon install` does.
	out.Reset()
	linger := &lingerManager{stubManager: &stubManager{}}
	withManager(t, linger, nil)
	if err := (Setup{}).InstallAndStart(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "norite daemon install --linger") || linger.enables != 0 {
		t.Errorf("output=%q enables=%d", out.String(), linger.enables)
	}
}

func TestSetupStartIsTheServiceManagers(t *testing.T) {
	mgr := &stubManager{startErr: ErrNotInstalled}
	withManager(t, mgr, nil)
	if err := (Setup{}).Start(t.Context()); !errors.Is(err, ErrNotInstalled) || mgr.starts != 1 {
		t.Errorf("err=%v starts=%d", err, mgr.starts)
	}
}
