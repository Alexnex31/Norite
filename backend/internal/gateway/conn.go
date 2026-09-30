// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/guilds"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// conn is one client's socket, from the upgrade until it closes.
//
// It owns the socket and nothing that outlives it: control frames, deadlines and limits. The event stream
// belongs to a session, which a connection is attached to by IDENTIFY or RESUME and detached from when it
// closes, so a reconnecting client can pick the stream up where it left off.
//
// Three goroutines touch it: serve reads frames and answers them, writeLoop drains out onto the socket, and
// the watchdog timer closes it when a deadline passes. Closing is once-only, so any of them may end it.
type conn struct {
	srv *Server
	ws  *websocket.Conn
	log zerolog.Logger

	// id is unique for the life of the process. It keys this connection's frame limiter, and it must not
	// be the connection's address: a closed connection's memory is reused by a later one, which would then
	// inherit the dead connection's frame count and be refused on its first frame. That is not
	// hypothetical: it made a test fail intermittently, after a flooded connection's address came back.
	id uint64

	out  chan []byte
	done chan struct{}

	// unwritten counts frames queued and not yet on the socket. The channel's length is not enough: the
	// writer takes a frame off it before writing it, so for that moment the channel is empty and the frame
	// is nowhere a close could wait for. Waiting on this instead is what stops a close overtaking the
	// Reconnect it follows.
	unwritten atomic.Int64

	closeOnce sync.Once
	watchdog  *time.Timer

	mu   sync.Mutex
	sess *session

	// checkedAt is when the sign-in last answered that it is live. Touched only by serve's goroutine.
	checkedAt time.Time

	// addr is the address key holding one of the server's unidentified slots for this connection, until
	// markIdentified gives it back, once.
	addr         string
	slotReleased atomic.Bool
}

// markIdentified gives back this connection's unidentified slot. Idempotent, since identifying and closing
// both call it.
func (c *conn) markIdentified() {
	if c.slotReleased.CompareAndSwap(false, true) {
		c.srv.identified(c.addr)
	}
}

func newConn(s *Server, ws *websocket.Conn, log *zerolog.Logger) *conn {
	return &conn{
		srv:  s,
		ws:   ws,
		id:   s.nextConnID.Add(1),
		log:  log.With().Str("component", "gateway").Logger(),
		out:  make(chan []byte, outboundBuffer),
		done: make(chan struct{}),
	}
}

func (c *conn) session() *session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess
}

func (c *conn) serve(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer close(c.done)

	go c.writeLoop(ctx)

	// A panic here is contained to this connection. The router's Recoverer covers the handler goroutine,
	// and this is that goroutine, but a panic that escaped would still take the connection's cleanup with
	// it; recovering closes it properly and leaves every other connection alone.
	defer func() {
		if p := recover(); p != nil {
			c.log.Error().Interface("panic", p).Msg("gateway connection panicked")
			c.closeWith(gatewayproto.CloseUnknownError, "internal error")
		}
	}()

	// Whatever the connection was attached to keeps its stream, for ResumeWindow.
	defer func() {
		if s := c.session(); s != nil {
			s.detach(c)
		}
	}()

	c.watchdog = time.AfterFunc(c.srv.opts.IdentifyTimeout, func() {
		c.closeWith(gatewayproto.CloseSessionTimedOut, "no IDENTIFY in time")
	})
	defer c.watchdog.Stop()

	c.send(gatewayproto.OpHello, gatewayproto.Hello{
		HeartbeatInterval: c.srv.opts.HeartbeatInterval.Milliseconds(),
		ServerTime:        c.srv.opts.Now().UTC(),
		Version:           c.srv.opts.Version,
	})

	frameKey := "conn:" + strconv.FormatUint(c.id, 10)
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			return
		}
		if res, err := c.srv.frameLimiter.Allow(ctx, frameKey); err == nil && !res.Allowed {
			c.closeWith(gatewayproto.CloseRateLimited, "too many frames")
			return
		}
		if typ != websocket.MessageText {
			c.closeWith(gatewayproto.CloseDecodeError, "frames are JSON text")
			return
		}
		var f gatewayproto.Frame
		if err := strictDecode(data, &f); err != nil {
			c.closeWith(gatewayproto.CloseDecodeError, "not a gateway frame")
			return
		}
		if !c.handle(ctx, f) {
			return
		}
	}
}

