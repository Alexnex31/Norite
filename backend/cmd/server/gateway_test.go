// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rs/zerolog"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/gateway"
	"github.com/Alexnex31/Norite/backend/internal/guildauth"
)

// gatewaySchemaID is the schema's $id, which is how the compiler addresses its definitions.
const gatewaySchemaID = "https://norite.example/contracts/gateway-events.schema.json"

var (
	serverFrameSchemaOnce sync.Once
	serverFrameSchema     *jsonschema.Schema
	serverFrameSchemaErr  error
)

// serverFrames compiles the contract's ServerFrame definition once.
//
// Formats are asserted, not merely annotated, which is not the 2020-12 default: a date-time that does not
// parse is exactly the drift this exists to catch, and the default would let it through.
func serverFrames(t *testing.T) *jsonschema.Schema {
	t.Helper()
	serverFrameSchemaOnce.Do(func() {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "gateway-events.schema.json"))
		if err != nil {
			serverFrameSchemaErr = err
			return
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			serverFrameSchemaErr = err
			return
		}
		c := jsonschema.NewCompiler()
		c.AssertFormat()
		if err := c.AddResource(gatewaySchemaID, doc); err != nil {
			serverFrameSchemaErr = err
			return
		}
		serverFrameSchema, serverFrameSchemaErr = c.Compile(gatewaySchemaID + "#/$defs/ServerFrame")
	})
	require.NoError(t, serverFrameSchemaErr)
	return serverFrameSchema
}

// gatewayClient is a raw WebSocket client, the kind the done-when names: nothing of the daemon, only the
// protocol. Every frame it reads is validated against the contract before a test sees it, so every test in
// this file is also a test that the server sends only what gateway-events.schema.json describes (rule 6).
type gatewayClient struct {
	t  *testing.T
	ws *websocket.Conn
}

func dialGateway(t *testing.T, url string, header http.Header) *gatewayClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, res, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(url, "http")+gatewayPath,
		&websocket.DialOptions{HTTPHeader: header})
	if res != nil && res.Body != nil {
		_ = res.Body.Close()
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = ws.CloseNow() })
	return &gatewayClient{t: t, ws: ws}
}

// read returns the next frame, failing the test if it does not match the contract.
func (g *gatewayClient) read() gatewayproto.Frame {
	g.t.Helper()
	f, err := g.tryRead()
	require.NoError(g.t, err)
	return f
}

func (g *gatewayClient) tryRead() (gatewayproto.Frame, error) {
	g.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, raw, err := g.ws.Read(ctx)
	if err != nil {
		return gatewayproto.Frame{}, err
	}
	require.Equal(g.t, websocket.MessageText, typ)

	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	require.NoError(g.t, err)
	require.NoError(g.t, serverFrames(g.t).Validate(inst),
		"the server sent a frame the contract does not describe:\n%s", raw)

	var f gatewayproto.Frame
	require.NoError(g.t, json.Unmarshal(raw, &f))
	return f, nil
}

func (g *gatewayClient) send(op gatewayproto.Opcode, d any) {
	g.t.Helper()
	body, err := json.Marshal(map[string]any{"op": op, "d": d, "s": nil, "t": nil})
	require.NoError(g.t, err)
	g.sendRaw(body)
}

func (g *gatewayClient) sendRaw(body []byte) {
	g.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(g.t, g.ws.Write(ctx, websocket.MessageText, body))
}

func (g *gatewayClient) identify(token, version string) {
	g.t.Helper()
	g.send(gatewayproto.OpIdentify, map[string]any{
		"token":      token,
		"properties": map[string]any{"os": "linux", "client": "test", "version": version},
	})
}

// expectClose reads until the server closes, and asserts the code. Frames before the close are validated
// like any other.
func (g *gatewayClient) expectClose(code int) string {
	g.t.Helper()
	for {
		_, err := g.tryRead()
		if err == nil {
			continue
		}
		var ce websocket.CloseError
		require.True(g.t, errors.As(err, &ce), "expected a close frame, got %v", err)
		assert.Equal(g.t, websocket.StatusCode(code), ce.Code, "reason: %q", ce.Reason)
		return ce.Reason
	}
}

// hello reads and returns HELLO, which is always the first frame.
func (g *gatewayClient) hello() gatewayproto.Hello {
	g.t.Helper()
	f := g.read()
	require.Equal(g.t, gatewayproto.OpHello, f.Op)
	var h gatewayproto.Hello
	require.NoError(g.t, json.Unmarshal(f.D, &h))
	return h
}

