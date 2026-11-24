// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package gatewayclient

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
)

// voiceRecorder records what the client hands Options.Voice.
type voiceRecorder struct {
	mu    sync.Mutex
	types []string
}

func (r *voiceRecorder) take(eventType string, _ json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.types = append(r.types, eventType)
}

func (r *voiceRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.types...)
}

// Op 4 goes out on the established connection, held to the contract by the fake's validation, and is refused
// at once with no connection rather than waiting for one (M33).
func TestOp4IsSentOnTheEstablishedConnectionAndRefusedWithoutOne(t *testing.T) {
	h := start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	channel, video := "42", false
	join := gatewayproto.VoiceStateUpdate{Nonce: "n1", ChannelID: &channel, Offer: "v=0\r\n", SupportsVideo: &video,
		SelfMute: &video, SelfDeaf: &video}

	require.ErrorIs(t, h.client.SendVoice(ctx, join), ErrNotConnected, "before any connection")
	assert.False(t, h.client.Connected())

	c := h.g.next()
	c.hello(time.Now(), gatewayproto.DevVersion, time.Minute)
	c.expect(gatewayproto.OpIdentify)
	require.ErrorIs(t, h.client.SendVoice(ctx, join), ErrNotConnected, "before READY")
	c.ready("sess-1")
	require.Eventually(t, h.client.Connected, 5*time.Second, time.Millisecond)

	require.NoError(t, h.client.SendVoice(ctx, join))
	f := c.expect(gatewayproto.OpVoiceStateUpdate)
	got, err := gatewayproto.DecodeVoiceStateUpdate(f.D, strictJSON)
	require.NoError(t, err)
	assert.Equal(t, "n1", got.Nonce)
	assert.Equal(t, "42", *got.ChannelID)

	require.NoError(t, h.client.SendVoice(ctx, gatewayproto.VoiceStateUpdate{}))
	f = c.expect(gatewayproto.OpVoiceStateUpdate)
	assert.JSONEq(t, `{"channel_id":null}`, string(f.D), "a leave names a null channel and nothing else")

	c.closeWith(4000, "gone")
	require.Eventually(t, func() bool { return !h.client.Connected() }, 5*time.Second, time.Millisecond)
	require.ErrorIs(t, h.client.SendVoice(ctx, join), ErrNotConnected, "after the connection ended")
}

// The replies to op 4 reach Options.Voice and never the Sink, which relays to attach clients: the server's
// carries a TURN credential. The voice states reach both.
func TestTheRepliesToOp4NeverReachTheSink(t *testing.T) {
	rec := &voiceRecorder{}
	h := start(t, func(o *Options) { o.Voice = rec.take })
	c := h.g.next()
	c.handshake("sess-1")

	c.dispatch(gatewayproto.EventVoiceServerUpdate, map[string]any{
		"nonce": "n1", "guild_id": "1", "channel_id": "2", "connection_id": "3", "answer": "v=0\r\n",
		"ice_servers": []any{map[string]any{"urls": []string{"turn:chat.example.com:3478?transport=udp"},
			"username": "1700000000:9", "credential": "c2VjcmV0"}},
		"bitrate": 128000, "mute": false, "deaf": false, "suppress": false,
	})
	c.dispatch(gatewayproto.EventVoiceJoinRefused, map[string]any{"nonce": "n2", "channel_id": "2", "code": "full"})
	c.dispatch(gatewayproto.EventVoiceStateUpdate, map[string]any{
		"connection_id": "3", "guild_id": "1", "channel_id": "2", "user_id": "9", "mute": true, "deaf": false,
		"suppress": false, "self_mute": false, "self_deaf": false, "supports_video": false,
		"joined_at": "2026-01-01T00:00:00Z",
	})
	c.dispatch(gatewayproto.EventVoiceStateDelete, map[string]any{
		"connection_id": "3", "guild_id": "1", "channel_id": "2", "user_id": "9",
	})

	want := []string{gatewayproto.EventVoiceServerUpdate, gatewayproto.EventVoiceJoinRefused,
		gatewayproto.EventVoiceStateUpdate, gatewayproto.EventVoiceStateDelete}
	require.Eventually(t, func() bool { return len(rec.seen()) == len(want) }, 5*time.Second, time.Millisecond)
	assert.Equal(t, want, rec.seen())
	require.Eventually(t, func() bool { return len(h.sink.seen()) == 4 }, 5*time.Second, time.Millisecond)
	assert.Equal(t, []string{"begin", "READY", gatewayproto.EventVoiceStateUpdate, gatewayproto.EventVoiceStateDelete},
		h.sink.seen())
}

