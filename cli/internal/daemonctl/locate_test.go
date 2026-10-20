// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonctl

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeExecutable creates a file that passes the executability check.
func writeExecutable(t *testing.T, dir, name string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // a test fixture standing in for a real binary
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// isolate clears every environment source LocateDaemon consults, so a test only sees what it sets up. A
// developer with norite-daemon already on PATH must not get different results from CI.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv(DaemonBinaryEnvVar, "")
	t.Setenv("PATH", t.TempDir())
}

func TestLocateDaemonPrefersTheExplicitPath(t *testing.T) {
	isolate(t)

	dir := t.TempDir()
	want := writeExecutable(t, dir, daemonBinaryName)
	// An unrelated binary is also reachable, to prove the explicit answer wins rather than merely being
	// the only candidate.
	t.Setenv(DaemonBinaryEnvVar, writeExecutable(t, t.TempDir(), daemonBinaryName))

	got, err := LocateDaemon(want)
	if err != nil {
		t.Fatalf("LocateDaemon: %v", err)
	}
	if got != want {
		t.Errorf("LocateDaemon = %q, want the explicitly-named %q", got, want)
	}
}

func TestLocateDaemonFallsBackToTheEnvironment(t *testing.T) {
	isolate(t)

	want := writeExecutable(t, t.TempDir(), daemonBinaryName)
	t.Setenv(DaemonBinaryEnvVar, want)

	got, err := LocateDaemon("")
	if err != nil {
		t.Fatalf("LocateDaemon: %v", err)
	}
	if got != want {
		t.Errorf("LocateDaemon = %q, want %q from %s", got, want, DaemonBinaryEnvVar)
	}
}

func TestLocateDaemonFindsItOnPATH(t *testing.T) {
	isolate(t)

	dir := t.TempDir()
	want := writeExecutable(t, dir, daemonBinaryName+exeSuffix())
	t.Setenv("PATH", dir)

	got, err := LocateDaemon("")
	if err != nil {
		t.Fatalf("LocateDaemon: %v", err)
	}
	if got != want {
		t.Errorf("LocateDaemon = %q, want %q", got, want)
	}
}

// linkFarm builds Homebrew's shape: the programs in a directory per version, and links to them in a bin
// directory that survives upgrades. Only the programs named in linked get a link.
func linkFarm(t *testing.T, linked ...string) (bin, cellar string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("creating a symbolic link on Windows needs a privilege the test runner may not hold")
	}
	// Resolved, so the temporary directory's own links (macOS's /var is /private/var) do not show up as a
	// difference between the two candidates.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin, cellar = filepath.Join(root, "bin"), filepath.Join(root, "Caskroom", "norite", "0.1.1")
	for _, dir := range []string{bin, cellar} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, cellar, "norite")
	writeExecutable(t, cellar, daemonBinaryName)
	for _, name := range linked {
		if err := os.Symlink(filepath.Join(cellar, name), filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	return bin, cellar
}

// A service records the daemon beside the link a package manager keeps, not beside the file it points at:
// that file's directory is the version's own, and the next upgrade deletes it (ADR 0035).
func TestTheDaemonBesideTheLinkComesFirst(t *testing.T) {
	bin, cellar := linkFarm(t, "norite", daemonBinaryName)

	got := siblingDaemons(filepath.Join(bin, "norite"))
	want := []string{filepath.Join(bin, daemonBinaryName), filepath.Join(cellar, daemonBinaryName)}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("siblingDaemons = %q, want %q", got, want)
	}
	if path, err := verifyDaemonBinary(got[0], ""); err != nil || path != want[0] {
		t.Errorf("the first candidate verifies as %q, %v; want %q, which a link resolves through", path, err, want[0])
	}
}

// A `norite` linked on its own, from a dotfiles directory say, still finds the daemon beside the real file.
func TestALoneLinkStillFindsTheDaemonBesideTheRealFile(t *testing.T) {
	bin, cellar := linkFarm(t, "norite")

	got := siblingDaemons(filepath.Join(bin, "norite"))
	if len(got) != 2 {
		t.Fatalf("siblingDaemons = %q, want two candidates", got)
	}
	if _, err := verifyDaemonBinary(got[0], ""); err == nil {
		t.Fatalf("%s verified, and nothing is there", got[0])
	}
	if path, err := verifyDaemonBinary(got[1], ""); err != nil || path != filepath.Join(cellar, daemonBinaryName) {
		t.Errorf("the second candidate verifies as %q, %v; want the daemon beside the real file", path, err)
	}
}

// A binary that is not a link has one candidate, not the same one twice.
func TestAnUnlinkedBinaryHasOneCandidate(t *testing.T) {
	dir := t.TempDir()
	self := writeExecutable(t, dir, "norite")
	if got := siblingDaemons(self); len(got) != 1 {
		t.Errorf("siblingDaemons = %q, want one", got)
	}
}

func TestLocateDaemonReturnsAnAbsolutePath(t *testing.T) {
	isolate(t)

	dir := t.TempDir()
	writeExecutable(t, dir, daemonBinaryName)

	// Whatever the caller typed, the answer is written into a unit file or plist that outlives this
	// process. A service manager resolving a relative path months later resolves it against a working
	// directory nobody chose.
	rel, err := filepath.Rel(mustGetwd(t), filepath.Join(dir, daemonBinaryName))
	if err != nil {
		t.Skipf("cannot express %s relative to the working directory: %v", dir, err)
	}

	got, err := LocateDaemon(rel)
	if err != nil {
		t.Fatalf("LocateDaemon(%q): %v", rel, err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("LocateDaemon(%q) = %q, which is not absolute", rel, got)
	}
}

func TestLocateDaemonRejectsAnExplicitPathThatDoesNotExist(t *testing.T) {
	isolate(t)

	// Also make a perfectly good binary discoverable. Falling back to it would install a service pointing
	// at an executable the operator did not name, which is worse than refusing.
	dir := t.TempDir()
	writeExecutable(t, dir, daemonBinaryName)
	t.Setenv("PATH", dir)

	missing := filepath.Join(t.TempDir(), "nope")
	_, err := LocateDaemon(missing)
	if err == nil {
		t.Fatal("LocateDaemon accepted a path that does not exist")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("the error does not name the offending path: %v", err)
	}
}

func TestLocateDaemonRejectsADirectory(t *testing.T) {
	isolate(t)

	dir := t.TempDir()
	_, err := LocateDaemon(dir)
	if err == nil {
		t.Fatal("LocateDaemon accepted a directory")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
}

func TestLocateDaemonRejectsANonExecutableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows decides executability by extension, not permission bits")
	}
	isolate(t)

	path := filepath.Join(t.TempDir(), daemonBinaryName)
	if err := os.WriteFile(path, []byte("not a program"), 0o644); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	_, err := LocateDaemon(path)
	if err == nil {
		t.Fatal("LocateDaemon accepted a file with no execute bit")
	}
	// Catching it here means a clear message now, rather than a service that installs successfully and
	// then fails with a permission error at every boot.
	if !strings.Contains(err.Error(), "not executable") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
}

func TestLocateDaemonExplainsItselfWhenNothingIsFound(t *testing.T) {
	isolate(t)

	_, err := LocateDaemon("")
	if err == nil {
		t.Fatal("LocateDaemon succeeded with no daemon anywhere")
	}

	// This is the error a first-time user is most likely to hit — an archive extracted with the two
	// binaries in different places. It has to say what to do next, not just what went wrong.
	for _, want := range []string{daemonBinaryName, "--daemon-binary", DaemonBinaryEnvVar} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return wd
}
