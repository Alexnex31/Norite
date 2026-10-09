// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package logfile

import (
	"os"
	"syscall"
)

// openForRead opens a log to read it, sharing the right to delete and rename it.
//
// The daemon rotates its log by renaming it, and Windows refuses to rename a file another process has
// open unless that process opened it allowing so. os.Open does not, so a reader holding the log at the
// wrong moment would make the rotation fail and the line that caused it be lost. Not run on Windows from
// the branch that wrote it: it builds, and M23's entry says what was and was not tried there.
func openForRead(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}
