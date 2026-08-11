// Package paths resolves the per-user directories the daemon owns.
//
// One process per OS user account (ADR 0010), so every path here is user-scoped — never a system-wide
// location. Two daemons for two different logins on one machine must not share a lock file, a log file, or
// later a state file, and deriving everything from the user's own base directory is what guarantees that.
//
// Only the daemon-owned state directory lives here. The hand-editable client config
// (~/.config/norite/config.toml, docs/architecture.md §3) is a separate concern with separate rules — it is
// shared with the CLI and GUI, and it arrives with the milestone that first reads it.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// appDirName is the per-platform directory name. Lowercase where the surrounding convention is lowercase
// (XDG), capitalized where the platform's own directories are.
const (
	xdgDirName     = "norite"
	appleDirName   = "Norite"
	windowsDirName = "Norite"
	lockFileName   = "daemon.lock"
	logFileName    = "daemon.log"
	stateDirPerm   = 0o700
)

// StateDir returns the directory holding everything the daemon writes and nobody hand-edits: the
// single-instance lock, logs, and (from later milestones) plugin capability grants and the voice-channel
// breadcrumb.
//
// 0700 on creation, and deliberately so: later milestones put plugin grants and hash pins in here, and a
// world-writable version of that directory would let any local user rewrite what a plugin is allowed to do.
// Establishing the mode now, while the directory holds nothing sensitive, means no migration later.
func StateDir() (string, error) {
	dir, err := stateDirFor(runtime.GOOS)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, stateDirPerm); err != nil {
		return "", fmt.Errorf("creating state directory %s: %w", dir, err)
	}
	if err := tighten(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// tighten removes group and world access from an already-existing state directory.
//
// MkdirAll applies its mode only when it actually creates the directory; on one that already exists it does
// nothing. So a directory left at 0755 by an older build, a restore from backup, or a permissive umask
// stays that way forever, and "0700" above would be true only of a first run. That is not a guarantee worth
// stating for a directory about to hold plugin capability grants and pinned .wasm hashes (CLAUDE.md
// rule 12), where another local user's write access decides what plugin code is allowed to do.
//
// Only ever tightens: a directory an operator deliberately made stricter than 0700 is left alone, and the
// mode is rewritten only when loose bits are actually set, so the ordinary case costs one Stat.
func tighten(dir string) error {
	// Windows does not model Unix permission bits — os.Chmod there only toggles the read-only flag, so
	// applying this would be meaningless. Access control comes from the ACL inherited from %LOCALAPPDATA%,
	// which is already user-scoped.
	if runtime.GOOS == "windows" {
		return nil
	}

	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("checking state directory %s: %w", dir, err)
	}

	if loose := info.Mode().Perm() &^ stateDirPerm; loose != 0 {
		if err := os.Chmod(dir, info.Mode().Perm()&stateDirPerm); err != nil {
			return fmt.Errorf("securing state directory %s: it is %#o and group/other access could not be "+
				"removed: %w", dir, info.Mode().Perm(), err)
		}
	}
	return nil
}

// stateDirFor resolves the state directory for a named GOOS without creating it.
//
// The platform is a parameter rather than a direct read of runtime.GOOS so that the resolution rules for
// all three platforms are testable from any one of them. These paths are the kind of thing that is wrong
// for years because nobody on the team runs the platform that got it wrong.
func stateDirFor(goos string) (string, error) {
	parts, err := stateBase(goos)
	if err != nil {
		return "", err
	}
	return filepath.Join(parts...), nil
}

func stateBase(goos string) ([]string, error) {
	switch goos {
	case "windows":
		// LOCALAPPDATA rather than APPDATA: this is machine-local state by nature, and a roaming profile
		// would sync a lock file and a log between machines, which is meaningless at best.
		if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
			return []string{dir, windowsDirName}, nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locating the user's home directory: %w", err)
		}
		return []string{home, "AppData", "Local", windowsDirName}, nil

	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locating the user's home directory: %w", err)
		}
		return []string{home, "Library", "Application Support", appleDirName}, nil

	default:
		// XDG_STATE_HOME is the correct base for "state that should persist between restarts but is not
		// important enough for XDG_DATA_HOME" — logs and lock files are its canonical examples. Only an
		// absolute value is honored, per the spec; a relative one is treated as unset rather than resolved
		// against the working directory, which for a service started by systemd is not a meaningful place.
		if dir := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
			return []string{dir, xdgDirName}, nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locating the user's home directory: %w", err)
		}
		return []string{home, ".local", "state", xdgDirName}, nil
	}
}

// LockFile is the path of the single-instance lock.
func LockFile(stateDir string) string { return filepath.Join(stateDir, lockFileName) }

// LogFile is the path of the daemon's rotating log.
func LogFile(stateDir string) string { return filepath.Join(stateDir, logFileName) }
