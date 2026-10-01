// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package gatewayclient holds the daemon's connection to the instance's gateway: the one persistent
// WebSocket an account has per machine, which the CLI, TUI and GUI share through the daemon rather than
// each opening their own (ADR 0010).
//
// It speaks backend/gatewayproto, the wire format the server is written against, so the two sides cannot
// disagree about an op-code or a close code. What it does with each frame is the point of this package:
// when to RESUME and when to IDENTIFY afresh, what each close code asks of the client, and how hard to back
// off. Those decisions are the table on Client.after.
//
// It decides nothing about what an event means. Every dispatch, READY included, goes to a Sink in sequence
// order, and the sink is told when a fresh session begins — the moment anything it built from the previous
// one stops being trustworthy, because events were missed in between.
package gatewayclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/daemon/internal/backoff"
	"github.com/Alexnex31/Norite/daemon/internal/session"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// Credentials is what the client needs from the session: a token, and somewhere to report what the gateway
// said about it. *session.Source implements it.
type Credentials interface {
	Current(ctx context.Context) (session.Credential, error)
	Rejected(accessToken string)
	Revoked()
	ObserveServerTime(t time.Time)
	// Ended is closed once the sign-in of that generation is over: the connection streaming it must close.
	Ended(generation uint64) <-chan struct{}
}

// Sink receives what the gateway sends. Called from one goroutine, in sequence order.
type Sink interface {
	// Begin says a fresh session is starting: IDENTIFY, not RESUME. Whatever the sink built from an earlier
	// session — or an earlier sign-in, when generation changed — may have missed events and is stale.
	Begin(generation uint64)
	// Dispatch delivers one event: its name and its payload exactly as the server sent it.
	Dispatch(eventType string, data json.RawMessage)
	// End says the sign-in is over — signed out, revoked, or replaced by another account's. Whatever the
	// sink holds belongs to an account nobody on this machine is signed in as any more, and must go before
	// anything reads it, not at the next IDENTIFY, which a signed-out daemon never sends (M19 /code-review).
	End()
}

// Options configures a Client.
type Options struct {
	Credentials Credentials
	Sink        Sink
	// Version is this daemon's release version, sent in IDENTIFY and checked against HELLO's. Empty means
	// gatewayproto.DevVersion: the server refuses an empty one, and an unstamped build is a dev build.
	Version string
	Log     zerolog.Logger

	// The waits between connections. Zero means the defaults; tests shrink them.
	RetryMin, RetryMax time.Duration
	// RateLimitedFloor is the least wait after 4008, ProtocolFloor after a close that says this client is
	// broken, and VersionHold after a version mismatch.
	RateLimitedFloor, ProtocolFloor, VersionHold time.Duration
	// HelloTimeout bounds the wait for HELLO, and the time to produce a token for IDENTIFY after it.
	HelloTimeout time.Duration
}

const (
	defaultRetryMin = time.Second
	// defaultRetryMax keeps a daemon well inside the server's thirty IDENTIFYs a minute per account, which
	// every device on the account shares.
	defaultRetryMax         = time.Minute
	defaultRateLimitedFloor = 30 * time.Second
	defaultProtocolFloor    = time.Minute
	// defaultVersionHold is how long a version mismatch is believed. A daemon upgrade restarts the daemon, so
	// the case worth retrying for is the server being upgraded under a running one.
	defaultVersionHold  = time.Hour
	defaultHelloTimeout = 10 * time.Second

	// identifyBudget is how long the client may spend producing a token once HELLO has arrived: half the
	// server's ten-second identify deadline (gateway.DefaultIdentifyTimeout), which started before HELLO was
	// sent. Waiting the full hello timeout here could be pre-empted by the server's 4009 and waste the
	// connection (M19 /code-review).
	identifyBudget = 5 * time.Second

	// maxHeartbeatInterval bounds what HELLO may ask for. The server's default is 41.25 seconds; ten minutes
	// is room for any real configuration and keeps the interval far from overflowing a Duration.
	maxHeartbeatInterval = 10 * time.Minute

	// maxInboundFrame bounds one frame from the server. READY is about 20 KB at the joined-guild cap and a
	// message at most a few times its 4,000-rune content; this is room for growth, not for a server trying
	// to make the daemon buffer something enormous. It replaces the stream-decoding the roadmap once asked
	// for, which a READY of summaries does not need (M19 planning).
	maxInboundFrame = 4 << 20

	// stableAfter is how long a connection must have lasted for the backoff to start again from its floor. A
	// server that accepts a session and drops it at once must not be reconnected to at the floor for ever.
	stableAfter = 30 * time.Second
)