// handle answers one frame, reporting whether the connection stays open.
func (c *conn) handle(ctx context.Context, f gatewayproto.Frame) bool {
	identified := c.session() != nil

	switch f.Op {
	case gatewayproto.OpHeartbeat:
		// Only an identified connection earns a longer deadline. Before IDENTIFY the identify timeout stands,
		// or heartbeating alone would hold a connection open unauthenticated for as long as a client liked.
		if identified {
			if !c.stillSignedIn(ctx) {
				return false
			}
			c.watchdog.Reset(c.heartbeatDeadline())
		}
		c.send(gatewayproto.OpHeartbeatAck, nil)
		return true

	case gatewayproto.OpIdentify:
		if identified {
			c.closeWith(gatewayproto.CloseAlreadyAuthenticated, "already identified")
			return false
		}
		return c.identify(ctx, f.D)

	case gatewayproto.OpResume:
		if identified {
			c.closeWith(gatewayproto.CloseAlreadyAuthenticated, "already identified")
			return false
		}
		return c.resume(ctx, f.D)
	}

	if !identified {
		c.closeWith(gatewayproto.CloseNotAuthenticated, "identify first")
		return false
	}
	// Presence (M38), voice state (Phase E) and member requests are reserved, and every other number is
	// unknown. Refused rather than ignored: a client believing its presence update landed is worse off than
	// one told it did not.
	c.closeWith(gatewayproto.CloseUnknownOpcode, fmt.Sprintf("op %d is not supported", f.Op))
	return false
}

// authenticate is what IDENTIFY and RESUME both ask of a token: that it is an access token, that its
// account has not identified too often, and which sign-in it belongs to. It closes the connection saying why
// when the answer is no. Whether that sign-in is still live is checkLive's, asked separately so IDENTIFY can
// register the session in between.
//
// Shared, so the two cannot drift apart. RESUME skipping any of these would make it the easier door.
func (c *conn) authenticate(ctx context.Context, token string) (auth.Actor, auth.SignIn, bool) {
	// An access token only. An API token is not a JWT and fails here, which is the intent: the gateway
	// carries everything an account can see, and a delegated credential reaching it would need a scope
	// check on every event type (M18 plan, finding 12).
	actor, err := c.srv.opts.Accounts.AuthenticateAccessToken(ctx, token)
	if err != nil || actor.Kind != auth.ActorUser {
		c.closeWith(gatewayproto.CloseAuthenticationFailed, "invalid token")
		return auth.Actor{}, auth.SignIn{}, false
	}

	// Counted per account and fail-open like the HTTP limiter: a store that cannot answer must not become
	// every daemon on the instance failing to connect.
	if res, err := c.srv.identifyLimiter.Allow(ctx, "account:"+actor.UserID.String()); err == nil && !res.Allowed {
		c.closeWith(gatewayproto.CloseRateLimited, "identifying too often")
		return auth.Actor{}, auth.SignIn{}, false
	}

	in, err := c.srv.opts.Accounts.SignInOf(ctx, actor.UserID, actor.SessionID)
	if err != nil {
		c.log.Error().Err(err).Msg("gateway could not look up the session")
		c.closeWith(gatewayproto.CloseUnknownError, "internal error")
		return auth.Actor{}, auth.SignIn{}, false
	}
	if in.Device == "" {
		// A token naming a session this account does not hold: not one this instance minted, so the same
		// answer as a device that has signed out.
		c.closeWith(gatewayproto.CloseAuthenticationFailed, auth.ErrSessionSignedOut.Error())
		return auth.Actor{}, auth.SignIn{}, false
	}
	return actor, in, true
}

// checkLive asks whether the sign-in is still live, closing the connection when it is not. A token outlives
// its session by up to fifteen minutes, which a request can afford and a connection cannot.
func (c *conn) checkLive(ctx context.Context, userID snowflake.ID, in auth.SignIn) bool {
	err := c.srv.opts.Accounts.RequireLiveSignIn(ctx, userID, in)
	if err == nil {
		return true
	}
	if errors.Is(err, auth.ErrSessionSignedOut) {
		c.closeWith(gatewayproto.CloseAuthenticationFailed, auth.ErrSessionSignedOut.Error())
		return false
	}
	c.log.Error().Err(err).Msg("gateway could not check the session")
	c.closeWith(gatewayproto.CloseUnknownError, "internal error")
	return false
}

