// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package automationcmd

import (
	"fmt"
	"syscall"
)

// replaceProcess starts the program in this process's place, so nothing of norite is left between a
// service manager and the script it means to run: a signal sent to stop the script reaches the script, and
// its exit code is the one reported.
func replaceProcess(path string, argv, env []string) error {
	if err := syscall.Exec(path, argv, env); err != nil { //nolint:gosec // the program the caller named
		return fmt.Errorf("cannot start %s: %w", path, err)
	}
	return nil
}
