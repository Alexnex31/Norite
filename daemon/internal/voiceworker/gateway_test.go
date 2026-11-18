// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceworker

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/daemon/voiceipc"
)

// fakeLink is the gateway connection: it records what op 4 sends, and answers through reply when set.
type fakeLink struct {
	mu        sync.Mutex
	connected bool
	sent      []gatewayproto.VoiceStateUpdate
	reply     func(gatewayproto.VoiceStateUpdate)
}

func (l *fakeLink) SendVoice(_ context.Context, v gatewayproto.VoiceStateUpdate) error {
	l.mu.Lock()
	if !l.connected {
		l.mu.Unlock()
		return errors.New("not connected")
	}
	l.sent = append(l.sent, v)
	reply := l.reply
	l.mu.Unlock()
	// A leave has no reply.
	if reply != nil && v.ChannelID != nil {
		go reply(v)
	}
	return nil
}

func (l *fakeLink) Connected() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.connected
}

func deliver(t *testing.T, g *Gateway, event string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	g.Deliver(event, raw)
}

func server(nonce, channel string) gatewayproto.VoiceServer {
	return gatewayproto.VoiceServer{Nonce: nonce, GuildID: "1", ChannelID: channel, ConnectionID: "77",
		Answer: "v=0\r\nanswer\r\n", Bitrate: 128000, Suppress: true,
		ICEServers: []gatewayproto.ICEServer{{URLs: []string{"turn:chat.example.com:3478?transport=udp"},
			Username: "1700000000:9", Credential: "c2VjcmV0"}}}
}

// A join is op 4 with the offer, answered by the reply carrying its nonce; the account's rights come with
// it, and later ones for its own connection reach the call (M33).
func TestTheGatewaySignalerJoinsByOp4(t *testing.T) {
	link := &fakeLink{connected: true}
	g := &Gateway{Link: link, Instance: func() string { return "https://chat.example.com" }}
	link.reply = func(v gatewayproto.VoiceStateUpdate) {
		stale := server("not this one", *v.ChannelID)
		stale.Answer = "v=0\r\nstale\r\n"
		deliver(t, g, gatewayproto.EventVoiceServerUpdate, stale)
		deliver(t, g, gatewayproto.EventVoiceServerUpdate, server(v.Nonce, *v.ChannelID))
	}

	route, err := g.Prepare(context.Background(), "9")
	require.NoError(t, err)
	assert.False(t, route.Loopback, "an instance elsewhere admits no loopback candidate")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := g.Join(ctx, "9", "42", "v=0\r\noffer\r\n")
	require.NoError(t, err)
	assert.Equal(t, "v=0\r\nanswer\r\n", a.SDP)
	assert.Equal(t, 128000, a.Bitrate)
	assert.Equal(t, voiceipc.Rights{Send: false, Hear: true}, a.Rights, "suppressed: the account may not send")
	require.Len(t, a.ICEServers, 1)
	assert.Equal(t, "1700000000:9", a.ICEServers[0].Username)

	link.mu.Lock()
	sent := link.sent[0]
	link.mu.Unlock()
	assert.True(t, gatewayproto.ValidNonce(sent.Nonce))
	assert.Equal(t, "v=0\r\noffer\r\n", sent.Offer)
	assert.False(t, *sent.SupportsVideo, "a terminal client takes no video")

	// Its own connection's state reaches the call; another connection's says nothing of it, read on its own
	// so that the latest-only channel cannot hide it behind a later one.
	deliver(t, g, gatewayproto.EventVoiceStateUpdate, map[string]any{"connection_id": "77", "deaf": true})
	select {
	case r := <-a.Changes:
		assert.Equal(t, voiceipc.Rights{Send: true, Hear: false}, r)
	case <-time.After(5 * time.Second):
		t.Fatal("the call's rights did not change")
	}
	deliver(t, g, gatewayproto.EventVoiceStateUpdate, map[string]any{"connection_id": "78", "mute": true})
	select {
	case r := <-a.Changes:
		t.Fatalf("another connection's state reached the call: %+v", r)
	default:
	}

	g.Leave(context.Background(), "9")
	link.mu.Lock()
	defer link.mu.Unlock()
	require.Len(t, link.sent, 2)
	assert.Nil(t, link.sent[1].ChannelID, "a leave names no channel")
}

