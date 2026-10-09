// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package daemonctl

import (
	"context"
	"syscall"
	"time"
)

// processExited waits until the process has exited, or ctx ends, and reports which.
//
// A handle with only the right to wait on it. A process that cannot be opened is one that is gone, or is
// somebody else's under a reused id, and either way the daemon is not running under it.
func processExited(ctx context.Context, pid int) bool {
	handle, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(pid)) //nolint:gosec // a process id fits
	if err != nil {
		return true
	}
	defer func() { _ = syscall.CloseHandle(handle) }()

	const slice = 100 * time.Millisecond
	for {
		event, err := syscall.WaitForSingleObject(handle, uint32(slice/time.Millisecond))
		if err != nil {
			return false
		}
		if event == syscall.WAIT_OBJECT_0 {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
	}
}
