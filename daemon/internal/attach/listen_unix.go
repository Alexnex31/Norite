// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package attach

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"

	"github.com/Alexnex31/Norite/daemon/ipc"
)

// Listen opens the attach socket in stateDir. The caller must hold the daemon's single-instance lock: that is
// what makes any socket file already there stale by construction — left by a daemon that crashed — and safe
// to remove.
//
// Something there that is not a socket is refused rather than removed. Nothing in this program puts one
// there, and the state directory is the user's; deleting what somebody else left is not this daemon's call.
func Listen(stateDir string) (net.Listener, error) {
	path := ipc.SocketPath(stateDir)
	if err := ipc.CheckSocketPath(path); err != nil {
		return nil, err
	}

	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("checking for a stale attach socket: %w", err)
	case info.Mode()&fs.ModeSocket == 0:
		return nil, fmt.Errorf("%s exists and is not a socket; remove it so the daemon can listen there", path)
	default:
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("removing the stale attach socket: %w", err)
		}
	}

	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on the attach socket: %w", err)
	}
	// The directory's 0700 is the boundary. The socket's own mode follows the umask, and is narrowed so a
	// directory somebody loosened does not also leave the socket writable by everybody.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("restricting the attach socket: %w", err)
	}
	return l, nil
}