// A refusal is worded for a person from its code; an answer for another channel, or past a bound, is not
// believed; no connection refuses before any worker is started.
func TestTheGatewaySignalerRefuses(t *testing.T) {
	link := &fakeLink{}
	g := &Gateway{Link: link, Instance: func() string { return "http://127.0.0.1:8080" }}
	_, err := g.Prepare(context.Background(), "9")
	require.ErrorContains(t, err, "not connected")

	link.connected = true
	route, err := g.Prepare(context.Background(), "9")
	require.NoError(t, err)
	assert.True(t, route.Loopback, "an instance on this machine announces loopback addresses")

	for name, tc := range map[string]struct {
		event string
		reply func(v gatewayproto.VoiceStateUpdate) any
		want  string
	}{
		"full": {gatewayproto.EventVoiceJoinRefused, func(v gatewayproto.VoiceStateUpdate) any {
			return gatewayproto.VoiceJoinRefused{Nonce: v.Nonce, ChannelID: *v.ChannelID, Code: gatewayproto.VoiceFull}
		}, "the voice channel is full"},
		"a code nobody knows": {gatewayproto.EventVoiceJoinRefused, func(v gatewayproto.VoiceStateUpdate) any {
			return gatewayproto.VoiceJoinRefused{Nonce: v.Nonce, ChannelID: *v.ChannelID, Code: "\x1b[31mevil"}
		}, "the instance refused the join"},
		"another channel": {gatewayproto.EventVoiceServerUpdate, func(v gatewayproto.VoiceStateUpdate) any {
			return server(v.Nonce, "43")
		}, "shape"},
		"too many servers": {gatewayproto.EventVoiceServerUpdate, func(v gatewayproto.VoiceStateUpdate) any {
			s := server(v.Nonce, *v.ChannelID)
			s.ICEServers = make([]gatewayproto.ICEServer, voiceipc.MaxICEServers+1)
			for i := range s.ICEServers {
				s.ICEServers[i] = gatewayproto.ICEServer{URLs: []string{"turn:a"}}
			}
			return s
		}, "past a bound"},
		"a field nobody declared": {gatewayproto.EventVoiceServerUpdate, func(v gatewayproto.VoiceStateUpdate) any {
			return map[string]any{"nonce": v.Nonce, "channel_id": *v.ChannelID, "token": "x"}
		}, "shape"},
	} {
		t.Run(name, func(t *testing.T) {
			link.mu.Lock()
			link.reply = func(v gatewayproto.VoiceStateUpdate) { deliver(t, g, tc.event, tc.reply(v)) }
			link.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := g.Join(ctx, "9", "42", "v=0\r\n")
			require.ErrorContains(t, err, tc.want)
		})
	}

	link.mu.Lock()
	link.reply = nil
	link.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = g.Join(ctx, "9", "42", "v=0\r\n")
	require.ErrorContains(t, err, "did not answer", "an instance that never answers")
}

// A session that begins afresh ends what the old one held (M33's manual pass, where a daemon kept a call
// its restarted instance no longer held for 32 s): a join waiting for its reply fails at once, and an
// answered call's Lost closes. A call left before is not touched, nor is the next one.
func TestALostSessionEndsTheCallAndTheJoinUnderWay(t *testing.T) {
	link := &fakeLink{connected: true}
	g := &Gateway{Link: link}

	failed := make(chan error, 1)
	go func() {
		_, err := g.Join(context.Background(), "9", "42", "v=0\r\n")
		failed <- err
	}()
	require.Eventually(t, func() bool {
		link.mu.Lock()
		defer link.mu.Unlock()
		return len(link.sent) == 1
	}, 5*time.Second, time.Millisecond)
	g.SessionLost()
	select {
	case err := <-failed:
		require.ErrorContains(t, err, "lost during the join")
	case <-time.After(5 * time.Second):
		t.Fatal("a join under way waited out its bound for a session that is gone")
	}

	link.mu.Lock()
	link.reply = func(v gatewayproto.VoiceStateUpdate) {
		deliver(t, g, gatewayproto.EventVoiceServerUpdate, server(v.Nonce, *v.ChannelID))
	}
	link.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := g.Join(ctx, "9", "42", "v=0\r\n")
	require.NoError(t, err)
	select {
	case <-a.Lost:
		t.Fatal("an answered call was lost before its session was")
	default:
	}
	g.SessionLost()
	select {
	case <-a.Lost:
	case <-time.After(5 * time.Second):
		t.Fatal("the call's session was lost and the call was not told")
	}
	g.SessionLost() // a second fresh session, with no call: nothing to close twice

	b, err := g.Join(ctx, "9", "42", "v=0\r\n")
	require.NoError(t, err)
	g.Leave(context.Background(), "9")
	g.SessionLost()
	select {
	case <-b.Lost:
		t.Fatal("a call already left was told its session was lost")
	default:
	}
}

// The account's own VOICE_STATE_UPDATE can arrive before the reply it follows, since the reply goes
// straight to the session and the update through the bus: what it says wins over the reply's rights, which
// were read as the row was written. Another account's, or another channel's, says nothing of the call
// (M33's /code-review: a mute set during the join was lost).
func TestAnUpdateBeforeTheReplyIsTheCallsRights(t *testing.T) {
	link := &fakeLink{connected: true}
	g := &Gateway{Link: link}
	link.reply = func(v gatewayproto.VoiceStateUpdate) {
		deliver(t, g, gatewayproto.EventVoiceStateUpdate, map[string]any{
			"connection_id": "77", "user_id": "9", "channel_id": *v.ChannelID, "mute": true})
		// After it, so that taking them would overwrite it.
		deliver(t, g, gatewayproto.EventVoiceStateUpdate, map[string]any{
			"connection_id": "77", "user_id": "8", "channel_id": *v.ChannelID, "deaf": true})
		deliver(t, g, gatewayproto.EventVoiceStateUpdate, map[string]any{
			"connection_id": "77", "user_id": "9", "channel_id": "43", "deaf": true})
		s := server(v.Nonce, *v.ChannelID)
		s.Suppress = false
		deliver(t, g, gatewayproto.EventVoiceServerUpdate, s)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := g.Join(ctx, "9", "42", "v=0\r\noffer\r\n")
	require.NoError(t, err)
	assert.Equal(t, voiceipc.Rights{Send: false, Hear: true}, a.Rights,
		"the mute that came before the reply was lost, or another's update was taken")
}
