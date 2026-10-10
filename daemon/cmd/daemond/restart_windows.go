// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// createNewConsole is CREATE_NEW_CONSOLE: the new process gets a console of its own.
const createNewConsole = 0x00000010

// startAgain starts the program now at path as a process of its own, and returns; the caller then exits.
// Windows has no way for a process to replace itself.
//
// It does not lean on the task's restart-on-failure, because the installer starts the daemon by hand
// unless it is asked to register one, and a daemon nobody restarts would stay stopped after every update.
//
// The new daemon gets a console of its own, hidden. A console is how Windows tells a program about a
// logoff or a shutdown, and Go turns that into the signal the daemon stops on; a daemon started with none
// is killed where it stands (M23). This is what the installer's own `Start-Process -WindowStyle Hidden`
// does. Not run on Windows before M24's own pass there.
func startAgain(path string) error {
	cmd := exec.Command(path, os.Args[1:]...) //nolint:gosec // this program's own path and arguments
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNewConsole}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
