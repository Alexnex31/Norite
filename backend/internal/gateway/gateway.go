// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package gateway is the real-time half of the API: the WebSocket at /gateway that a daemon holds open,
// identifies on, and receives the account's events over (docs/architecture.md §2, M18).
//
// It is the codebase's first long-lived connection, and most of what is written here follows from that.
// A REST request is authorized once and is over in milliseconds; a connection is authorized once and then
// lasts as long as the client keeps it. So everything a request can leave to "the next request will be
// checked" has to be checked here instead, or closed from outside when it stops being true.
//
//   - **IDENTIFY asks whether the device is still signed in**, not only whether the token verifies. A token
//     outlives its session by up to fifteen minutes, which a request can afford and a connection cannot.
//   - **Nothing here decides who may see what.** READY carries only what the account is a member of; which
//     events reach a connection is a fresh permission check per event (M18 part 6), never a cached one.
//   - **Every connection belongs to the server that accepted it**, which knows how to close it: on
//     shutdown with a Reconnect, on a revoked session (part 5), on a client too slow to keep up.
//
// The wire format is gatewayproto's, shared with the daemon from M19.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/dispatch"
	"github.com/Alexnex31/Norite/backend/internal/guilds"
	"github.com/Alexnex31/Norite/backend/internal/platform/events"
	"github.com/Alexnex31/Norite/backend/internal/platform/logging"
	"github.com/Alexnex31/Norite/backend/internal/platform/ratelimit"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// Accounts is what the gateway needs from auth. An interface so this package's own tests can drive the
// protocol without a database, while cmd/server wires the real service.
type Accounts interface {
	AuthenticateAccessToken(ctx context.Context, raw string) (auth.Actor, error)
	LiveDevice(ctx context.Context, userID, sessionID snowflake.ID) (string, error)
	ReadyUser(ctx context.Context, userID snowflake.ID) (any, error)
}

// Guilds is what the gateway needs from guilds.
type Guilds interface {
	ListForMember(ctx context.Context, userID snowflake.ID) ([]guilds.Guild, error)
}

// Options configures a Server.
type Options struct {
	Accounts Accounts
	Guilds   Guilds
	// Bus carries events and revocations to this server's connections.
	Bus events.Bus
	// Audience decides which connections may receive each event (guildauth.Audience).
	Audience AudienceResolver
	// RateLimitBackend counts IDENTIFY attempts per account, across replicas when it is Redis.
	RateLimitBackend ratelimit.Backend
	// Version is this server's release version, sent in HELLO and checked against every client's.
	Version string
	Logger  zerolog.Logger

	// HeartbeatInterval is what HELLO asks clients to keep. Zero means DefaultHeartbeatInterval; tests set
	// it small so a missed heartbeat takes milliseconds to observe rather than a minute.
	HeartbeatInterval time.Duration
	// IdentifyTimeout is how long a connection may stay open without identifying. Zero means the
	// heartbeat interval.
	IdentifyTimeout time.Duration
	// Now is the clock HELLO reports. Zero means time.Now.
	Now func() time.Time

	// ResumeWindow is how long a session outlives its connection, buffering events, waiting to be resumed.
	// Zero means DefaultResumeWindow.
	ResumeWindow time.Duration
	// ResumeBuffer is how many frames a session keeps for replay. Zero means DefaultResumeBuffer.
	ResumeBuffer int
}

// DefaultResumeWindow is how long a disconnected session waits to be resumed. Long enough for a laptop
// changing networks or a daemon restarting; short enough that a client which is not coming back does not
// hold a buffer for long.
const DefaultResumeWindow = 2 * time.Minute

// DefaultResumeBuffer is how many frames a session keeps for replay, alongside maxResumeBytes. A client that
// missed more than this identifies afresh and resyncs over REST, which is always correct and only slower.
const DefaultResumeBuffer = 512

// maxResumeBytes bounds one session's replay buffer in bytes, whatever its frame count: a few large payloads
// must not let one detached session hold megabytes.
const maxResumeBytes = 1 << 20

// DefaultHeartbeatInterval is Discord's value, which docs/architecture.md §2 shows in its HELLO example.
const DefaultHeartbeatInterval = 41250 * time.Millisecond

