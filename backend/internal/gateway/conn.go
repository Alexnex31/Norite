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
// Three goroutines touch it: serve reads frames and answers them, writeLoop drains out onto the socket,
// and the watchdog timer closes it when a deadline passes. Anything they share is behind mu, and closing is
// once-only, so any of them may end the connection.
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

	mu        sync.Mutex
	seq       int64
	userID    snowflake.ID
	deviceID  string
	sessionID string
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

// identity is the account this connection identified as, or 0 before IDENTIFY.
func (c *conn) identity() snowflake.ID {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.userID
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
	identified := c.identity() != 0

	switch f.Op {
	case gatewayproto.OpHeartbeat:
		// Only an identified connection earns a longer deadline. Before IDENTIFY the identify timeout stands,
		// or heartbeating alone would hold a connection open unauthenticated for as long as a client liked.
		if identified {
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
		// There is nothing to resume yet: sessions do not outlive their connection until RESUME lands.
		// "Not resumable" is the answer a client already handles, by identifying afresh.
		c.send(gatewayproto.OpInvalidSess, false)
		return true
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

// identify authenticates the connection and sends READY, or closes it saying why not.
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

	// An access token only. An API token is not a JWT and fails here, which is the intent: the gateway
	// carries everything an account can see, and a delegated credential reaching it would need a scope
	// check on every event type (M18 plan, finding 12).
	actor, err := c.srv.opts.Accounts.AuthenticateAccessToken(ctx, id.Token)
	if err != nil || actor.Kind != auth.ActorUser {
		c.closeWith(gatewayproto.CloseAuthenticationFailed, "invalid token")
		return false
	}

	// Counted per account and fail-open like the HTTP limiter: a store that cannot answer must not become
	// every daemon on the instance failing to connect.
	if res, err := c.srv.identifyLimiter.Allow(ctx, "account:"+actor.UserID.String()); err == nil && !res.Allowed {
		c.closeWith(gatewayproto.CloseRateLimited, "identifying too often")
		return false
	}

	device, err := c.srv.opts.Accounts.LiveDevice(ctx, actor.UserID, actor.SessionID)
	if err != nil {
		if errors.Is(err, auth.ErrSessionSignedOut) {
			c.closeWith(gatewayproto.CloseAuthenticationFailed, auth.ErrSessionSignedOut.Error())
			return false
		}
		c.log.Error().Err(err).Msg("gateway could not check the session")
		c.closeWith(gatewayproto.CloseUnknownError, "internal error")
		return false
	}

	user, err := c.srv.opts.Accounts.ReadyUser(ctx, actor.UserID)
	if err != nil {
		c.log.Error().Err(err).Msg("gateway could not load the account for READY")
		c.closeWith(gatewayproto.CloseUnknownError, "internal error")
		return false
	}
	memberOf, err := c.srv.opts.Guilds.ListForMember(ctx, actor.UserID)
	if err != nil {
		c.log.Error().Err(err).Msg("gateway could not list guilds for READY")
		c.closeWith(gatewayproto.CloseUnknownError, "internal error")
		return false
	}

	if !c.srv.claim(actor.UserID) {
		c.closeWith(gatewayproto.CloseRateLimited, "too many connections for this account")
		return false
	}
	sessionID, err := newSessionID()
	if err != nil {
		c.log.Error().Err(err).Msg("gateway could not mint a session id")
		c.closeWith(gatewayproto.CloseUnknownError, "internal error")
		return false
	}

	c.mu.Lock()
	c.userID, c.deviceID, c.sessionID = actor.UserID, device, sessionID
	c.mu.Unlock()
	c.watchdog.Reset(c.heartbeatDeadline())

	c.dispatch("READY", ready{SessionID: sessionID, User: user, Guilds: memberOf})
	return true
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

// send queues a control frame.
func (c *conn) send(op gatewayproto.Opcode, payload any) {
	c.enqueue(op, payload, "")
}

// dispatch queues an op 0 frame with the connection's next sequence number.
func (c *conn) dispatch(event string, payload any) {
	c.enqueue(gatewayproto.OpDispatch, payload, event)
}

func (c *conn) enqueue(op gatewayproto.Opcode, payload any, event string) {
	d, err := json.Marshal(payload)
	if err != nil {
		c.log.Error().Err(err).Int("op", int(op)).Msg("gateway could not encode a frame")
		c.closeWith(gatewayproto.CloseUnknownError, "internal error")
		return
	}

	// The sequence number is taken and the frame queued under one lock, so frames reach the socket in the
	// order of their numbers. Taken outside it, two dispatches racing could queue 8 before 7, and a client
	// resuming from 7 would skip 8.
	c.mu.Lock()
	defer c.mu.Unlock()
	f := gatewayproto.Frame{Op: op, D: d}
	if op == gatewayproto.OpDispatch {
		c.seq++
		seq, t := c.seq, event
		f.S, f.T = &seq, &t
	}
	frame, err := json.Marshal(f)
	if err != nil {
		c.log.Error().Err(err).Msg("gateway could not encode a frame envelope")
		go c.closeWith(gatewayproto.CloseUnknownError, "internal error")
		return
	}
	c.unwritten.Add(1)
	select {
	case c.out <- frame:
	default:
		c.unwritten.Add(-1)
		// §15.4: a client that cannot keep up is dropped, never allowed to hold the server's memory or
		// delay anybody else's events. It reconnects and resyncs.
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
