// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package logfile

import (
	"os"
	"syscall"
)

// openForRead opens a log to read it, without waiting on whatever is at the path.
//
// O_NONBLOCK because a log's path can hold something that is not a file, and opening a pipe nobody writes
// to waits for ever (M21 met the same with config.toml). Opened this way it returns at once, and the
// caller's own check that it holds a regular file is made on the handle, where nothing can change it. On
// a regular file the flag does nothing.
func openForRead(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // the path is the user's own log
}
