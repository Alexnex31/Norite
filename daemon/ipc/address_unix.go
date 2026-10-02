// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package ipc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/Alexnex31/Norite/daemon/internal/paths"
)

// socketName is the attach socket's file name inside the state directory.
const socketName = "daemon.sock"

// SocketPath is where the daemon whose state directory is stateDir listens.
//
// Inside the state directory rather than $XDG_RUNTIME_DIR: that directory is 0700, which is what protects
// the socket, and it is the per-user rendezvous point the single-instance lock already uses. macOS has no
// runtime directory, and a lingering systemd user service can outlive the login session that owns one.
func SocketPath(stateDir string) string {
	return filepath.Join(stateDir, socketName)
}

// Address returns this user's attach socket path, checked against the platform's length limit.
func Address() (string, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	path := SocketPath(dir)
	if err := CheckSocketPath(path); err != nil {
		return "", err
	}
	return path, nil
}

// maxSocketPath is the longest path a Unix socket address holds, its terminating NUL excluded: sun_path is
// 108 bytes on Linux and 104 on the BSDs, macOS among them.
func maxSocketPath() int {
	if runtime.GOOS == "linux" {
		return 107
	}
	return 103
}

// CheckSocketPath refuses a path too long for a socket address. Without it, the failure is bind's or
// connect's "invalid argument", which names nothing; an exported XDG_STATE_HOME deep enough to reach it is
// unusual and entirely possible.
func CheckSocketPath(path string) error {
	if max := maxSocketPath(); len(path) > max {
		return fmt.Errorf("the attach socket path %s is %d bytes, and this platform allows %d; "+
			"point XDG_STATE_HOME at a shorter directory", path, len(path), max)
	}
	return nil
}

// Dial connects to this user's daemon.
func Dial(ctx context.Context) (net.Conn, error) {
	addr, err := Address()
	if err != nil {
		return nil, err
	}
	return DialAt(ctx, addr)
}

// DialAt connects to the daemon listening at addr. A missing socket and one nobody accepts on are both
// ErrNotRunning: the second is what a daemon that crashed leaves behind until the next one starts.
func DialAt(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", addr)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, ErrNotRunning
		}
		return nil, fmt.Errorf("connecting to the daemon at %s: %w", addr, err)
	}
	return conn, nil
}