// readyPayload is READY's d, decoded loosely: the schema has already checked its shape.
type readyPayload struct {
	SessionID string `json:"session_id"`
	User      struct {
		ID string `json:"id"`
	} `json:"user"`
	Guilds []struct {
		ID string `json:"id"`
	} `json:"guilds"`
}

func (g *gatewayClient) ready() readyPayload {
	g.t.Helper()
	f := g.read()
	require.Equal(g.t, gatewayproto.OpDispatch, f.Op)
	require.NotNil(g.t, f.T)
	require.Equal(g.t, "READY", *f.T)
	require.NotNil(g.t, f.S)
	require.Equal(g.t, int64(1), *f.S, "READY is the session's first dispatch")
	var r readyPayload
	require.NoError(g.t, json.Unmarshal(f.D, &r))
	return r
}

// serveGateway puts the fixture's router behind a real listener: a WebSocket needs a socket, which the
// recorder the REST tests use does not have.
func serveGateway(t *testing.T, handler http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

// customGateway builds a gateway over the fixture's services with options of the test's own, and serves it
// directly. It skips the router's middleware, which the tests that need it do not exercise.
func customGateway(t *testing.T, f *guildFixture, mutate func(*gateway.Options)) (string, *gateway.Server) {
	t.Helper()
	opts := gateway.Options{
		Accounts:    f.api.authSvc,
		Guilds:      f.api.guildsSvc,
		Bus:         f.api.bus,
		Audience:    guildauth.NewAudience(db.New(f.api.pool)),
		Version:     gatewayproto.DevVersion,
		Logger:      zerolog.New(os.Stderr).Level(zerolog.Disabled),
		FanoutLanes: 4,
	}
	mutate(&opts)
	gw, err := gateway.New(opts)
	require.NoError(t, err)
	return serveGateway(t, gw), gw
}

// The done-when's first clause, over the real router: a raw client completes the handshake.
func TestARawClientCompletesTheHandshake(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)

	before := time.Now().Add(-time.Second)
	c := dialGateway(t, url, nil)
	h := c.hello()
	assert.Equal(t, gateway.DefaultHeartbeatInterval.Milliseconds(), h.HeartbeatInterval)
	assert.WithinRange(t, h.ServerTime, before, time.Now().Add(time.Second),
		"HELLO carries the server's clock, for the client's offset (ADR 0010)")
	assert.Equal(t, gatewayproto.DevVersion, h.Version)

	c.identify(f.ownerToken, "dev")
	r := c.ready()
	assert.NotEmpty(t, r.SessionID)
	assert.Equal(t, f.ownerID, r.User.ID)
	require.Len(t, r.Guilds, 1)
	assert.Equal(t, f.guildID, r.Guilds[0].ID)

	c.send(gatewayproto.OpHeartbeat, 1)
	ack := c.read()
	assert.Equal(t, gatewayproto.OpHeartbeatAck, ack.Op)
}

// READY carries what the account is a member of and nothing else: the stranger's lists no guild of the
// owner's, and the member's does.
func TestReadyListsOnlyTheAccountsOwnGuilds(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)

	for _, tc := range []struct {
		token string
		want  []string
	}{
		{f.memberToken, []string{f.guildID}},
		{f.strangerToken, nil},
	} {
		c := dialGateway(t, url, nil)
		c.hello()
		c.identify(tc.token, "dev")
		var got []string
		for _, g := range c.ready().Guilds {
			got = append(got, g.ID)
		}
		assert.Equal(t, tc.want, got)
	}
}

