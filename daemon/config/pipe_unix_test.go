// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A config that is a link to a pipe, which a dotfiles repository can hold as easily as a link to a
// terminal device, is refused before it is opened. Opening a pipe waits for a writer: it hung every
// command, the client before its first frame, and the daemon's toggle with the state file's lock held.
func TestAConfigThatIsAPipeIsRefusedWithoutWaiting(t *testing.T) {
	dir := t.TempDir()
	pipe := filepath.Join(dir, "pipe")
	require.NoError(t, syscall.Mkfifo(pipe, 0o600))
	link := filepath.Join(dir, "linked.toml")
	require.NoError(t, os.Symlink(pipe, link))
	done := make(chan error, 1)
	go func() {
		_, err := Load(link, TUI)
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrNotAFile)
		assert.Contains(t, err.Error(), link, "the error names the file")
	case <-time.After(5 * time.Second):
		t.Fatal("reading the config is waiting on a pipe")
	}
}