const (
	// maxInboundFrame bounds one client frame. The largest a client sends is IDENTIFY, a few hundred bytes
	// with its token; this leaves room for later ops and none for a client trying to make the server
	// buffer something large.
	maxInboundFrame = 16 << 10

	// outboundBuffer is how many frames may wait for one connection's socket (§15.4). A client that lets
	// it fill is dropped with CloseTooSlow rather than allowed to hold the server's memory or slow the
	// fan-out every other connection shares.
	outboundBuffer = 1024

	// maxConnectionsPerAccount bounds how many sessions one account may hold on this process, attached or
	// waiting to be resumed: a detached session holds a buffer, so it counts. A daemon holds one; several
	// devices hold several. Sixteen is room for an unusual person and not for a script.
	maxConnectionsPerAccount = 16

	// identifyRate bounds IDENTIFY per account. A daemon reconnecting after a network drop identifies
	// again, so this has to allow a few in a row; thirty a minute does, and stops a stolen token being
	// used to churn sessions.
	identifyRate = "30-M"

	// frameRate bounds inbound frames per connection. A heartbeat every forty seconds is the whole of a
	// normal client's traffic until presence and voice arrive; 120 a minute is Discord's figure.
	frameRate = "120-M"

	// writeTimeout bounds one frame's write, so a peer that stops reading is noticed.
	writeTimeout = 10 * time.Second

	// closeFlushWait is how long a close waits for queued frames to reach the socket first, so a refusal's
	// reason is not overtaken by the close it explains.
	closeFlushWait = time.Second
)

// Server accepts and holds gateway connections.
type Server struct {
	opts            Options
	identifyLimiter *ratelimit.Limiter
	frameLimiter    *ratelimit.Limiter

	sub events.Subscription

	mu       sync.Mutex
	conns    map[*conn]struct{}
	sessions map[string]*session
	perUser  map[snowflake.ID]int
	closing  bool
	active   sync.WaitGroup

	nextConnID atomic.Uint64
}

// New builds a Server.
func New(opts Options) (*Server, error) {
	if opts.Accounts == nil || opts.Guilds == nil {
		return nil, errors.New("gateway: Accounts and Guilds are required")
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if opts.IdentifyTimeout <= 0 {
		opts.IdentifyTimeout = opts.HeartbeatInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ResumeWindow <= 0 {
		opts.ResumeWindow = DefaultResumeWindow
	}
	if opts.ResumeBuffer <= 0 {
		opts.ResumeBuffer = DefaultResumeBuffer
	}

	identifyLimiter, err := ratelimit.New(ratelimit.Options{
		Rate: identifyRate, Bucket: "gateway-identify", Backend: opts.RateLimitBackend,
	})
	if err != nil {
		return nil, err
	}
	// Always in memory, whatever the instance's backend: a connection lives in exactly one process, so its
	// frame count has nowhere else to be, and a Redis round trip per frame would be a cost with no reader.
	frameLimiter, err := ratelimit.New(ratelimit.Options{
		Rate: frameRate, Bucket: "gateway-frames", Backend: ratelimit.MemoryBackend(),
	})
	if err != nil {
		return nil, err
	}

	s := &Server{
		opts:            opts,
		identifyLimiter: identifyLimiter,
		frameLimiter:    frameLimiter,
		conns:           map[*conn]struct{}{},
		sessions:        map[string]*session{},
		perUser:         map[snowflake.ID]int{},
	}
	if opts.Bus != nil {
		if s.sub, err = opts.Bus.Subscribe(dispatch.Topic, s.onEvent); err != nil {
			return nil, fmt.Errorf("gateway: subscribing to events: %w", err)
		}
	}
	return s, nil
}

// ServeHTTP upgrades GET /gateway and runs the connection until it ends.
//
// The request has already passed the router's whole chain, including the per-address rate limit and the
// refusal while migrations run, so an upgrade is one ordinary request as far as those are concerned.
//
// No origin exemption. coder/websocket refuses a cross-origin upgrade by default and that stays: the
// daemon sends no Origin header, and once M108 puts a cookie in front of this, the origin check is what
// stops cross-site WebSocket hijacking. Loosening it now "because only the daemon connects" would be a
// hole with a delayed fuse.
//
// Compression stays off. It is simpler, and it keeps attacker-influenced content and anything sensitive
// out of one compression context; turning it on is a decision to make with a measurement.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		// Accept has already answered: a failed handshake is the client's error, reported to it.
		logging.FromContext(r.Context()).Debug().Err(err).Msg("gateway upgrade refused")
		return
	}
	ws.SetReadLimit(maxInboundFrame)

	c := newConn(s, ws, logging.FromContext(r.Context()))
	if !s.track(c) {
		// Shutting down: the same instruction every open connection is about to get, so a client
		// reconnecting mid-rollout goes somewhere that is staying up.
		_ = ws.Close(websocket.StatusServiceRestart, "server restarting")
		return
	}
	defer s.untrack(c)

	// Detached from the request's cancellation but not its values: after a hijack net/http no longer
	// cancels the request context when the client goes away, so the connection runs on its own context and
	// ends when its socket does. The values carry the request id and logger into every line it writes.
	c.serve(context.WithoutCancel(r.Context()))
}