// Every refusal is a close code a client can act on without reading the reason text.
func TestEveryRefusalHasItsCloseCode(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)

	minted := f.api.call(http.MethodPost, "/api/v1/auth/tokens",
		map[string]any{"name": "bot", "scopes": []string{"guilds.read"}}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, minted.Code, minted)
	apiToken := minted.field(t, "value")

	cases := []struct {
		name string
		run  func(c *gatewayClient)
		code int
	}{
		{"not JSON", func(c *gatewayClient) { c.sendRaw([]byte("hello")) }, gatewayproto.CloseDecodeError},
		{"an unknown field", func(c *gatewayClient) {
			c.sendRaw([]byte(`{"op":1,"d":null,"s":null,"t":null,"extra":true}`))
		}, gatewayproto.CloseDecodeError},
		{"a reserved op before IDENTIFY", func(c *gatewayClient) {
			c.send(gatewayproto.OpPresenceUpdate, map[string]any{})
		}, gatewayproto.CloseNotAuthenticated},
		{"an invalid token", func(c *gatewayClient) { c.identify("not-a-token", "dev") },
			gatewayproto.CloseAuthenticationFailed},
		{"an API token", func(c *gatewayClient) { c.identify(apiToken, "dev") },
			gatewayproto.CloseAuthenticationFailed},
		{"intents", func(c *gatewayClient) {
			c.send(gatewayproto.OpIdentify, map[string]any{
				"token": f.ownerToken, "intents": 1,
				"properties": map[string]any{"os": "linux", "client": "test", "version": "dev"},
			})
		}, gatewayproto.CloseInvalidIntents},
		{"an unknown field in IDENTIFY", func(c *gatewayClient) {
			c.send(gatewayproto.OpIdentify, map[string]any{
				"token": f.ownerToken, "shard": []int{0, 1},
				"properties": map[string]any{"os": "linux", "client": "test", "version": "dev"},
			})
		}, gatewayproto.CloseDecodeError},
		{"IDENTIFY twice", func(c *gatewayClient) {
			c.identify(f.ownerToken, "dev")
			c.ready()
			c.identify(f.ownerToken, "dev")
		}, gatewayproto.CloseAlreadyAuthenticated},
		{"a reserved op after IDENTIFY", func(c *gatewayClient) {
			c.identify(f.ownerToken, "dev")
			c.ready()
			c.send(gatewayproto.OpVoiceStateUpdate, map[string]any{})
		}, gatewayproto.CloseUnknownOpcode},
		{"an op nobody defined", func(c *gatewayClient) {
			c.identify(f.ownerToken, "dev")
			c.ready()
			c.send(gatewayproto.Opcode(42), nil)
		}, gatewayproto.CloseUnknownOpcode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := dialGateway(t, url, nil)
			c.t = t
			c.hello()
			tc.run(c)
			c.expectClose(tc.code)
		})
	}
}

// A RESUME naming a session that does not exist is answered "not resumable", which a client handles by
// identifying on the same connection.
func TestAResumeOfNoSessionIsAnsweredNotResumable(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	c := dialGateway(t, serveGateway(t, f.api.handler), nil)
	c.hello()

	c.send(gatewayproto.OpResume, map[string]any{"token": f.ownerToken, "session_id": "x", "seq": 3})
	got := c.read()
	assert.Equal(t, gatewayproto.OpInvalidSess, got.Op)
	assert.JSONEq(t, "false", string(got.D))

	c.identify(f.ownerToken, "dev")
	c.ready()
}

// Finding 1 of the plan: a token outlives its session, and a connection outlives a request. Signed out from
// a second device, the laptop's still-unexpired token must not open a stream.
func TestASignedOutSessionCannotIdentify(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)

	// logout/all spares the device that calls it (M11), so the owner's first session is ended from a
	// second one: somebody signing a lost laptop out from their phone.
	phone := f.api.login("owner@example.com", "owner-phone")
	res := f.api.call(http.MethodPost, "/api/v1/auth/logout/all", nil, withToken(phone.AccessToken))
	require.Equal(t, http.StatusOK, res.Code, res)

	laptop := dialGateway(t, url, nil)
	laptop.hello()
	laptop.identify(f.ownerToken, "dev")
	assert.Contains(t, laptop.expectClose(gatewayproto.CloseAuthenticationFailed), "signed out")

	stillSignedIn := dialGateway(t, url, nil)
	stillSignedIn.hello()
	stillSignedIn.identify(phone.AccessToken, "dev")
	stillSignedIn.ready()
}

// ADR 0033's rule, applied at IDENTIFY, with the reason naming the side to upgrade.
func TestAnIncompatibleClientIsRefusedNamingTheSideToUpgrade(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url, _ := customGateway(t, f, func(o *gateway.Options) { o.Version = "0.2.1" })

	old := dialGateway(t, url, nil)
	assert.Equal(t, "0.2.1", old.hello().Version)
	old.identify(f.ownerToken, "0.1.4")
	assert.Contains(t, old.expectClose(gatewayproto.CloseVersionMismatch), "upgrade the client")

	patchBehind := dialGateway(t, url, nil)
	patchBehind.hello()
	patchBehind.identify(f.ownerToken, "0.2.0")
	patchBehind.ready()

	dev := dialGateway(t, url, nil)
	dev.hello()
	dev.identify(f.ownerToken, "dev")
	dev.ready()
}

