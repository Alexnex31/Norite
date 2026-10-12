// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package updater

import (
	"syscall"
	"testing"
	"time"
)

// A pipe at the record's path is not opened: opening one waits for a writer that never comes, and the
// record is read as the first thing a start does, with the daemon's lock held (M21's rule for a config).
func TestARecordThatIsAPipeDoesNotHoldTheStart(t *testing.T) {
	x := newInstalled(t)
	if err := syscall.Mkfifo(x.in.recordPath(), 0o600); err != nil {
		t.Skipf("no pipes here: %v", err)
	}
	read := make(chan Record, 1)
	go func() { read <- x.in.Load() }()
	select {
	case r := <-read:
		if !r.unreadable {
			t.Errorf("a pipe was read as a record: %+v", r)
		}
	case <-time.After(3 * time.Second):
		// Let the reader go, so the test's own goroutine does not outlive it.
		if w, err := syscall.Open(x.in.recordPath(), syscall.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = syscall.Close(w)
		}
		t.Fatal("reading the record waited on a pipe")
	}
}