// identify authenticates the connection, starts a session, and sends READY, or closes it saying why not.
//
// The order is cheapest refusal first, and nothing about an account is consulted before its token
// verifies. The version is checked before the token because HELLO has already told anybody who connects
// what the server's version is, so refusing on it discloses nothing and spares a signature check.
func (c *conn) identify(ctx context.Context, raw json.RawMessage) bool {
	var id gatewayproto.Identify
	if err := strictDecode(raw, &id); err != nil || id.Token == "" {
		c.closeWith(gatewayproto.CloseDecodeError, "IDENTIFY needs a token")
		return false
	}
	if id.Intents != nil && *id.Intents != 0 {
		c.closeWith(gatewayproto.CloseInvalidIntents, "intents are reserved and must be 0")
		return false
	}

	compat := gatewayproto.Check(c.srv.opts.Version, id.Properties.Version)
	if !compat.Compatible {
		c.closeWith(gatewayproto.CloseVersionMismatch, compat.Reason)
		return false
	}
	if !compat.Checked {
		c.log.Warn().Str("server_version", c.srv.opts.Version).Str("client_version", id.Properties.Version).
			Msg("gateway version check skipped: a development build is on one side")
	}

	actor, in, ok := c.authenticate(ctx, id.Token)
	if !ok {
		return false
	}

	sessionID, err := newSessionID()
	if err != nil {
		c.log.Error().Err(err).Msg("gateway could not mint a session id")
		c.closeWith(gatewayproto.CloseUnknownError, "internal error")
		return false
	}
	s, ok := c.srv.newSession(sessionID, actor.UserID, actor.SessionID, in)
	if !ok {
		c.closeWith(gatewayproto.CloseRateLimited, "too many connections for this account")
		return false
	}
	s.attach(c)
	c.mu.Lock()
	c.sess = s
	c.mu.Unlock()

	// Liveness is asked only now that the session is registered, which is what leaves a sign-out no gap to
	// fall into. One committed before this read is refused by it; one committed after it publishes its
	// revocation after commit, finds this session registered, and closes it. Asked before registering, a
	// sign-out landing between the two would be seen by neither.
	if !c.checkLive(ctx, actor.UserID, in) {
		c.srv.dropSession(s)
		return false
	}
	c.checkedAt = time.Now()

	// The session is a fan-out candidate from here, before READY's data is read, and holds what arrives
	// meanwhile (session.pending). Read first and register second, and an event committed in between
	// would be missed with nothing to say so.
	user, err := c.srv.opts.Accounts.ReadyUser(ctx, actor.UserID)
	if err != nil {
		c.log.Error().Err(err).Msg("gateway could not load the account for READY")
		c.srv.dropSession(s)
		c.closeWith(gatewayproto.CloseUnknownError, "internal error")
		return false
	}
	memberOf, err := c.srv.opts.Guilds.ListForMember(ctx, actor.UserID)
	if err != nil {
		c.log.Error().Err(err).Msg("gateway could not list guilds for READY")
		c.srv.dropSession(s)
		c.closeWith(gatewayproto.CloseUnknownError, "internal error")
		return false
	}

	set := make(map[snowflake.ID]struct{}, len(memberOf))
	for _, g := range memberOf {
		set[g.ID] = struct{}{}
	}
	s.becomeReady(ready{SessionID: sessionID, User: user, Guilds: memberOf}, set)
	c.markIdentified()
	c.watchdog.Reset(c.heartbeatDeadline())
	return true
}

// resume continues a session after a disconnect: the frames the client missed, in order, then RESUMED.
//
// The token is checked exactly as IDENTIFY checks it, and must belong to the account and device the session
// was started by. A session id is half of what resumes a stream and never the whole: with the id alone,
// anybody who read one from a log line would resume somebody else's (docs/architecture.md §2). Every failure
// that is not the token's is answered "not resumable", which a client handles by identifying afresh.
func (c *conn) resume(ctx context.Context, raw json.RawMessage) bool {
	var r gatewayproto.Resume
	if err := strictDecode(raw, &r); err != nil || r.Token == "" || r.SessionID == "" || r.Seq < 0 {
		c.closeWith(gatewayproto.CloseDecodeError, "RESUME needs a token, a session id and a sequence number")
		return false
	}

	actor, in, ok := c.authenticate(ctx, r.Token)
	if !ok || !c.checkLive(ctx, actor.UserID, in) {
		return false
	}

	// Somebody else's session, one on another device, and one that has expired all get the same answer.
	// A different answer for a session that exists but is not yours would confirm session ids.
	s := c.srv.lookupSession(r.SessionID)
	if s == nil || s.userID != actor.UserID || s.deviceID != in.Device {
		c.send(gatewayproto.OpInvalidSess, false)
		return true
	}

	switch s.resume(c, actor.SessionID, in, r.Seq) {
	case resumeEnded:
		// Revoked or expired between the lookup and here: the same answer as a session that was never found.
		c.send(gatewayproto.OpInvalidSess, false)
		return true
	case resumeInvalidSeq:
		c.closeWith(gatewayproto.CloseInvalidSeq, "that sequence number was never sent")
		return false
	case resumeGap:
		// Too far behind to replay. The session is of no further use to anybody, so it goes now rather
		// than holding its buffer until it expires.
		c.srv.dropSession(s)
		c.send(gatewayproto.OpInvalidSess, false)
		return true
	}

	c.mu.Lock()
	c.sess = s
	c.mu.Unlock()
	c.checkedAt = time.Now()
	c.markIdentified()
	c.watchdog.Reset(c.heartbeatDeadline())
	return true
}

