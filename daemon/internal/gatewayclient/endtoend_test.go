// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package gatewayclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/daemon/credentials"
	"github.com/Alexnex31/Norite/daemon/internal/session"
	"github.com/Alexnex31/Norite/daemon/internal/state"
)

// M19's first done-when, with the daemon's three pieces assembled as daemonproc assembles them — the
// session, the connection, the state — against one stand-in instance serving the refresh endpoint and the
// gateway: the daemon alone, with nothing attached, stays connected and builds its state correctly. Through
// a dropped connection it resumes and keeps what it had; through a session the server cannot resume it
// starts again from nothing rather than keeping history with a hole in it.
func TestTheDaemonAloneStaysConnectedAndBuildsItsState(t *testing.T) {
	g := newFakeGateway(t)

	var mu sync.Mutex
	refreshes := 0
	g.rest = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/refresh" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		refreshes++
		n := refreshes
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fmt.Sprintf("eyJ.access.%d", n), "refresh_token": fmt.Sprintf("nrt_rotated_%d", n),
			"token_type": "Bearer", "expires_at": time.Now().Add(15 * time.Minute).Format(time.RFC3339Nano),
		})
	})

	store, err := credentials.OpenLocalForTest(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.Save(credentials.Record{
		InstanceURL: g.srv.URL, UserID: "1", Username: "ada", DeviceID: "dev_test", DeviceName: "laptop",
	}, "nrt_from_login"))

	log := zerolog.Nop()
	src := session.New(session.Options{Store: store, HTTP: g.srv.Client(), Log: log})
	st := state.New(log, state.Limits{})
	client := New(Options{
		Credentials: src, Sink: st, Version: "dev", Log: log,
		RetryMin: 5 * time.Millisecond, RetryMax: 20 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { src.Run(ctx) })
	wg.Go(func() { client.Run(ctx) })
	t.Cleanup(func() { cancel(); wg.Wait() })

	held := func(channelID string) []string {
		var out []string
		for _, m := range st.Messages(channelID) {
			out = append(out, m.Content)
		}
		return out
	}

	// Signed in, connected, and identified with the token the session renewed.
	c := g.next()
	c.hello(time.Now(), gatewayproto.DevVersion, time.Minute)
	f := c.expect(gatewayproto.OpIdentify)
	var id gatewayproto.Identify
	require.NoError(t, json.Unmarshal(f.D, &id))
	assert.Equal(t, "eyJ.access.1", id.Token)

	c.dispatch("READY", map[string]any{
		"session_id": "sess-1",
		"user": map[string]any{
			"id": "1", "username": "ada", "display_name": "Ada", "email": "ada@example.com",
			"created_at": "2026-01-01T00:00:00Z",
		},
		"guilds": []any{guild("10")},
	})
	c.dispatch("MESSAGE_CREATE", message("1", "30", "one"))
	c.dispatch("MESSAGE_CREATE", message("2", "30", "two"))
	require.Eventually(t, func() bool { return len(held("30")) == 2 }, 2*time.Second, time.Millisecond)
	user, ok := st.User()
	require.True(t, ok)
	assert.Equal(t, "Ada", user.DisplayName)
	assert.Len(t, st.Guilds(), 1)

	// The network drops. The daemon resumes, the server replays what it missed, and nothing is lost.
	c.closeWith(websocket.StatusCode(gatewayproto.CloseUnknownError), "")
	c2 := g.next()
	c2.hello(time.Now(), gatewayproto.DevVersion, time.Minute)
	f = c2.expect(gatewayproto.OpResume)
	var r gatewayproto.Resume
	require.NoError(t, json.Unmarshal(f.D, &r))
	assert.Equal(t, int64(3), r.Seq)
	c2.seq = 3
	c2.dispatch("MESSAGE_CREATE", message("3", "30", "three, replayed"))
	c2.dispatch("RESUMED", map[string]any{})
	require.Eventually(t, func() bool { return len(held("30")) == 3 }, 2*time.Second, time.Millisecond)
	assert.Equal(t, []string{"one", "two", "three, replayed"}, held("30"))

	// The server cannot resume. Whatever happened in between is unknown, so the daemon starts again from
	// nothing rather than keeping a history with a hole in it nobody can see.
	c2.closeWith(gatewayproto.CloseInvalidSeq, "")
	c3 := g.next()
	c3.hello(time.Now(), gatewayproto.DevVersion, time.Minute)
	c3.expect(gatewayproto.OpIdentify)
	require.Eventually(t, func() bool { return held("30") == nil }, 2*time.Second, time.Millisecond)
	c3.ready("sess-2")
	c3.dispatch("MESSAGE_CREATE", message("4", "30", "four"))
	require.Eventually(t, func() bool { return len(held("30")) == 1 }, 2*time.Second, time.Millisecond)
	assert.Equal(t, []string{"four"}, held("30"))

	mu.Lock()
	assert.Equal(t, 1, refreshes, "a fifteen-minute token covers the whole test; nothing refreshed for nothing")
	mu.Unlock()

	// `norite logout`. The daemon hands its token back — to an endpoint this stand-in does not serve, which
	// is the case where the instance never closes the connection — and closes it itself, forgetting the
	// account rather than keeping its messages for whoever reads the state next.
	require.NoError(t, store.Clear())
	src.Reload()
	assert.Equal(t, websocket.StatusNormalClosure, c3.closedWith())
	require.Eventually(t, func() bool { _, ok := st.User(); return !ok && held("30") == nil },
		2*time.Second, time.Millisecond)
	assert.Empty(t, st.Guilds())
}
