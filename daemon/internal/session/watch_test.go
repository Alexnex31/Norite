// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The watch is what tells a running daemon about `norite login` and `norite logout`. Neither test calls
// Reload: only the watch can, which is the property under test.

func (h *harness) watch() {
	ctx, cancel := context.WithCancel(h.t.Context())
	done := make(chan error, 1)
	go func() { done <- h.src.Watch(ctx) }()
	h.t.Cleanup(func() {
		cancel()
		require.NoError(h.t, <-done)
	})
	// fsnotify's watch is in place once NewWatcher and Add have returned, which Watch does before its loop;
	// a short real pause lets it get there before the test changes anything.
	time.Sleep(50 * time.Millisecond)
}

// A logout revokes nothing at the instance, so before the watch a running daemon went on streaming the
// account's messages until its next renewal found no record. Now it hands its token back and stops at once.
func TestALogoutReachesARunningDaemonAtOnce(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.watch()
	_, err := h.current()
	require.NoError(t, err)

	require.NoError(t, h.store.Clear()) // what `norite logout` does
	require.Eventually(t, func() bool { return h.f.handedBackToken() == "nrt_rotated_1" },
		3*time.Second, 5*time.Millisecond, "the daemon's live token must be handed back")
	h.noCredential()
	assert.Len(t, h.f.refreshes(), 1, "noticed without waiting for a renewal")
}

// A signed-out daemon holds no connection, so there is no 4011 to tell it somebody signed in.
func TestALoginReachesASignedOutDaemon(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.store.Clear())
	h.start()
	h.watch()
	h.noCredential()

	loginTo(t, h.store, h.f.server.URL, "nrt_new_login")
	require.Eventually(t, func() bool { c, ok := h.tryCurrent(); return ok && c.Username == "grace" },
		3*time.Second, 5*time.Millisecond)
	assert.Equal(t, []string{"nrt_new_login"}, h.f.refreshes())
}
