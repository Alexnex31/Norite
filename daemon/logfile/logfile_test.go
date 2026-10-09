// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package logfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTheLogIsOnePlacePerPlatform(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	state := filepath.Join(home, "xdg-state")
	t.Setenv("XDG_STATE_HOME", state)
	local := filepath.Join(home, "local-app-data")
	t.Setenv("LOCALAPPDATA", local)

	cases := []struct{ goos, want string }{
		{"linux", filepath.Join(state, "norite", "daemon.log")},
		{"freebsd", filepath.Join(state, "norite", "daemon.log")},
		// Not the state directory, and not dependent on who started the daemon.
		{"darwin", filepath.Join(home, "Library", "Logs", "norite-daemon.log")},
		{"windows", filepath.Join(local, "Norite", "daemon.log")},
	}
	for _, c := range cases {
		got, err := pathFor(c.goos)
		if err != nil {
			t.Fatalf("%s: %v", c.goos, err)
		}
		if got != c.want {
			t.Errorf("%s: the log is %q, want %q", c.goos, got, c.want)
		}
	}

	// A command that only reads must leave nothing behind on a machine that never ran a daemon.
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("listing the home directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("resolving the log's path created %d entries under the home directory", len(entries))
	}
}

func TestTheCrashFileIsBesideItsLog(t *testing.T) {
	cases := []struct{ log, want string }{
		{filepath.Join("s", "daemon.log"), filepath.Join("s", "daemon.crash.log")},
		{filepath.Join("l", MacName), filepath.Join("l", "norite-daemon.crash.log")},
		// A log put elsewhere under another name still gets a crash file that is not the log itself.
		{filepath.Join("x", "out.txt"), filepath.Join("x", "out.txt.crash.log")},
	}
	for _, c := range cases {
		if got := CrashPath(c.log); got != c.want {
			t.Errorf("CrashPath(%q) = %q, want %q", c.log, got, c.want)
		}
	}
}
