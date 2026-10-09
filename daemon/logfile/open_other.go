// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !unix && !windows

package logfile

import "os"

// openForRead opens a log to read it, with nothing platform-specific to ask for.
func openForRead(path string) (*os.File, error) { return os.Open(path) } //nolint:gosec // the user's own log