// stillSignedIn is the periodic half of revocation: once LivenessInterval has passed since the sign-in last
// answered, a heartbeat asks again. A revocation's close normally arrives over the bus within moments, but
// the bus is at-most-once, and without this a lost one would leave the connection open for as long as its
// client kept it. This bounds that at the interval.
//
// A database that cannot answer keeps the connection and asks again on the next heartbeat. Closing instead
// would turn a database blip into every connection on the process reconnecting at once, into the same
// blip. So the read is bounded well inside the heartbeat deadline: this runs on the read loop ahead of the
// watchdog's reset, and a read left to hang would let the watchdog close the connection anyway.
func (c *conn) stillSignedIn(ctx context.Context) bool {
	if time.Since(c.checkedAt) < c.srv.opts.LivenessInterval {
		return true
	}
	s := c.session()
	ctx, cancel := context.WithTimeout(ctx, c.srv.opts.HeartbeatInterval/4)
	defer cancel()
	err := c.srv.opts.Accounts.RequireLiveSignIn(ctx, s.userID, s.signInNow())
	switch {
	case err == nil:
		c.checkedAt = time.Now()
		return true
	case errors.Is(err, auth.ErrSessionSignedOut):
		// revoke drops the session and closes this connection with CloseSessionRevoked.
		c.srv.revoke(s)
		return false
	default:
		c.log.Warn().Err(err).Msg("gateway could not re-check a connection's sign-in; keeping it until the next heartbeat")
		return true
	}
}

// ready is READY's payload.
//
// user is exactly GET /users/@me's body and each guild exactly GET /guilds/{id}'s, so an object has one wire
// shape wherever it arrives. dm_channels and presences are absent until M57 and M38 fill them: adding a
// field later is additive, and one shipped empty would claim an account has no DMs.
type ready struct {
	SessionID string         `json:"session_id"`
	User      any            `json:"user"`
	Guilds    []guilds.Guild `json:"guilds"`
}

// heartbeatDeadline is how long the connection may go without a heartbeat: one interval plus half again,
// so a client heartbeating on time is never closed by a little latency.
func (c *conn) heartbeatDeadline() time.Duration {
	return c.srv.opts.HeartbeatInterval * 3 / 2
}

// send queues a control frame: anything but a dispatch, which the session numbers.
func (c *conn) send(op gatewayproto.Opcode, payload any) {
	d, err := json.Marshal(payload)
	if err != nil {
		c.log.Error().Err(err).Int("op", int(op)).Msg("gateway could not encode a frame")
		go c.closeWith(gatewayproto.CloseUnknownError, "internal error")
		return
	}
	frame, err := json.Marshal(gatewayproto.Frame{Op: op, D: d})
	if err != nil {
		c.log.Error().Err(err).Msg("gateway could not encode a frame envelope")
		go c.closeWith(gatewayproto.CloseUnknownError, "internal error")
		return
	}
	c.enqueueFrame(frame)
}

// enqueueFrame queues an encoded frame without blocking.
func (c *conn) enqueueFrame(frame []byte) {
	c.unwritten.Add(1)
	select {
	case c.out <- frame:
	default:
		c.unwritten.Add(-1)
		// §15.4: a client that cannot keep up is dropped, never allowed to hold the server's memory or
		// delay anybody else's events. Its session keeps buffering, so it can resume.
		go c.closeWith(gatewayproto.CloseTooSlow, "not reading fast enough")
	}
}

func (c *conn) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-c.out:
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.ws.Write(wctx, websocket.MessageText, frame)
			cancel()
			c.unwritten.Add(-1)
			if err != nil {
				return
			}
		}
	}
}

// reconnect is what shutdown sends: op 7, then a close saying the server is restarting.
func (c *conn) reconnect() {
	c.send(gatewayproto.OpReconnect, nil)
	c.closeWith(int(websocket.StatusServiceRestart), "server restarting")
}

// closeWith closes the connection once, with a gateway close code.
//
// It gives queued frames a moment to reach the socket first, so a refusal is never overtaken by the close
// that follows it, then closes. Safe from any goroutine; later calls do nothing.
func (c *conn) closeWith(code int, reason string) {
	c.closeOnce.Do(func() {
		deadline := time.Now().Add(closeFlushWait)
		for c.unwritten.Load() > 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		c.log.Debug().Int("code", code).Str("reason", reason).Msg("gateway closing connection")
		_ = c.ws.Close(websocket.StatusCode(code), reason)
	})
}

// strictDecode refuses unknown fields and trailing data, as httpx.DecodeJSON does for REST: a field this
// build does not know is a client written for a different protocol, and ignoring it would hide that.
func strictDecode(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after the frame")
	}
	return nil
}

// newSessionID mints the opaque id RESUME names. Random rather than a snowflake: a snowflake carries its
// creation time and is guessable from its neighbors, and this id is half of what resumes a stream.
func newSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
