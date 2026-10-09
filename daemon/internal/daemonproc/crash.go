// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonproc

import (
	"fmt"
	"os"
	"runtime/debug"
)

// crashFileMax is the size past which the crash file is emptied at start. One crash is a few kilobytes
// per goroutine; a file past this holds crashes nobody read, and it must not grow for ever.
const crashFileMax = 256 << 10

// crashFileMode matches the log's: a traceback can name paths and arguments (rule 8).
const crashFileMode = 0o600

// captureCrashes has the runtime write a fatal crash to path as well as to stderr, and returns what
// undoes it.
//
// The file is appended to, so a daemon that crashes at every start leaves each crash rather than the
// last, and emptied at start once it passes crashFileMax. The runtime keeps its own duplicate of the
// handle, so the one opened here is closed straight away.
func captureCrashes(path string) (release func(), err error) {
	if info, err := os.Lstat(path); err == nil {
		// Whatever is at the path is appended to as this user. A link would send a traceback to a file
		// somebody else chose, and anything else that is not a file cannot be written sensibly.
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", path)
		}
		if info.Size() > crashFileMax {
			if err := os.Truncate(path, 0); err != nil {
				return nil, fmt.Errorf("emptying %s: %w", path, err)
			}
		}
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, crashFileMode) //nolint:gosec // path is beside the daemon's own log
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	if err := debug.SetCrashOutput(file, debug.CrashOptions{}); err != nil {
		return nil, fmt.Errorf("directing crashes to %s: %w", path, err)
	}
	// Undone when the daemon returns, so a process that runs Run more than once, as the tests do, does not
	// keep a handle on a file in a directory that is about to be removed.
	return func() { _ = debug.SetCrashOutput(nil, debug.CrashOptions{}) }, nil
}