// Silence is not a connection: no IDENTIFY in time, or no heartbeat once identified, closes it. And
// heartbeating without identifying does not hold a connection open unauthenticated.
func TestADeadlineClosesASilentConnection(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url, _ := customGateway(t, f, func(o *gateway.Options) {
		o.HeartbeatInterval = 100 * time.Millisecond
		o.IdentifyTimeout = 300 * time.Millisecond
	})

	t.Run("no IDENTIFY", func(t *testing.T) {
		c := dialGateway(t, url, nil)
		c.hello()
		assert.Equal(t, "no IDENTIFY in time", c.expectClose(gatewayproto.CloseSessionTimedOut))
	})

	t.Run("heartbeats without IDENTIFY", func(t *testing.T) {
		c := dialGateway(t, url, nil)
		c.hello()
		start := time.Now()
		go func() {
			for range 20 {
				time.Sleep(50 * time.Millisecond)
				if c.ws.Write(context.Background(), websocket.MessageText,
					[]byte(`{"op":1,"d":null,"s":null,"t":null}`)) != nil {
					return
				}
			}
		}()
		assert.Equal(t, "no IDENTIFY in time", c.expectClose(gatewayproto.CloseSessionTimedOut))
		assert.Less(t, time.Since(start), 700*time.Millisecond, "heartbeats must not extend the identify deadline")
	})

	t.Run("no heartbeat after IDENTIFY", func(t *testing.T) {
		c := dialGateway(t, url, nil)
		c.hello()
		c.identify(f.ownerToken, "dev")
		c.ready()
		// The reason is what a client logs, and it said "no IDENTIFY in time" to a connection that had
		// identified, until the M19 manual pass read it in the server's log during a dropped network.
		assert.Equal(t, "no heartbeat in time", c.expectClose(gatewayproto.CloseSessionTimedOut))
	})
}

// §15.4 and the plan's limits: a client flooding frames, or an account opening too many connections, is
// refused with the rate-limit code.
func TestFloodsAreRateLimited(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)

	t.Run("frames", func(t *testing.T) {
		c := dialGateway(t, url, nil)
		c.hello()
		c.identify(f.ownerToken, "dev")
		c.ready()
		go func() {
			for range 200 {
				if c.ws.Write(context.Background(), websocket.MessageText,
					[]byte(`{"op":1,"d":null,"s":null,"t":null}`)) != nil {
					return
				}
			}
		}()
		c.expectClose(gatewayproto.CloseRateLimited)
	})

	t.Run("connections per account", func(t *testing.T) {
		for range 16 {
			c := dialGateway(t, url, nil)
			c.hello()
			c.identify(f.memberToken, "dev")
			c.ready()
		}
		one := dialGateway(t, url, nil)
		one.hello()
		one.identify(f.memberToken, "dev")
		assert.Contains(t, one.expectClose(gatewayproto.CloseRateLimited), "too many connections")
	})
}

// http.Server.Shutdown ignores hijacked connections. The gateway's own shutdown tells each client to
// reconnect and closes it with 1012, and a connection arriving after that is told the same.
func TestShutdownTellsEveryClientToReconnect(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url, gw := customGateway(t, f, func(*gateway.Options) {})

	c := dialGateway(t, url, nil)
	c.hello()
	c.identify(f.ownerToken, "dev")
	c.ready()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- gw.Shutdown(ctx) }()

	assert.Equal(t, gatewayproto.OpReconnect, c.read().Op)
	c.expectClose(int(websocket.StatusServiceRestart))
	require.NoError(t, <-shutdownErr, "shutdown returns once every connection has closed")

	late := dialGateway(t, url, nil)
	late.expectClose(int(websocket.StatusServiceRestart))
}

// The origin check stays at its default. Once M108 puts a cookie in front of this, it is what stops
// cross-site WebSocket hijacking.
func TestACrossOriginUpgradeIsRefused(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, res, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(url, "http")+gatewayPath,
		&websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{"https://evil.example"}}})
	require.Error(t, err)
	require.NotNil(t, res)
	_ = res.Body.Close()
	assert.Equal(t, http.StatusForbidden, res.StatusCode)
}

