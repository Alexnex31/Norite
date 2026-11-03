// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package voiceworker

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/Alexnex31/Norite/daemon/voiceipc"
)

// inherit hands the worker its two pipes as handles, which Windows has no numbered slots for, and returns
// the -pipes value naming them: a handle keeps its value in the child that inherits it. Go starts every
// process with an explicit list of the handles to inherit, so marking these inheritable reaches this child
// and no other the daemon starts.
//
// The worker is given no console of its own (no CREATE_NEW_CONSOLE): it shares the daemon's, which the
// logon task runs headless (M24), so no window appears, and a logoff's console event reaches it too.
func inherit(cmd *exec.Cmd, read, write *os.File) (string, error) {
	for _, f := range []*os.File{read, write} {
		if err := syscall.SetHandleInformation(syscall.Handle(f.Fd()), syscall.HANDLE_FLAG_INHERIT,
			syscall.HANDLE_FLAG_INHERIT); err != nil {
			return "", err
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(read.Fd()), syscall.Handle(write.Fd())},
	}
	return voiceipc.FormatPipes(read.Fd(), write.Fd()), nil
}
