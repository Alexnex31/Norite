// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package gateway

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// holdingVoice holds each join until released, and counts the ends it is told of.
type holdingVoice struct {
	started, release chan struct{}
	mu               sync.Mutex
	ended            int
	endedAfterJoin   bool
	joinReturned     bool
}

func (v *holdingVoice) Join(context.Context, snowflake.ID, string, string, gatewayproto.VoiceStateUpdate) (string, any) {
	close(v.started)
	<-v.release
	v.mu.Lock()
	v.joinReturned = true
	v.mu.Unlock()
	return gatewayproto.EventVoiceServerUpdate, struct{}{}
}

func (v *holdingVoice) Leave(context.Context, snowflake.ID, string) {}

func (v *holdingVoice) Update(context.Context, snowflake.ID, string, gatewayproto.VoiceStateUpdate) {}

func (v *holdingVoice) Ended(snowflake.ID, string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.ended++
	if v.joinReturned {
		v.endedAfterJoin = true
	}
}

// A session that ends while its join runs is told to the voice side again once the join returns: stop
// told it before the join had registered, so the call the join made would otherwise belong to nobody
// (M33's /code-review).
func TestASessionEndedDuringItsJoinEndsTheCallTheJoinMade(t *testing.T) {
	v := &holdingVoice{started: make(chan struct{}), release: make(chan struct{})}
	srv := &Server{opts: Options{Logger: zerolog.Nop(), ResumeBuffer: DefaultResumeBuffer, Voice: v}}
	s := &session{srv: srv, id: "a", userID: 1}
	srv.idx.register(s)

	channel := "42"
	require.Equal(t, voiceQueued, s.queueVoice(voiceOp{join: true, frame: gatewayproto.VoiceStateUpdate{
		Nonce: "n", ChannelID: &channel}}))
	select {
	case <-v.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the join never ran")
	}
	s.stop()
	close(v.release)
	require.Eventually(t, func() bool {
		v.mu.Lock()
		defer v.mu.Unlock()
		return v.endedAfterJoin
	}, 5*time.Second, time.Millisecond, "the call a join made for an ended session was never ended")
}

// recordingVoice records the order of the requests it runs, and when each update ran.
type recordingVoice struct {
	release chan struct{}
	mu      sync.Mutex
	ran     []string
	updated []time.Time
}

func (v *recordingVoice) Join(_ context.Context, _ snowflake.ID, _, _ string, f gatewayproto.VoiceStateUpdate) (string, any) {
	<-v.release
	v.note("join " + *f.ChannelID)
	return gatewayproto.EventVoiceServerUpdate, struct{}{}
}

func (v *recordingVoice) Update(_ context.Context, _ snowflake.ID, _ string, f gatewayproto.VoiceStateUpdate) {
	v.mu.Lock()
	v.updated = append(v.updated, time.Now())
	v.mu.Unlock()
	v.note(fmt.Sprintf("update %s %t %t", *f.ChannelID, *f.SelfMute, *f.SelfDeaf))
}

func (v *recordingVoice) Leave(context.Context, snowflake.ID, string) { v.note("leave") }
func (v *recordingVoice) Ended(snowflake.ID, string)                  {}

func (v *recordingVoice) note(s string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.ran = append(v.ran, s)
}

func (v *recordingVoice) seen() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.ran)
}

func update(channel string, mute, deaf bool) voiceOp {
	return voiceOp{update: true, frame: gatewayproto.VoiceStateUpdate{ChannelID: &channel, SelfMute: &mute, SelfDeaf: &deaf}}
}

func voiceSession(v Voice) *session {
	srv := &Server{opts: Options{Logger: zerolog.Nop(), ResumeBuffer: DefaultResumeBuffer, Voice: v}}
	s := &session{srv: srv, id: "a", userID: 1}
	srv.idx.register(s)
	return s
}

// A session toggling as fast as it can send is a write and an event per toggle to everyone in the guild,
// unless its updates are spaced. They are, and the newest waiting replaces the one before, so the last
// state always lands (M34).
func TestUpdatesAreSpacedAndTheNewestWins(t *testing.T) {
	v := &recordingVoice{release: make(chan struct{})}
	s := voiceSession(v)
	// The first runs at once; the rest come every 10 ms for 400 ms, a client toggling as fast as it can.
	require.Equal(t, voiceQueued, s.queueVoice(update("42", false, false)))
	require.Eventually(t, func() bool { return len(v.seen()) == 1 }, 5*time.Second, time.Millisecond)
	for i := range 40 {
		require.Equal(t, voiceQueued, s.queueVoice(update("42", i%2 == 0, false)))
		time.Sleep(10 * time.Millisecond)
	}
	require.Equal(t, voiceQueued, s.queueVoice(update("42", true, true)))
	require.Eventually(t, func() bool {
		seen := v.seen()
		return len(seen) > 0 && seen[len(seen)-1] == "update 42 true true"
	}, 5*time.Second, time.Millisecond, "the last update never ran")
	v.mu.Lock()
	defer v.mu.Unlock()
	require.LessOrEqual(t, len(v.updated), 4, "every toggle ran: %v", v.ran)
	for i := 1; i < len(v.updated); i++ {
		require.GreaterOrEqual(t, v.updated[i].Sub(v.updated[i-1]), updateEvery-5*time.Millisecond)
	}
}

// An update queued behind a join is for the call the join makes, and is never folded into one queued
// before it, which would run it ahead of the join and against the old call.
func TestAnUpdateIsNeverFoldedAcrossAJoin(t *testing.T) {
	v := &recordingVoice{release: make(chan struct{})}
	close(v.release)
	s := voiceSession(v)
	b := "43"
	// The first update runs at once, so the second waits its turn at the head of the queue, with the join
	// and the third behind it.
	require.Equal(t, voiceQueued, s.queueVoice(update("42", true, false)))
	require.Eventually(t, func() bool { return len(v.seen()) == 1 }, 5*time.Second, time.Millisecond)
	require.Equal(t, voiceQueued, s.queueVoice(update("42", false, false)))
	require.Equal(t, voiceQueued, s.queueVoice(voiceOp{join: true, frame: gatewayproto.VoiceStateUpdate{
		Nonce: "n", ChannelID: &b}}))
	require.Equal(t, voiceQueued, s.queueVoice(update("43", true, true)))
	require.Eventually(t, func() bool { return len(v.seen()) == 4 }, 5*time.Second, time.Millisecond)
	require.Equal(t, []string{"update 42 true false", "update 42 false false", "join 43", "update 43 true true"},
		v.seen())
}
