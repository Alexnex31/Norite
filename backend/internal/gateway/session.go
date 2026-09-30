// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package gateway

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/backend/internal/dispatch"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// session is what IDENTIFY creates and RESUME continues: an account's event stream, which outlives any one
// connection to it.
//
// Everything that has to survive a disconnect lives here rather than on the connection: the sequence
// numbers, the guilds the stream follows, and the frames sent recently enough to be replayed. A session
// with no connection keeps receiving events into its buffer for ResumeWindow, so a client that reconnects
// in time and resumes loses nothing, which is the done-when's third clause.
//
// One lock orders everything that numbers a frame. A frame's sequence number, its place in the replay
// buffer and its place in the attached connection's queue are decided together, so the socket, the buffer
// and a replay can never disagree about what came after what.
type session struct {
	srv      *Server
	id       string
	userID   snowflake.ID
	deviceID string

	mu  sync.Mutex
	seq int64
	// guilds is what READY listed, kept current by GUILD_CREATE and GUILD_DELETE. It picks which events
	// this session is a candidate for and nothing more: whether it may receive one is decided against the
	// database at fan-out (see onEvent), because this is exactly what goes stale.
	guilds map[snowflake.ID]struct{}
	// ready is false from the moment the session becomes a fan-out candidate until READY is sent. Events
	// arriving in between wait in pending. That order is what closes the gap between reading READY's data
	// and becoming a candidate: register first and an event committed in between is held rather than
	// lost; register second and it is lost.
	ready   bool
	pending []dispatch.Event

	buf      []buffered
	bufBytes int
	conn     *conn
	expiry   *time.Timer
}

type buffered struct {
	seq   int64
	frame []byte
}

// candidate reports whether an event about guildID could be for this session. A session not yet READY is a
// candidate for everything, because it does not know its guilds yet; the audience check decides, as for
// every event.
func (s *session) candidate(guildID snowflake.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return true
	}
	_, ok := s.guilds[guildID]
	return ok
}

// deliver takes one permitted event.
func (s *session) deliver(ev dispatch.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		s.pending = append(s.pending, ev)
		return
	}
	s.applyLocked(ev)
}

// applyLocked keeps the guild set current, then numbers and sends the event: GUILD_CREATE adds its guild
// and GUILD_DELETE removes it, so later events for that guild find, or stop finding, this session.
func (s *session) applyLocked(ev dispatch.Event) {
	switch ev.Type {
	case "GUILD_CREATE":
		s.guilds[ev.GuildID] = struct{}{}
	case "GUILD_DELETE":
		delete(s.guilds, ev.GuildID)
	}
	// The payload was encoded once by the publisher and is the same bytes for every recipient.
	s.dispatchLocked(ev.Type, ev.Data)
}

// becomeReady sends READY as sequence 1, then whatever arrived while it was being assembled.
func (s *session) becomeReady(payload ready, guilds map[snowflake.ID]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.guilds = guilds
	s.dispatchLocked("READY", payload)
	s.ready = true
	for _, ev := range s.pending {
		s.applyLocked(ev)
	}
	s.pending = nil
}

// dispatchLocked numbers a dispatch, keeps it for replay, and queues it on the attached connection if there
// is one.
func (s *session) dispatchLocked(event string, payload any) {
	d, err := json.Marshal(payload)
	if err != nil {
		s.srv.opts.Logger.Error().Err(err).Str("type", event).Msg("gateway could not encode a dispatch")
		return
	}
	s.seq++
	seq, t := s.seq, event
	frame, err := json.Marshal(gatewayproto.Frame{Op: gatewayproto.OpDispatch, D: d, S: &seq, T: &t})
	if err != nil {
		s.srv.opts.Logger.Error().Err(err).Str("type", event).Msg("gateway could not encode a frame envelope")
		return
	}

	s.buf = append(s.buf, buffered{seq: seq, frame: frame})
	s.bufBytes += len(frame)
	// Bounded by count and by bytes, oldest first. A client that falls further behind than this cannot be
	// replayed to and is told to identify afresh; the bound is what stops a detached session holding an
	// unbounded share of the server's memory for a client that may never come back.
	for len(s.buf) > s.srv.opts.ResumeBuffer || s.bufBytes > maxResumeBytes {
		s.bufBytes -= len(s.buf[0].frame)
		s.buf = s.buf[1:]
	}

	if s.conn != nil {
		s.conn.enqueueFrame(frame)
	}
}

// resumeResult is what an attempt to resume found.
type resumeResult int

const (
	resumed resumeResult = iota
	// resumeInvalidSeq: the client claims a frame the server never sent.
	resumeInvalidSeq
	// resumeGap: the buffer no longer reaches back to the client's last frame.
	resumeGap
)

// resume attaches c and replays every frame after afterSeq, then RESUMED, all under the lock that numbers
// frames, so an event arriving mid-replay lands after it rather than in the middle.
//
// A connection still attached is replaced and closed: the client has evidently moved to the new one, and two
// sockets for one stream would each see half of it.
func (s *session) resume(c *conn, afterSeq int64) resumeResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	if afterSeq > s.seq {
		return resumeInvalidSeq
	}
	oldest := s.seq + 1
	if len(s.buf) > 0 {
		oldest = s.buf[0].seq
	}
	if afterSeq < oldest-1 {
		return resumeGap
	}

	if s.expiry != nil {
		s.expiry.Stop()
		s.expiry = nil
	}
	if old := s.conn; old != nil && old != c {
		go old.closeWith(gatewayproto.CloseSessionTimedOut, "resumed on another connection")
	}
	s.conn = c
	for _, b := range s.buf {
		if b.seq > afterSeq {
			c.enqueueFrame(b.frame)
		}
	}
	s.dispatchLocked("RESUMED", struct{}{})
	return resumed
}

// attach binds the connection that identified it.
func (s *session) attach(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conn = c
}

// detach unbinds c if it is still the attached connection, and starts the window in which the session may
// be resumed. When it passes unresumed, the session is gone.
func (s *session) detach(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != c {
		return
	}
	s.conn = nil
	s.expiry = time.AfterFunc(s.srv.opts.ResumeWindow, func() { s.srv.expireSession(s) })
}

// stop ends the session's timer, for a session being dropped for any reason.
func (s *session) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.expiry != nil {
		s.expiry.Stop()
		s.expiry = nil
	}
	s.buf, s.pending = nil, nil
}
