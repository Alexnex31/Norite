// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// createNewConsole is CREATE_NEW_CONSOLE: the new process gets a console of its own.
const createNewConsole = 0x00000010

const (
	// watchStarted is how long a version just started is watched for. A program that cannot start at
	// all is gone within a second.
	watchStarted = 20 * time.Second
	// restartsWatched is how many times one that failed inside that time is started again. The second
	// start of a version on trial that crashed puts the previous version back (updater.AtStart).
	restartsWatched = 2
)

// startAgain starts the program now at path as a process of its own and watches it for a moment; the
// caller then exits. Windows has no way for a process to replace itself.
//
// It does not lean on the task's restart-on-failure. A daemon started by hand has no task, and the task
// runs the daemon inside a console host that exits 0 whatever the daemon did, so it never sees a failure.
// So this starts a version that failed at once again itself (startAndWatch).
//
// The new daemon gets a console of its own, hidden. A console is how Windows tells a program about a
// logoff or a shutdown, and Go turns that into the signal the daemon stops on; a daemon started with none
// is killed where it stands (M23). This is what the installer's own `Start-Process -WindowStyle Hidden`
// does. Walked in M24's pass in a Windows VM: an update, a crash undone, and sign-outs inside the trial.
func startAgain(path string) error {
	return startAndWatch(func() (func() error, error) {
		cmd := exec.Command(path, os.Args[1:]...) //nolint:gosec // this program's own path and arguments
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNewConsole}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return func() error {
			err := cmd.Wait()
			// Another daemon holds the lock, or this one cannot run as configured: starting it again
			// changes neither.
			var exit *exec.ExitError
			if errors.As(err, &exit) && (exit.ExitCode() == exitAlreadyUp || exit.ExitCode() == exitMisconfigured) {
				return nil
			}
			return err
		}, nil
	}, watchStarted, restartsWatched)
}
