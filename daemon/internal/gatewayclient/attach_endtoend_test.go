// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package gatewayclient

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/daemon/credentials"
	"github.com/Alexnex31/Norite/daemon/internal/attach"
	"github.com/Alexnex31/Norite/daemon/internal/session"
	"github.com/Alexnex31/Norite/daemon/internal/state"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

type refusingRelay struct{}

func (refusingRelay) Do(context.Context, ipc.Request) ipc.Response {
	return ipc.Response{Error: &ipc.RelayError{Code: ipc.RelayRefused, Message: "not in this test"}}
}

// M20's first done-when: a client attached to the daemon's socket — the one daemon/ipc gives the CLI —
// receives the same dispatches the daemon gets from the gateway. Assembled as daemonproc assembles it: the
// session, the gateway connection, and the attach server as the connection's Sink around the state, against
// a stand-in gateway that validates every frame it sends against the contract.
func TestAnAttachedClientReceivesWhatTheGatewaySends(t *testing.T) {
	g := newFakeGateway(t)
	g.rest = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/refresh" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "eyJ.access.1", "refresh_token": "nrt_rotated_1", "token_type": "Bearer",
			"expires_at": time.Now().Add(15 * time.Minute).Format(time.RFC3339Nano),
		})
	})

	store, err := credentials.OpenLocalForTest(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.Save(credentials.Record{
		InstanceURL: g.srv.URL, UserID: "1", Username: "ada", DeviceID: "dev_test", DeviceName: "laptop",
	}, "nrt_from_login"))

	dir, err := os.MkdirTemp("", "ne2e")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	l, err := attach.Listen(dir)
	require.NoError(t, err)

	log := zerolog.Nop()
	src := session.New(session.Options{Store: store, HTTP: g.srv.Client(), Log: log})
	st := state.New(log, state.DefaultLimits)
	srv := attach.New(attach.Options{Session: src, State: st, Relay: refusingRelay{}, Version: "dev", Log: log})
	client := New(Options{
		Credentials: src, Sink: srv, Version: "dev", Log: log,
		RetryMin: 5 * time.Millisecond, RetryMax: 20 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { src.Run(ctx) })
	wg.Go(func() { client.Run(ctx) })
	wg.Go(func() { srv.Serve(ctx, l) })
	t.Cleanup(func() { cancel(); wg.Wait() })

	c := g.next()
	c.hello(time.Now(), gatewayproto.DevVersion, time.Minute)
	c.expect(gatewayproto.OpIdentify)
	c.dispatch("READY", map[string]any{
		"session_id": "sess-1",
		"user": map[string]any{
			"id": "1", "username": "ada", "display_name": "Ada", "email": "ada@example.com",
			"created_at": "2026-01-01T00:00:00Z",
		},
		"guilds": []any{guild("10")},
	})
	require.Eventually(t, func() bool { return len(st.Guilds()) == 1 }, 2*time.Second, time.Millisecond)

	attachCtx, attachCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer attachCancel()
	conn, err := ipc.DialAt(attachCtx, ipc.SocketPath(dir))
	require.NoError(t, err)
	cli, err := ipc.Attach(attachCtx, conn, ipc.Options{Client: "norite-test", Version: "dev", Events: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })

	ready := cli.Ready()
	require.NotNil(t, ready.Account, "the daemon is signed in, and says as whom")
	assert.Equal(t, "1", ready.Account.UserID)
	require.Len(t, ready.Guilds, 1)
	assert.Equal(t, "10", ready.Guilds[0].ID)

	sent := []struct {
		typ  string
		data any
	}{
		{"MESSAGE_CREATE", message("1", "30", "one")},
		{"GUILD_UPDATE", guild("10")},
		{"MESSAGE_CREATE", message("2", "30", "two")},
		{"MESSAGE_DELETE", map[string]any{"id": "1", "channel_id": "30", "guild_id": "10"}},
	}
	for _, e := range sent {
		c.dispatch(e.typ, e.data)
	}
	for i, e := range sent {
		select {
		case got, ok := <-cli.Events():
			require.True(t, ok, "the attached client was closed: %v", cli.Err())
			assert.Equal(t, e.typ, got.Type)
			want, err := json.Marshal(e.data)
			require.NoError(t, err)
			assert.JSONEq(t, string(want), string(got.Data), "the payload as the gateway sent it")
		case <-time.After(5 * time.Second):
			t.Fatalf("dispatch %d (%s) never reached the attached client", i, e.typ)
		}
	}
}
