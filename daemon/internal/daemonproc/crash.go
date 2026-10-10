// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonproc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime/debug"
	"strings"
)

// crashFileMode matches the log's: a traceback can name paths and arguments (rule 8).
const crashFileMode = 0o600

// previousCrashPath is where the traceback of the run before this one is kept: daemon.crash.1.log for
// daemon.crash.log. One, replaced by the next: a daemon crashing at every start keeps its latest two.
func previousCrashPath(path string) string {
	return strings.TrimSuffix(path, ".log") + ".1.log"
}

// captureCrashes has the runtime write a fatal crash to path as well as to stderr.
//
// The file belongs to one run. Anything in it at start was written by the run before, which therefore
// crashed: it is moved aside and its new path returned as previous, for the caller to say so in the log,
// where `norite logs tail` shows it. That is the whole of the file's bound, with no size to choose: one
// traceback here, one set aside.
//
// release undoes the redirection. The runtime keeps its own duplicate of the handle, so the one opened
// here is closed straight away.
func captureCrashes(path string) (previous string, release func(), err error) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return "", nil, fmt.Errorf("checking %s: %w", path, err)
	// Whatever is at the path is written to as this user. A link would send a traceback to a file
	// somebody else chose, and anything else that is not a file cannot be written sensibly.
	case !info.Mode().IsRegular():
		return "", nil, fmt.Errorf("%s is not a regular file", path)
	case info.Size() > 0:
		previous = previousCrashPath(path)
		if err := os.Rename(path, previous); err != nil {
			return "", nil, fmt.Errorf("setting the last crash aside: %w", err)
		}
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, crashFileMode) //nolint:gosec // path is beside the daemon's own log
	if err != nil {
		return previous, nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	if err := debug.SetCrashOutput(file, debug.CrashOptions{}); err != nil {
		return previous, nil, fmt.Errorf("directing crashes to %s: %w", path, err)
	}
	// Undone when the daemon returns, so a process that runs Run more than once, as the tests do, does not
	// keep a handle on a file in a directory that is about to be removed.
	return previous, func() { _ = debug.SetCrashOutput(nil, debug.CrashOptions{}) }, nil
}
