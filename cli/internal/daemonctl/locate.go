package daemonctl

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// DaemonBinaryEnvVar names the daemon executable explicitly.
//
// An environment variable as well as a flag because the install is often run by provisioning tooling
// (a Dockerfile, an Ansible task, a dotfiles bootstrap) where exporting a variable once is easier than
// threading a flag through every invocation.
const DaemonBinaryEnvVar = "NORITE_DAEMON_BINARY"

// daemonBinaryName is the executable goreleaser produces for the daemon module.
const daemonBinaryName = "norite-daemon"

// LocateDaemon resolves the absolute path of the daemon executable to record in the service definition.
//
// It has to be absolute and it has to be right, because the answer is written into a unit file or plist
// that outlives this process by months. A service manager resolving a relative path, or a PATH lookup
// performed at start time in an environment that is not the shell's, is a class of bug that surfaces only
// after a reboot — long after the install that caused it.
//
// Order, most explicit first:
//
//  1. the --daemon-binary flag, passed here as explicit
//  2. NORITE_DAEMON_BINARY
//  3. the file sitting next to the running `norite` binary, which is how every archive, package, and
//     `go build` output arranges the two
//  4. PATH
//
// An explicitly-named binary that does not exist is an error rather than a fallback: silently installing a
// service pointing at a different executable than the one asked for is worse than refusing.
func LocateDaemon(explicit string) (string, error) {
	if explicit != "" {
		return verifyDaemonBinary(explicit, "the path given on the command line")
	}
	if fromEnv := os.Getenv(DaemonBinaryEnvVar); fromEnv != "" {
		return verifyDaemonBinary(fromEnv, DaemonBinaryEnvVar)
	}

	if sibling, err := siblingDaemon(); err == nil {
		if resolved, err := verifyDaemonBinary(sibling, ""); err == nil {
			return resolved, nil
		}
	}

	if onPath, err := exec.LookPath(daemonBinaryName); err == nil {
		return verifyDaemonBinary(onPath, "")
	}

	return "", fmt.Errorf(
		"cannot find the %s executable: it is not next to this `norite` binary and not on PATH"+
			" — pass --daemon-binary /path/to/%s, or set %s",
		daemonBinaryName, daemonBinaryName, DaemonBinaryEnvVar)
}

// siblingDaemon returns the daemon path implied by where the running binary lives.
func siblingDaemon() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating this executable: %w", err)
	}
	// EvalSymlinks so that a `norite` reached through a symlink farm — Homebrew's bin, a dotfiles link,
	// GOPATH/bin — resolves next to the real file rather than next to the link.
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	return filepath.Join(filepath.Dir(self), daemonBinaryName+exeSuffix()), nil
}

// verifyDaemonBinary turns a candidate into an absolute path, or explains why it cannot be used.
//
// source names where the candidate came from, for the error message; empty means it was discovered rather
// than asked for, in which case failures are quiet and the caller moves on to the next strategy.
func verifyDaemonBinary(candidate, source string) (string, error) {
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", candidate, err)
	}

	// Reject control characters before anything else looks at this path.
	//
	// The resolved path is interpolated into a systemd unit file, which is parsed line by line: a newline
	// inside it ends the ExecStart= directive and everything after becomes further directives. Quoting does
	// not help — the quote sits on the first line while the injected text is parsed as configuration — so
	// `.../norite-daemon\nExecStartPost=/bin/sh -c ...` would run an attacker's command at every login.
	//
	// Rejecting rather than escaping is the correct fix: systemd has no representation for a newline inside
	// ExecStart, so there is nothing to escape it to. Filenames like this are legal on Linux but never
	// legitimate here, and checking once at the single point every path is resolved covers all three
	// backends rather than each having to remember.
	if i := strings.IndexFunc(abs, isControl); i >= 0 {
		return "", describeBadBinary(strconv.Quote(abs), source,
			fmt.Sprintf("it contains a control character (%q at byte %d), which cannot appear in a service definition", abs[i], i))
	}

	info, err := os.Stat(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", describeBadBinary(abs, source, "no such file")
	case err != nil:
		return "", fmt.Errorf("checking %s: %w", abs, err)
	case info.IsDir():
		return "", describeBadBinary(abs, source, "it is a directory")
	}

	// Windows decides executability by extension, and os.Stat reports no permission bits worth checking, so
	// the mode test is Unix-only rather than a portable check that would always pass there.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return "", describeBadBinary(abs, source, "it is not executable")
	}

	return abs, nil
}

func describeBadBinary(path, source, why string) error {
	if source == "" {
		return fmt.Errorf("%s: %s", path, why)
	}
	return fmt.Errorf("%s names %s, but %s", source, path, why)
}

// isControl reports whether r is a character that must never appear in a path written into a service
// definition. Covers C0 (newline, carriage return, NUL, tab), DEL, and C1.
func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
