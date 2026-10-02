// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package ipc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// pipePrefix starts every daemon's pipe name; the account's SID finishes it, so two users' daemons on one
// machine never contend for a name.
const pipePrefix = `\\.\pipe\norite-daemon-`

// UserSID is the SID of the account this process runs as.
func UserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("reading this process's account: %w", err)
	}
	return user.User.Sid, nil
}

// Address returns this user's pipe name.
func Address() (string, error) {
	sid, err := UserSID()
	if err != nil {
		return "", err
	}
	return pipePrefix + sid.String(), nil
}

// PipeSecurity is the descriptor the daemon creates its pipe with: owned by the account, and readable and
// writable by it alone. A pipe's default DACL grants read to Everyone and to the anonymous account, so the
// first-party tier exists on Windows only because of this.
func PipeSecurity(sid *windows.SID) string {
	s := sid.String()
	return "O:" + s + "D:P(A;;GA;;;" + s + ")"
}

// Dial connects to this user's daemon.
func Dial(ctx context.Context) (net.Conn, error) {
	addr, err := Address()
	if err != nil {
		return nil, err
	}
	return DialAt(ctx, addr)
}

// DialAt connects to the pipe at addr and checks that it belongs to this account before anything is sent.
//
// A pipe name is first come, first served, so another account's process can create this one before the
// daemon starts. The DACL that process chose would be its own, so the check is the owner: a pipe the daemon
// created is owned by the account it runs as, which is this one.
func DialAt(ctx context.Context, addr string) (net.Conn, error) {
	conn, err := winio.DialPipeContext(ctx, addr)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return nil, ErrNotRunning
		}
		return nil, fmt.Errorf("connecting to the daemon at %s: %w", addr, err)
	}
	if err := checkOwner(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func checkOwner(conn net.Conn) error {
	handled, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("the daemon's pipe exposes no handle to check its owner with")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(handled.Fd()), windows.SE_KERNEL_OBJECT,
		windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("reading the owner of the daemon's pipe: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("reading the owner of the daemon's pipe: %w", err)
	}
	me, err := UserSID()
	if err != nil {
		return err
	}
	if !owner.Equals(me) {
		return fmt.Errorf("the pipe %s belongs to another account (%s); refusing to attach to it",
			pipePrefix+me.String(), owner.String())
	}
	return nil
}
