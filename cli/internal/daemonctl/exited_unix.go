// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package daemonctl

import (
	"context"
	"errors"
	"syscall"
	"time"
)

// processExited waits until no process has the id, or ctx ends, and reports which.
//
// Signal 0 delivers nothing and checks only that the process exists. EPERM means one does and is somebody
// else's, which for an id the user's own daemon gave is a reused id: the daemon is gone.
//
// A process that has exited and not been waited for still exists to a signal. A daemon started by a
// parent that never waits, a container's entrypoint script for one, stays that way for good, so where the
// system says a process is one of those, it has exited.
func processExited(ctx context.Context, pid int) bool {
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) || isZombie(pid) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
}
