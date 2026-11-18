// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package gateway

import (
	"context"
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
