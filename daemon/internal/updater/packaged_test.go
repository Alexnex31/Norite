// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Alexnex31/Norite/daemon/installreceipt"
)

func writeMarker(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil { //nolint:gosec // a package's marker is world-readable
		t.Fatal(err)
	}
}

// A package install says which command updates it, and installs nothing itself (ADR 0035 §4).
func TestAPackageInstallNamesItsCommand(t *testing.T) {
	w := newWorld(t)
	if err := installreceipt.Remove(w.state); err != nil {
		t.Fatal(err)
	}

	// The FHS shape: /usr/bin's daemon, /usr/lib/norite's marker.
	usr := filepath.Join(t.TempDir(), "usr")
	writeMarker(t, filepath.Join(usr, "lib", "norite", MarkerName), "apt\n")
	u := w.updater("0.1.0", func(o *Options) { o.Executable = filepath.Join(usr, "bin", daemonName) })
	if s := u.Status(); s.Installs || !strings.Contains(s.Reason, "sudo apt update && sudo apt upgrade") {
		t.Errorf("the .deb's marker: installs=%v, reason %q", s.Installs, s.Reason)
	}

	// Scoop's shape: the marker beside the binaries.
	writeMarker(t, filepath.Join(w.bin, MarkerName), "scoop")
	if s := w.updater("0.1.0").Status(); s.Installs || !strings.Contains(s.Reason, "scoop update norite") {
		t.Errorf("Scoop's marker: installs=%v, reason %q", s.Installs, s.Reason)
	}
}

// A receipt naming the running daemon wins over a marker: the installer's copy is the updater's.
func TestAReceiptNamingThisDaemonWinsOverAMarker(t *testing.T) {
	w := newWorld(t)
	writeMarker(t, filepath.Join(w.bin, MarkerName), "apt")
	if u := w.updater("0.1.0"); !u.installs {
		t.Errorf("a receipted install with a marker beside it: %s", u.reason)
	}
}

// A receipt naming another directory and a marker beside this daemon: the package's copy is running, and
// its manager's command is the answer, not "the installer put it somewhere else".
func TestAReceiptForAnotherCopyGivesWayToTheMarker(t *testing.T) {
	w := newWorld(t)
	usr := filepath.Join(t.TempDir(), "usr")
	writeMarker(t, filepath.Join(usr, "lib", "norite", MarkerName), "pacman")
	u := w.updater("0.1.0", func(o *Options) { o.Executable = filepath.Join(usr, "bin", daemonName) })
	if s := u.Status(); s.Installs || !strings.Contains(s.Reason, "pacman -Syu") {
		t.Errorf("installs=%v, reason %q", s.Installs, s.Reason)
	}
}

func TestThePathsOfManagersThatCannotInstallAMarker(t *testing.T) {
	cases := []struct{ exe, goos, says string }{
		{"/opt/homebrew/Caskroom/norite/0.1.1/norite-daemon", "darwin", "brew upgrade norite"},
		{"/home/linuxbrew/.linuxbrew/Caskroom/norite/0.1.1/norite-daemon", "linux", "brew upgrade norite"},
		{`C:\Users\Ada\AppData\Local\Microsoft\WinGet\Packages\Norite.Norite_Microsoft.Winget.Source_8wekyb3d8bbwe\norite-daemon.exe`, "windows", "winget upgrade Norite.Norite"},
		{"/nix/store/0123456789abcdfghijklmnpqrsvwxyz-norite-0.1.1/bin/norite-daemon", "linux", "configuration that installed it"},
		{"/home/ada/.local/bin/norite-daemon", "linux", ""},
		{`C:\Users\Ada\AppData\Local\Programs\Norite\norite-daemon.exe`, "windows", ""},
	}
	for _, c := range cases {
		got := packageManager(c.exe, c.goos)
		if (c.says == "") != (got == "") || !strings.Contains(got, c.says) {
			t.Errorf("%s on %s: %q, want one saying %q", c.exe, c.goos, got, c.says)
		}
	}
}

// What a marker says is shown to a person, so only a manager this project publishes to is believed, and
// only from a small regular file.
func TestAMarkerIsBelievedOnlyWhenItIsOne(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, daemonName)
	marker := filepath.Join(dir, MarkerName)

	for _, body := range []string{"", "dpkg", "apt; rm -rf /", strings.Repeat("apt ", maxMarker)} {
		writeMarker(t, marker, body)
		if got := packageManager(exe, "linux"); got != "" {
			t.Errorf("a marker saying %q was believed: %q", body, got)
		}
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(marker, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := packageManager(exe, "linux"); got != "" {
		t.Errorf("a directory named like the marker was believed: %q", got)
	}
}