func strictJSON(data []byte, v any) error { return json.Unmarshal(data, v) }

// A session that begins afresh is told, before the connection is live, and one resumed is not: a voice call
// belongs to its session (M33), so the daemon's ends with the session the instance no longer holds. A resumed
// one is told as resumed, once the connection is live, so the call can send again what the lost connection
// may have dropped (M34's /code-review).
//
// A fresh session is told again once its connection is live (Online), after NewSession, so a call lost with
// the session before it can be joined again on it (M36); a resumed one is not, nothing having been lost.
func TestAFreshSessionIsToldAndAResumedOneIsNot(t *testing.T) {
	var fresh, resumed, online atomic.Int32
	var liveWhenTold, liveWhenResumed, liveWhenOnline, freshBeforeOnline atomic.Bool
	var h *harness
	h = start(t, func(o *Options) {
		o.NewSession = func() {
			fresh.Add(1)
			liveWhenTold.Store(h.client.Connected())
		}
		o.Resumed = func() {
			resumed.Add(1)
			liveWhenResumed.Store(h.client.Connected())
		}
		o.Online = func() {
			freshBeforeOnline.Store(fresh.Load() == online.Add(1))
			liveWhenOnline.Store(h.client.Connected())
		}
	})
	c := h.g.next()
	c.handshake("sess-1")
	require.Eventually(t, func() bool { return fresh.Load() == 1 }, 5*time.Second, time.Millisecond)
	assert.False(t, liveWhenTold.Load(), "told after the connection was live, where a join could be sent first")
	require.Eventually(t, func() bool { return online.Load() == 1 }, 5*time.Second, time.Millisecond,
		"a fresh session's live connection was not told")
	assert.True(t, liveWhenOnline.Load(), "told online before the connection was live, where a join is refused")
	assert.True(t, freshBeforeOnline.Load(), "told online before the session was told it was fresh")

	c.closeWith(websocket.StatusCode(gatewayproto.CloseUnknownError), "")
	c2 := h.g.next()
	c2.hello(time.Now(), gatewayproto.DevVersion, time.Minute)
	c2.expect(gatewayproto.OpResume)
	c2.seq = 1
	c2.dispatch("RESUMED", map[string]any{})
	require.Eventually(t, h.client.Connected, 5*time.Second, time.Millisecond)
	assert.Equal(t, int32(1), fresh.Load(), "a resumed session was told it was fresh")
	assert.Equal(t, int32(1), online.Load(), "a resumed session was told online, as a fresh one is")
	require.Eventually(t, func() bool { return resumed.Load() == 1 }, 5*time.Second, time.Millisecond,
		"a resumed session was not told")
	assert.True(t, liveWhenResumed.Load(), "told before the connection was live, where nothing could be sent")

	c2.closeWith(websocket.StatusCode(gatewayproto.CloseUnknownError), "")
	c3 := h.g.next()
	c3.hello(time.Now(), gatewayproto.DevVersion, time.Minute)
	c3.expect(gatewayproto.OpResume)
	c3.send(gatewayproto.OpInvalidSess, false, nil, nil)
	c3.expect(gatewayproto.OpIdentify)
	c3.ready("sess-2")
	require.Eventually(t, func() bool { return fresh.Load() == 2 }, 5*time.Second, time.Millisecond,
		"a session that could not be resumed was not told")
	require.Eventually(t, func() bool { return online.Load() == 2 }, 5*time.Second, time.Millisecond,
		"a session that could not be resumed was not told online")
	assert.Equal(t, int32(1), resumed.Load(), "a fresh session was told it was resumed")
}
