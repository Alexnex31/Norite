// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package updater

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A named pipe at the marker's path would hold the daemon's start for ever if it were opened.
func TestAPipeAtTheMarkersPathIsNotOpened(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, MarkerName), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() { done <- packageManager(filepath.Join(dir, daemonName), "linux") }()
	select {
	case got := <-done:
		if got != "" {
			t.Errorf("a pipe was believed: %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading the marker waited on a pipe nobody writes to")
	}
}
