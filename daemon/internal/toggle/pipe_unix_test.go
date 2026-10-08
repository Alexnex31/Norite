// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package toggle

import (
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/ipc"
)

// A pipe where a client's file would go is refused without being read. Reading it would wait for ever,
// in the daemon, with the state file's lock held, so that no later toggle could run either.
func TestSplitDoesNotWaitOnAPipe(t *testing.T) {
	f := newFixture(t)
	f.write(f.files.Shared, shared)
	require.NoError(t, syscall.Mkfifo(f.files.TUI, 0o600))
	done := make(chan string, 1)
	go func() { done <- f.refused(ipc.PathConfigSplit) }()
	select {
	case msg := <-done:
		assert.Contains(t, msg, "config.tui.toml")
	case <-time.After(5 * time.Second):
		t.Fatal("the split is waiting on a pipe")
	}
	assert.False(t, f.isSplit())
}
