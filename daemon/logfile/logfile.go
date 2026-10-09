// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package logfile says where the daemon's log is, and reads it.
//
// Outside internal/ because two programs need the same answer: the daemon, which writes the log, and
// `norite logs tail`, which reads it and has to work with no daemon running, since a daemon that will not
// start is when the log is wanted. Before M23 the path was decided twice, by the daemon and by the launchd
// plist's -log-file, so a daemon started by hand on macOS wrote somewhere the service never did.
package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Alexnex31/Norite/daemon/internal/paths"
)

// MacName is the log's name in ~/Library/Logs. That directory is shared with every other program, so the
// name says whose it is; in the state directory the file is daemon.log.
const MacName = "norite-daemon.log"

// crashSuffix replaces the log's ".log" to name the file a fatal crash is written to.
const crashSuffix = ".crash.log"

// Path returns where the current user's daemon writes its log unless told otherwise with -log-file.
//
// It creates nothing: a command that only reads must not leave a state directory behind it.
func Path() (string, error) { return pathFor(runtime.GOOS) }

// pathFor takes the platform as a parameter so all three answers are testable from one machine.
func pathFor(goos string) (string, error) {
	if goos == "darwin" {
		// Where macOS users and Console.app look, whoever started the daemon.
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locating the user's home directory: %w", err)
		}
		return filepath.Join(home, "Library", "Logs", MacName), nil
	}
	dir, err := paths.StateDirFor(goos)
	if err != nil {
		return "", err
	}
	return paths.LogFile(dir), nil
}

// In returns the log's path inside a state directory given explicitly, on every platform. Tests give
// one; nothing in production does.
func In(stateDir string) string { return paths.LogFile(stateDir) }

// CrashPath returns the file a fatal crash is written to, beside the log it belongs to: daemon.crash.log
// for daemon.log. A log not named *.log gets the suffix appended.
func CrashPath(logPath string) string {
	return strings.TrimSuffix(logPath, ".log") + crashSuffix
}