// Client is the daemon's gateway connection.
type Client struct {
	creds   Credentials
	sink    Sink
	version string
	log     zerolog.Logger
	http    *http.Client

	retryMin, retryMax                           time.Duration
	rateLimitedFloor, protocolFloor, versionHold time.Duration
	helloTimeout                                 time.Duration

	// Owned by Run: what a RESUME needs, and the sign-in it belongs to.
	sessionID  string
	generation uint64
	seq        int64
}

// New builds a Client. Nothing connects until Run.
func New(opts Options) *Client {
	c := &Client{
		creds:            opts.Credentials,
		sink:             opts.Sink,
		version:          opts.Version,
		log:              opts.Log,
		retryMin:         orDefault(opts.RetryMin, defaultRetryMin),
		retryMax:         orDefault(opts.RetryMax, defaultRetryMax),
		rateLimitedFloor: orDefault(opts.RateLimitedFloor, defaultRateLimitedFloor),
		protocolFloor:    orDefault(opts.ProtocolFloor, defaultProtocolFloor),
		versionHold:      orDefault(opts.VersionHold, defaultVersionHold),
		helloTimeout:     orDefault(opts.HelloTimeout, defaultHelloTimeout),
		// The upgrade carries no credential — the token goes in IDENTIFY, after it — so a redirect leaks
		// nothing. Refused anyway, as every client in this repository refuses them: a gateway that has moved
		// is a configuration to fix, not something to follow silently. And no Timeout: coder/websocket
		// requires none, because it would cut the connection it hijacks; the dial is bounded by its context.
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
	if c.version == "" {
		c.version = gatewayproto.DevVersion
	}
	return c
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Run holds the connection until ctx is done, reconnecting for as long as it takes.
//
// It waits on the session for a credential, so a signed-out daemon is a client blocked here until somebody
// signs in, and never a client reconnecting with nothing to identify with.
func (c *Client) Run(ctx context.Context) {
	retry := &backoff.Backoff{Min: c.retryMin, Max: c.retryMax}
	for {
		cred, err := c.creds.Current(ctx)
		if err != nil {
			return // ctx is done; Current fails for no other reason
		}
		if cred.Generation != c.generation {
			// A different sign-in. A RESUME names a session the previous sign-in started, and the server
			// would refuse it — or, worse, the instance itself may be a different one.
			if c.generation != 0 {
				c.sink.End()
			}
			c.generation, c.sessionID, c.seq = cred.Generation, "", 0
		}

		started := time.Now()
		out := c.connect(ctx, cred)
		if ctx.Err() != nil {
			return
		}
		if out.established && time.Since(started) >= stableAfter {
			retry.Reset()
		}

		wait := c.after(out, retry)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// What ended a connection, as far as what happens next is concerned.
type ending int

const (
	endResume      ending = iota // reconnect and RESUME: a drop, a restart, a timeout, op 7
	endIdentify                  // reconnect and IDENTIFY afresh: the session cannot be resumed
	endRejected                  // 4004: the token was refused
	endRevoked                   // 4011: the sign-in is over
	endRateLimited               // 4008
	endVersion                   // 4010, or HELLO named a version this daemon cannot talk to
	endProtocol                  // a close that says this client sent something it should not have
	endAgain                     // the sign-in changed or ended: start over at once, waiting on the session
)

type outcome struct {
	ending      ending
	token       string // the access token this connection presented, for Rejected
	established bool   // READY or RESUMED arrived
}

// after applies what an ending asks for and returns how long to wait before connecting again. This is the
// close-code table in the M19 plan, and the one place it lives.
func (c *Client) after(out outcome, retry *backoff.Backoff) time.Duration {
	switch out.ending {
	case endIdentify:
		c.sessionID, c.seq = "", 0
		return retry.Next()

	case endRejected:
		// Either the token expired — a laptop that slept past it, a clock the estimate has wrong — or the
		// server's 4004 for a sign-in that is over. Both are the session's to settle: it refreshes, and a
		// refresh the instance refuses is a sign-out. The session id stays; RESUME is still worth trying with
		// the new token inside the server's resume window.
		c.creds.Rejected(out.token)
		return retry.Next()

	case endRevoked:
		// The sign-in is over — revoked, or superseded by a `norite login` on this machine. The session
		// reads the store; Current blocks until there is something to sign in with.
		c.creds.Revoked()
		c.sink.End()
		c.sessionID, c.seq = "", 0
		return c.retryMin

	case endRateLimited:
		return max(retry.Next(), c.rateLimitedFloor)

	case endVersion:
		return c.versionHold

	case endProtocol:
		// Something this client sent was refused as malformed. Reconnecting will send it again, so the
		// floor is long and the session is not resumed: whatever state led here is not worth keeping.
		c.sessionID, c.seq = "", 0
		return max(retry.Next(), c.protocolFloor)

	case endAgain:
		return 0
	}
	return retry.Next()
}

// connect dials, converses until the connection ends, and reports how.
func (c *Client) connect(ctx context.Context, cred session.Credential) outcome {
	target, err := gatewayURL(cred.InstanceURL)
	if err != nil {
		// Validated when it was stored; a record a person edited by hand is the way here.
		c.log.Error().Err(err).Msg("the stored instance URL cannot be turned into a gateway address")
		return outcome{ending: endProtocol}
	}

	dialCtx, cancel := context.WithTimeout(ctx, c.helloTimeout)
	ws, resp, err := websocket.Dial(dialCtx, target, &websocket.DialOptions{
		HTTPClient: c.http,
		// Off on the server too (M18): attacker-influenced content and anything sensitive never share a
		// compression context.
		CompressionMode: websocket.CompressionDisabled,
	})
	cancel()
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		// The error names the URL, which carries no credential — the token goes in IDENTIFY. The status, if
		// the server answered, is the useful part; its body and reason phrase are the server's text.
		ev := c.log.Warn().Str("instance", termsafe.Text(cred.InstanceURL))
		if resp != nil {
			ev = ev.Int("status", resp.StatusCode)
		}
		ev.Msg("could not reach the gateway; trying again")
		return outcome{ending: endResume}
	}
	defer func() { _ = ws.CloseNow() }()
	ws.SetReadLimit(maxInboundFrame)

	return c.converse(ctx, ws, cred)
}

// gatewayURL turns an instance URL into its gateway address: the same host, ws or wss, at /gateway under
// whatever path the instance is served from.
//
// The path is kept, not replaced. credentials.ParseInstanceURL accepts an instance behind a path prefix —
// https://example.com/norite, a self-hoster's reverse proxy — and REST appends to it. Replacing the path
// sent the access token in IDENTIFY to example.com/gateway: the same host, and possibly a different
// application entirely (M19 /security-sweep).
func gatewayURL(instanceURL string) (string, error) {
	u, err := url.Parse(instanceURL)
	if err != nil {
		return "", fmt.Errorf("parsing the instance URL: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("an instance URL must be http or https, not %q", u.Scheme)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/gateway"
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	return u.String(), nil
}

// closeEnding maps a close code from the server to what happens next.
func closeEnding(code websocket.StatusCode) ending {
	switch code {
	case gatewayproto.CloseInvalidSeq:
		return endIdentify
	case gatewayproto.CloseAuthenticationFailed:
		return endRejected
	case gatewayproto.CloseSessionRevoked:
		return endRevoked
	case gatewayproto.CloseRateLimited:
		return endRateLimited
	case gatewayproto.CloseVersionMismatch:
		return endVersion
	case gatewayproto.CloseUnknownOpcode, gatewayproto.CloseDecodeError, gatewayproto.CloseNotAuthenticated,
		gatewayproto.CloseAlreadyAuthenticated, gatewayproto.CloseInvalidIntents:
		return endProtocol
	}
	// 4000, 4009, 4012, the 1012 a restarting server sends after op 7, an abrupt drop, and any code this
	// build has no name for: the session may still be there, so try to resume it.
	return endResume
}

// errHeartbeatMissed is how the heartbeat ends a connection the server has stopped answering.
var errHeartbeatMissed = errors.New("the gateway stopped acknowledging heartbeats")
