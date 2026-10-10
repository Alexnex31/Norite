// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package daemonproc

import "golang.org/x/sys/unix"

// openFileLimit reports the soft RLIMIT_NOFILE the daemon runs under, or 0 when it cannot be read.
//
// M3 raised the limit here, toward 4096, against a default of 256 on macOS. It never raised anything: Go's
// runtime raises the soft limit to the hard one before main runs, so the function returned early on every
// platform (measured at M23: a program started under `ulimit -Sn 256` saw 524287). It is not repaired into
// doing something, because the runtime restores the original limit for child processes only while the
// program has not called Setrlimit itself, and the voice-worker should not inherit a raise.
//
// What bounds the daemon's handles is its own caps: ipc.MaxClients, the automation port's served and
// pending connections, one gateway connection.
func openFileLimit() uint64 {
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
		return 0
	}
	return lim.Cur
}
