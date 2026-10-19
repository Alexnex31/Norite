// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package main

import (
	"os"
	"syscall"
)

// startAgain replaces this process with the program now at path, keeping its arguments, its environment
// and its process id: to systemd and launchd the service never stopped, and a daemon somebody started by
// hand goes on running in their terminal. It returns only when it failed.
func startAgain(path string) error {
	return syscall.Exec(path, os.Args, os.Environ()) //nolint:gosec // this program's own path
}