// The gateway's objects are REST's objects: READY's user and guilds, and every dispatch carrying a channel,
// a role, a member or a message. A JSON Schema cannot reference a YAML document, so the gateway schema
// carries copies generated from contracts/openapi.yaml's components. This holds each copy deep-equal to its
// original, with references rewritten and only the top-level description allowed to differ, so a field
// added to REST and not to the gateway, or the reverse, fails here rather than in a client.
func TestTheGatewaySchemaMirrorsTheRESTShapes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "gateway-events.schema.json"))
	require.NoError(t, err)
	var gw struct {
		Defs map[string]any `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(raw, &gw))

	var doc struct {
		Components struct {
			Schemas map[string]any `yaml:"schemas"`
		} `yaml:"components"`
	}
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "openapi.yaml"))
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(body, &doc))

	for _, name := range []string{
		"User", "Guild", "Channel", "PermissionOverwrite", "Role", "Permissions", "Member", "Message",
		"AppliedMessageTag", "Snowflake",
	} {
		rest, ok := doc.Components.Schemas[name]
		require.True(t, ok, "openapi.yaml has no %s", name)
		mirror, ok := gw.Defs[name]
		require.True(t, ok, "gateway-events.schema.json has no %s", name)

		want := withoutTopDescription(rewriteRefs(normalizeJSON(t, rest)))
		got := withoutTopDescription(normalizeJSON(t, mirror))
		assert.Equal(t, want, got, "%s: the gateway's copy has drifted from openapi.yaml's", name)
	}
}

// normalizeJSON round-trips a decoded YAML or JSON value through JSON, so both sides compare as the same Go
// types (YAML decodes integers as int, JSON as float64).
func normalizeJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	var out any
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

func rewriteRefs(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if k == "$ref" {
				if ref, ok := val.(string); ok {
					out[k] = "#/$defs/" + ref[strings.LastIndex(ref, "/")+1:]
					continue
				}
			}
			out[k] = rewriteRefs(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = rewriteRefs(val)
		}
		return out
	}
	return v
}

func withoutTopDescription(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(m))
	for k, val := range m {
		if k != "description" {
			out[k] = val
		}
	}
	return out
}

// The validator every test above leans on has to be able to fail, or it proves nothing.
func TestTheFrameValidatorRejectsWhatTheContractDoesNot(t *testing.T) {
	for _, bad := range []string{
		`{"op":10,"d":{"heartbeat_interval":1,"server_time":"not a time","version":"dev"},"s":null,"t":null}`,
		`{"op":11,"d":null,"s":null}`,
		`{"op":0,"d":{"session_id":"x","user":{},"guilds":[]},"s":1,"t":"READY"}`,
		`{"op":0,"d":{},"s":1,"t":"MESSAGE_CREATE"}`,
		`{"op":42,"d":null,"s":null,"t":null}`,
	} {
		inst, err := jsonschema.UnmarshalJSON(strings.NewReader(bad))
		require.NoError(t, err)
		assert.Error(t, serverFrames(t).Validate(inst), bad)
	}
}

// dialStatus attempts an upgrade and reports the HTTP status of a refused one, or 101.
func dialStatus(t *testing.T, url string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, res, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(url, "http")+gatewayPath, nil)
	if res != nil && res.Body != nil {
		_ = res.Body.Close()
	}
	if err == nil {
		t.Cleanup(func() { _ = ws.CloseNow() })
		return http.StatusSwitchingProtocols
	}
	require.NotNil(t, res, "dial failed without a response: %v", err)
	return res.StatusCode
}

// A rate bounds how fast sockets open, not how many stay open, so the sockets one address holds before
// identifying are capped. Identifying gives the slot back, and so does closing.
func TestAnAddressHoldsBoundedlyManyUnidentifiedConnections(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url, _ := customGateway(t, f, func(o *gateway.Options) { o.MaxUnidentifiedPerAddress = 2 })

	first := dialGateway(t, url, nil)
	first.hello()
	second := dialGateway(t, url, nil)
	second.hello()
	assert.Equal(t, http.StatusTooManyRequests, dialStatus(t, url), "a third unidentified socket is refused")

	first.identify(f.ownerToken, "dev")
	first.ready()
	assert.Equal(t, http.StatusSwitchingProtocols, dialStatus(t, url), "identifying gave its slot back")

	_ = second.ws.CloseNow()
	assert.Eventually(t, func() bool {
		return dialStatus(t, url) == http.StatusSwitchingProtocols
	}, 5*time.Second, 20*time.Millisecond, "closing gave its slot back")
}
