// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// leaveOwnConsole lets go of the console window when it was opened for this process alone, and reports
// whether it did.
//
// Started by Task Scheduler at logon, or by a double click, a console program is given a new console
// window of its own. For a daemon that is a window that sits on the desktop saying nothing, and closing
// it ends the daemon. Started from a terminal, the console is the terminal's and is shared with the shell
// that typed the command: there the daemon keeps it, so `norite-daemon` in a tab still prints.
//
// The two are told apart by counting who is attached. One process, this one, means the window exists for
// the daemon and nobody is reading it. The window is on screen for a moment before it goes; building the
// daemon as a windowless program would avoid that and would also stop it printing in a terminal, which is
// the worse loss. Written from the documented API and not yet run on Windows from this code.
func leaveOwnConsole() bool {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	list := kernel32.NewProc("GetConsoleProcessList")
	free := kernel32.NewProc("FreeConsole")
	if list.Find() != nil || free.Find() != nil {
		return false
	}
	var ids [2]uint32
	// The count of processes attached to this console, or 0 when there is no console at all.
	attached, _, _ := list.Call(uintptr(unsafe.Pointer(&ids[0])), uintptr(len(ids)))
	if attached != 1 {
		return false
	}
	ok, _, _ := free.Call()
	return ok != 0
}
