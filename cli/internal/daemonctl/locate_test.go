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
