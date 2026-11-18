// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package gatewayclient

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

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
	join := gatewayproto.VoiceStateUpdate{Nonce: "n1", ChannelID: &channel, Offer: "v=0\r\n", SupportsVideo: &video}

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
		"suppress": false, "supports_video": false, "joined_at": "2026-01-01T00:00:00Z",
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
