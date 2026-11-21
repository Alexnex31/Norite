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
	// Update records what the caller has set on themselves in the session's call (M34).
	Update(ctx context.Context, userID snowflake.ID, sessionID string, v gatewayproto.VoiceStateUpdate)
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

// updateEvery spaces a session's updates (M34). Each is a write and an event to everyone who can see the
// channel, so a client toggling as fast as it can send would otherwise be a broadcast it controls; a
// person's toggles are seconds apart. An update waiting for its turn is replaced by a newer one, so what is
// lost is the states between, never the last.
const updateEvery = 250 * time.Millisecond

// voiceOp is one request in a session's queue.
type voiceOp struct {
	join, update bool
	frame        gatewayproto.VoiceStateUpdate
}

// voice takes op 4 for an identified connection, reporting whether the connection stays open.
//
// A session's requests run one at a time, in the order they arrived, on a goroutine of the session's: a
// leave sent after a join is never overtaken by it, which two goroutines racing for a lock would allow. The
// read loop never waits on one.
func (c *conn) voice(frame gatewayproto.VoiceStateUpdate) bool {
	s := c.session()
	if !frame.Leave() && !frame.Update() && c.srv.opts.Voice == nil {
		s.reply(gatewayproto.EventVoiceJoinRefused, gatewayproto.VoiceJoinRefused{
			Nonce: frame.Nonce, ChannelID: *frame.ChannelID, Code: gatewayproto.VoiceDisabled,
		})
		return true
	}
	if c.srv.opts.Voice == nil {
		return true
	}
	if frame.Update() {
		// An update has no reply, so a full queue is the only thing to say, and coalescing keeps updates
		// from filling it.
		if s.queueVoice(voiceOp{update: true, frame: frame}) == voiceFlooded {
			c.closeWith(gatewayproto.CloseRateLimited, "too many voice requests")
			return false
		}
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
	// Newest wins over an update still waiting, when it is the last request queued: one behind a join or a
	// leave is for whatever that request leaves the session in, and is queued after it.
	if n := len(s.voiceOps); op.update && n > 0 && s.voiceOps[n-1].update {
		s.voiceOps[n-1] = op
		return voiceQueued
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
		// An update waits for its turn in the queue, where a newer one can still replace it.
		if wait := time.Until(s.voiceUpdated.Add(updateEvery)); op.update && wait > 0 {
			s.mu.Unlock()
			time.Sleep(wait)
			continue
		}
		s.voiceOps = s.voiceOps[1:]
		if op.update {
			s.voiceUpdated = time.Now()
		}
		s.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), voiceWithin)
		if op.join {
			event, payload := s.srv.opts.Voice.Join(ctx, s.userID, s.id, s.deviceID, op.frame)
			s.reply(event, payload)
			s.mu.Lock()
			s.voiceJoining = false
			ended := s.ended
			s.mu.Unlock()
			// The session ended while the join ran: stop told the voice side before the join had registered,
			// so the call it then made belongs to a session nobody holds. It is told again, and finds it
			// (M33's /code-review).
			if ended {
				s.srv.opts.Voice.Ended(s.userID, s.id)
			}
		} else if op.update {
			s.srv.opts.Voice.Update(ctx, s.userID, s.id, op.frame)
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
