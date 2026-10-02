// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package attach

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID reads the connecting process's uid from the socket (SO_PEERCRED). ok is false for a connection
// that is not a Unix socket, which only a test makes.
func peerUID(nc net.Conn) (uid int, ok bool, err error) {
	uc, isUnix := nc.(*net.UnixConn)
	if !isUnix {
		return 0, false, nil
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, false, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, false, err
	}
	if credErr != nil {
		return 0, false, credErr
	}
	return int(cred.Uid), true, nil
}
