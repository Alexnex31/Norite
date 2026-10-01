// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package gateway

import (
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/backend/internal/auth"
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

	mu sync.Mutex
	// signIn is the auth session behind the token that last identified or resumed this stream. A revocation
	// closes the streams opened with a sign-in from before it (dispatch.Revocation), and a resume with a
	// newer token moves the stream onto the newer sign-in.
	signIn snowflake.ID
	// since is when that sign-in's session family started, which with deviceID is what the liveness checks
	// ask about (auth.SignIn).
	since time.Time
	seq   int64
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

	// ended is set once the session is dropped, for any reason. Nothing resumes it afterwards.
	ended bool

	buf      []buffered
	bufBytes int
	conn     *conn
	expiry   *time.Timer
}

type buffered struct {
	seq   int64
	frame []byte
}

// deliver takes one permitted event.
func (s *session) deliver(ev dispatch.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Fan-out picked its candidates a moment ago, and this one may have been dropped since. Applying the
	// event anyway would put a dropped session back in the index on GUILD_CREATE.
	if s.ended {
		return
	}
	if !s.ready {
		s.pending = append(s.pending, ev)
		return
	}
	s.applyLocked(ev)
}

// applyLocked keeps the guild set current, then numbers and sends the event: GUILD_CREATE adds its guild
// and GUILD_DELETE removes it, so later events for that guild find, or stop finding, this session.
func (s *session) applyLocked(ev dispatch.Event) {
	// A FormerMembers event is checked against nothing but this set, since its guild's rows are gone. A
	// session that was not READY when it arrived was a candidate for it regardless of guild, so it is
	// dropped here unless READY listed the guild: otherwise an account identifying while some other guild was
	// deleted would be told that guild's id, and that it had just been deleted.
	if ev.Audience == dispatch.FormerMembers {
		if _, ok := s.guilds[ev.GuildID]; !ok {
			return
		}
	}
	switch ev.Type {
	case "GUILD_CREATE":
		s.guilds[ev.GuildID] = struct{}{}
		s.srv.idx.joined(s, ev.GuildID)
	case "GUILD_DELETE":
		delete(s.guilds, ev.GuildID)
		s.srv.idx.left(s, ev.GuildID)
	}
	// The payload was encoded once by the publisher and is the same bytes for every recipient.
	s.dispatchLocked(ev.Type, ev.Data)
}

// becomeReady sends READY as sequence 1, then whatever arrived while it was being assembled.
func (s *session) becomeReady(payload ready, guilds map[snowflake.ID]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Revoked while READY's data was being read: there is nobody to send it to, and indexing the session
	// now would keep a dropped session a fan-out candidate for good.
	if s.ended {
		return
	}
	s.guilds = guilds
	// Indexed under the guilds READY lists before anything else is applied: a candidate for every guild
	// until here, and for its own from here, with this lock ordering the switch against deliver.
	s.srv.idx.ready(s, guilds)
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
	// An event's payload arrives encoded, once for every recipient, and is used as it is: encoding it again
	// here, then again inside the envelope, cost 6.1 µs and 4.6 KB per recipient for a 2 KB message against
	// 1.5 µs and 2.5 KB for appending its bytes (BenchmarkDispatchFrame). It is valid JSON already, because the event it came in was
	// decoded whole (onEvent). READY and RESUMED are built here and encoded here.
	d, ok := payload.(json.RawMessage)
	if !ok {
		var err error
		if d, err = json.Marshal(payload); err != nil {
			s.srv.opts.Logger.Error().Err(err).Str("type", event).Msg("gateway could not encode a dispatch")
			return
		}
	}
	// Numbered only once the frame exists, so a frame that could not be built leaves no gap in the sequence.
	seq := s.seq + 1
	frame, err := appendDispatchFrame(seq, event, d)
	if err != nil {
		s.srv.opts.Logger.Error().Err(err).Str("type", event).Msg("gateway could not encode a frame envelope")
		return
	}
	s.seq = seq

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
	// resumeEnded: the session was dropped after the client looked it up, by a revocation or its expiry.
	resumeEnded
)

// resume attaches c and replays every frame after afterSeq, then RESUMED, all under the lock that numbers
// frames, so an event arriving mid-replay lands after it rather than in the middle.
//
// A connection still attached is replaced and closed: the client has evidently moved to the new one, and two
// sockets for one stream would each see half of it.
func (s *session) resume(c *conn, signIn snowflake.ID, in auth.SignIn, afterSeq int64) resumeResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Attaching to a dropped session would leave a connection bound to a stream nothing delivers to.
	if s.ended {
		return resumeEnded
	}
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
	s.signIn, s.since = signIn, in.Since
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
	// A session already dropped, by a revocation or an IDENTIFY that failed after registering it, has no
	// window to open.
	if s.ended {
		return
	}
	s.expiry = time.AfterFunc(s.srv.opts.ResumeWindow, func() { s.srv.expireSession(s) })
}

// revokedBy reports whether r ends this session.
func (s *session) revokedBy(r dispatch.Revocation) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return r.Matches(s.userID, s.signIn, s.deviceID)
}

// signInNow is the sign-in the stream currently runs on, which a resume can move.
func (s *session) signInNow() auth.SignIn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return auth.SignIn{Device: s.deviceID, Since: s.since}
}

// takeConn unbinds and returns the attached connection, if any, for a session being ended from outside.
func (s *session) takeConn() *conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.conn
	s.conn = nil
	return c
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
	if !s.ended {
		s.srv.idx.remove(s, s.guilds)
	}
	s.ended = true
}

// appendDispatchFrame builds an op 0 frame around an already-encoded payload: gatewayproto.Frame's fields,
// in its order, with d copied rather than re-encoded. A test holds it equal to encoding the struct.
func appendDispatchFrame(seq int64, event string, d json.RawMessage) ([]byte, error) {
	t, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(d)+len(t)+40)
	out = append(out, `{"op":`...)
	out = strconv.AppendInt(out, int64(gatewayproto.OpDispatch), 10)
	out = append(out, `,"d":`...)
	out = append(out, d...)
	out = append(out, `,"s":`...)
	out = strconv.AppendInt(out, seq, 10)
	out = append(out, `,"t":`...)
	out = append(out, t...)
	return append(out, '}'), nil
}