// Shutdown tells every connection to reconnect elsewhere and closes it, then waits for their handlers to
// return or for ctx to end.
//
// http.Server.Shutdown does not do this. It waits for active requests and ignores hijacked connections
// entirely, and a WebSocket is a hijacked connection, so without this every client would simply be cut
// off with nothing telling it to come back (M118's rollout depends on this op-code existing).
func (s *Server) Shutdown(ctx context.Context) error {
	// No new events first: a connection being told to reconnect should not be handed one more frame.
	if s.sub != nil {
		s.sub.Unsubscribe()
	}
	s.mu.Lock()
	s.closing = true
	all := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		all = append(all, c)
	}
	s.mu.Unlock()

	// Concurrently: each close waits briefly for its queued frames, and one after another that wait would
	// add up across every connection on the process.
	for _, c := range all {
		go c.reconnect()
	}

	done := make(chan struct{})
	go func() { s.active.Wait(); close(done) }()
	// Sessions die with the process: there is nothing to resume on a server that is going away, and a
	// client told to reconnect resumes against whichever server answers, or identifies afresh (M114).
	s.mu.Lock()
	var sessions []*session
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()
	for _, sess := range sessions {
		s.dropSession(sess)
	}

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("gateway: %d connection(s) still open at shutdown: %w", s.count(), ctx.Err())
	}
}

func (s *Server) track(c *conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.conns[c] = struct{}{}
	s.active.Add(1)
	return true
}

func (s *Server) untrack(c *conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	s.active.Done()
}

// newSession registers a session for userID, if the account has a slot left on this process. From here it
// is a fan-out candidate.
//
// A fresh IDENTIFY from a device supersedes that device's detached sessions first: a client that identifies
// rather than resuming has given its earlier stream up, and leaving it to expire would let a daemon that
// reconnects by identifying fill its account's slots within minutes and lock itself out. Only detached
// ones: a session with a connection attached is in use, by this device or by one sharing its id.
func (s *Server) newSession(id string, userID snowflake.ID, device string) (*session, bool) {
	s.mu.Lock()
	var superseded []*session
	for _, old := range s.sessions {
		if old.userID != userID || old.deviceID != device {
			continue
		}
		old.mu.Lock()
		detached := old.conn == nil
		old.mu.Unlock()
		if detached {
			delete(s.sessions, old.id)
			s.perUser[userID]--
			superseded = append(superseded, old)
		}
	}
	if s.perUser[userID] >= maxConnectionsPerAccount {
		s.mu.Unlock()
		stopAll(superseded)
		return nil, false
	}
	s.perUser[userID]++
	sess := &session{srv: s, id: id, userID: userID, deviceID: device}
	s.sessions[id] = sess
	s.mu.Unlock()
	stopAll(superseded)
	return sess, true
}

func stopAll(sessions []*session) {
	for _, sess := range sessions {
		sess.stop()
	}
}

func (s *Server) lookupSession(id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// dropSession forgets a session and frees its account's slot. Idempotent, since an expiry and an explicit
// drop can race.
func (s *Server) dropSession(sess *session) {
	s.mu.Lock()
	if s.sessions[sess.id] == sess {
		delete(s.sessions, sess.id)
		if s.perUser[sess.userID]--; s.perUser[sess.userID] <= 0 {
			delete(s.perUser, sess.userID)
		}
	}
	s.mu.Unlock()
	sess.stop()
}

// expireSession is the resume window's timer: the session goes unless it was resumed while the timer was
// firing, which is the race a plain drop would lose.
func (s *Server) expireSession(sess *session) {
	sess.mu.Lock()
	attached := sess.conn != nil
	sess.mu.Unlock()
	if !attached {
		s.dropSession(sess)
	}
}

func (s *Server) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}
