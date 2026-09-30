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
	// Bus carries events and revocations to this server's connections (parts 5 and 6).
	Bus events.Bus
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
}

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

	// maxConnectionsPerAccount bounds how many connections one account may hold on this process. A daemon
	// holds one; several devices hold several. Sixteen is room for an unusual person and not for a script.
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

	mu      sync.Mutex
	conns   map[*conn]struct{}
	perUser map[snowflake.ID]int
	closing bool
	active  sync.WaitGroup

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

	return &Server{
		opts:            opts,
		identifyLimiter: identifyLimiter,
		frameLimiter:    frameLimiter,
		conns:           map[*conn]struct{}{},
		perUser:         map[snowflake.ID]int{},
	}, nil
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
	if id := c.identity(); id != 0 {
		if s.perUser[id]--; s.perUser[id] <= 0 {
			delete(s.perUser, id)
		}
	}
	s.mu.Unlock()
	s.active.Done()
}

// claim reserves one of userID's connection slots on this process.
func (s *Server) claim(userID snowflake.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perUser[userID] >= maxConnectionsPerAccount {
		return false
	}
	s.perUser[userID]++
	return true
}

func (s *Server) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}
