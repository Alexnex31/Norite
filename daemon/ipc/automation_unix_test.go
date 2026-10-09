// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package ipc

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestAnAutomationFileThatIsAPipeIsRefusedWithoutBeingOpened is M21's finding about a config, for the one
// file here a script's launcher reads: opening a pipe nobody writes to waits for ever.
func TestAnAutomationFileThatIsAPipeIsRefusedWithoutBeingOpened(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, syscall.Mkfifo(AutomationFilePath(dir), 0o600))

	done := make(chan error, 1)
	go func() {
		_, err := LoadAutomationFile(dir)
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrAutomationOff)
	case <-time.After(5 * time.Second):
		// Unblock the reader so the test binary can exit, then fail.
		if w, err := os.OpenFile(AutomationFilePath(dir), os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
		t.Fatal("LoadAutomationFile opened a pipe and waited on it")
	}
}
