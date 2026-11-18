// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package gateway

import (
	"context"
	"time"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// Voice is op 4's other half (M33), the voice coordinator. The gateway decodes the frame, keeps one session's
// requests in order, and sends the reply to that session alone; everything about the call is the
// coordinator's.
type Voice interface {
	// Join answers a join with VOICE_SERVER_UPDATE or VOICE_JOIN_REFUSED, and the payload.
	Join(ctx context.Context, userID snowflake.ID, sessionID, deviceID string, v gatewayproto.VoiceStateUpdate) (string, any)
	// Leave ends the session's call.
	Leave(ctx context.Context, userID snowflake.ID, sessionID string)
	// Ended ends the call of a dropped session. It must not block: it is called under the session's lock.
	Ended(userID snowflake.ID, sessionID string)
}

// voiceWithin bounds one join or leave. A provision is a Go call on a self-hosted instance, and the
// database round trips around it are few; past this the session is answered unavailable.
const voiceWithin = 20 * time.Second

// voiceQueue bounds the requests one session may have waiting. A join under way refuses the next as busy
// rather than queuing it, so what waits is leaves, and a client sending more than this is closed.
const voiceQueue = 8

// voiceOp is one request in a session's queue.
type voiceOp struct {
	join  bool
	frame gatewayproto.VoiceStateUpdate
}

// voice takes op 4 for an identified connection, reporting whether the connection stays open.
//
// A session's requests run one at a time, in the order they arrived, on a goroutine of the session's: a
// leave sent after a join is never overtaken by it, which two goroutines racing for a lock would allow. The
// read loop never waits on one.
func (c *conn) voice(frame gatewayproto.VoiceStateUpdate) bool {
	s := c.session()
	if !frame.Leave() && c.srv.opts.Voice == nil {
		s.reply(gatewayproto.EventVoiceJoinRefused, gatewayproto.VoiceJoinRefused{
			Nonce: frame.Nonce, ChannelID: *frame.ChannelID, Code: gatewayproto.VoiceDisabled,
		})
		return true
	}
	if c.srv.opts.Voice == nil {
		return true
	}
	switch s.queueVoice(voiceOp{join: !frame.Leave(), frame: frame}) {
	case voiceQueued:
	case voiceBusy:
		s.reply(gatewayproto.EventVoiceJoinRefused, gatewayproto.VoiceJoinRefused{
			Nonce: frame.Nonce, ChannelID: *frame.ChannelID, Code: gatewayproto.VoiceBusy,
		})
	case voiceFlooded:
		c.closeWith(gatewayproto.CloseRateLimited, "too many voice requests")
		return false
	}
	return true
}

type voiceQueueResult int

const (
	voiceQueued voiceQueueResult = iota
	voiceBusy
	voiceFlooded
)

// queueVoice adds op to the session's queue, starting the goroutine that runs it if none is running.
func (s *session) queueVoice(op voiceOp) voiceQueueResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return voiceQueued
	}
	if op.join && s.voiceJoining {
		return voiceBusy
	}
	if len(s.voiceOps) >= voiceQueue {
		return voiceFlooded
	}
	if op.join {
		s.voiceJoining = true
	}
	s.voiceOps = append(s.voiceOps, op)
	if !s.voiceRunning {
		s.voiceRunning = true
		go s.runVoice()
	}
	return voiceQueued
}

// runVoice runs the session's queue until it is empty or the session has ended.
func (s *session) runVoice() {
	for {
		s.mu.Lock()
		if s.ended || len(s.voiceOps) == 0 {
			s.voiceRunning, s.voiceOps = false, nil
			s.mu.Unlock()
			return
		}
		op := s.voiceOps[0]
		s.voiceOps = s.voiceOps[1:]
		s.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), voiceWithin)
		if op.join {
			event, payload := s.srv.opts.Voice.Join(ctx, s.userID, s.id, s.deviceID, op.frame)
			s.reply(event, payload)
			s.mu.Lock()
			s.voiceJoining = false
			s.mu.Unlock()
		} else {
			s.srv.opts.Voice.Leave(ctx, s.userID, s.id)
		}
		cancel()
	}
}

// reply numbers a reply to this session's op 4 and sends it like any dispatch, so a client that reconnects
// inside the resume window has it replayed. It never crosses the bus: it answers one session, and is not an
// event about anything committed.
func (s *session) reply(event string, payload any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended || !s.ready {
		return
	}
	s.dispatchLocked(event, payload)
}
