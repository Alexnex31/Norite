// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package gatewayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/rs/zerolog"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/daemon/internal/session"
)

// The client is driven against a fake gateway written with the same wire format, and every frame in either
// direction is validated against contracts/gateway-events.schema.json — the document the real server's
// tests validate its frames against. So the fake cannot drift from the server without failing here, and
// the daemon cannot send what the server would refuse to decode.

// ---------- the contract ----------

const gatewaySchemaID = "https://norite.example/contracts/gateway-events.schema.json"

var (
	schemaOnce              sync.Once
	serverSchema, clientSch *jsonschema.Schema
	schemaErr               error
)

func schemas(t *testing.T) (server, client *jsonschema.Schema) {
	t.Helper()
	schemaOnce.Do(func() {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "gateway-events.schema.json"))
		if err != nil {
			schemaErr = err
			return
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			schemaErr = err
			return
		}
		c := jsonschema.NewCompiler()
		c.AssertFormat()
		if schemaErr = c.AddResource(gatewaySchemaID, doc); schemaErr != nil {
			return
		}
		if serverSchema, schemaErr = c.Compile(gatewaySchemaID + "#/$defs/ServerFrame"); schemaErr != nil {
			return
		}
		clientSch, schemaErr = c.Compile(gatewaySchemaID + "#/$defs/ClientFrame")
	})
	require.NoError(t, schemaErr)
	return serverSchema, clientSch
}

func validate(t *testing.T, s *jsonschema.Schema, data []byte, what string) {
	t.Helper()
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	require.NoError(t, err)
	require.NoErrorf(t, s.Validate(inst), "%s does not match the contract: %s", what, data)
}

// ---------- a fake gateway ----------

type fakeGateway struct {
	t       *testing.T
	srv     *httptest.Server
	conns   chan *fakeConn
	version string
	// rest answers every path but /gateway, so one server can stand in for a whole instance.
	rest http.Handler
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	g := &fakeGateway{t: t, conns: make(chan *fakeConn, 16), version: gatewayproto.DevVersion}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gateway" {
			if g.rest != nil {
				g.rest.ServeHTTP(w, r)
				return
			}
			http.NotFound(w, r)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		fc := &fakeConn{t: t, ws: ws, frames: make(chan gatewayproto.Frame, 64), done: make(chan struct{})}
		go fc.readLoop()
		g.conns <- fc
		<-fc.done
	}))
	t.Cleanup(g.srv.Close)
	return g
}

// next waits for the daemon's next connection.
func (g *fakeGateway) next() *fakeConn {
	g.t.Helper()
	select {
	case c := <-g.conns:
		return c
	case <-time.After(5 * time.Second):
		g.t.Fatal("the daemon did not connect")
		return nil
	}
}

// noConnectionWithin asserts the daemon stays away for d.
func (g *fakeGateway) noConnectionWithin(d time.Duration) {
	g.t.Helper()
	select {
	case <-g.conns:
		g.t.Fatal("the daemon connected when it should have waited")
	case <-time.After(d):
	}
}

type fakeConn struct {
	t       *testing.T
	ws      *websocket.Conn
	frames  chan gatewayproto.Frame
	done    chan struct{}
	seq     int64
	closeMu sync.Mutex
	status  websocket.StatusCode
}

func (c *fakeConn) readLoop() {
	defer close(c.done)
	_, client := schemas(c.t)
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			c.closeMu.Lock()
			c.status = websocket.CloseStatus(err)
			c.closeMu.Unlock()
			close(c.frames)
			return
		}
		validate(c.t, client, data, "a frame the daemon sent")
		var f gatewayproto.Frame
		require.NoError(c.t, json.Unmarshal(data, &f))
		c.frames <- f
	}
}

func (c *fakeConn) send(op gatewayproto.Opcode, d any, s *int64, eventType *string) {
	c.t.Helper()
	raw, err := json.Marshal(d)
	require.NoError(c.t, err)
	frame, err := json.Marshal(gatewayproto.Frame{Op: op, D: raw, S: s, T: eventType})
	require.NoError(c.t, err)
	server, _ := schemas(c.t)
	validate(c.t, server, frame, "a frame the fake gateway sent")
	_ = c.ws.Write(context.Background(), websocket.MessageText, frame)
}

func (c *fakeConn) hello(serverTime time.Time, version string, interval time.Duration) {
	c.send(gatewayproto.OpHello, gatewayproto.Hello{
		HeartbeatInterval: interval.Milliseconds(), ServerTime: serverTime, Version: version,
	}, nil, nil)
}

func (c *fakeConn) dispatch(eventType string, d any) {
	c.seq++
	s, t := c.seq, eventType
	c.send(gatewayproto.OpDispatch, d, &s, &t)
}

func (c *fakeConn) ready(sessionID string) {
	c.dispatch("READY", map[string]any{
		"session_id": sessionID,
		"user": map[string]any{
			"id": "1", "username": "ada", "display_name": "Ada", "email": "ada@example.com",
			"created_at": "2026-01-01T00:00:00Z",
		},
		"guilds": []any{},
	})
}

// guild and message are payloads the contract accepts: the schema validates every frame the fake sends, so
// a test cannot get away with an empty object where the server would send a whole one.
func guild(id string) map[string]any {
	return map[string]any{
		"id": id, "name": "Guild " + id, "owner_id": "1", "icon_hash": nil, "description": nil,
		"message_audit_enabled": false, "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z",
	}
}

func message(id, channelID, content string) map[string]any {
	return map[string]any{
		"id": id, "channel_id": channelID, "author_id": "1", "content": content, "type": 0,
		"reply_to_id": nil, "edited_at": nil, "created_at": "2026-01-01T00:00:00Z", "tags": nil,
	}
}

// expect waits for the next frame with op, skipping heartbeats unless that is what is asked for.
func (c *fakeConn) expect(op gatewayproto.Opcode) gatewayproto.Frame {
	c.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-c.frames:
			require.True(c.t, ok, "the connection closed while waiting for op %d", op)
			if f.Op == gatewayproto.OpHeartbeat && op != gatewayproto.OpHeartbeat {
				continue
			}
			require.Equalf(c.t, op, f.Op, "expected op %d", op)
			return f
		case <-timeout:
			c.t.Fatalf("no op %d arrived", op)
		}
	}
}

func (c *fakeConn) closeWith(code websocket.StatusCode, reason string) {
	_ = c.ws.Close(code, reason)
}

// closedWith waits for the daemon to close the connection and reports the code.
func (c *fakeConn) closedWith() websocket.StatusCode {
	c.t.Helper()
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		c.t.Fatal("the daemon did not close the connection")
	}
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	return c.status
}

// handshake does HELLO, expects IDENTIFY and answers READY, returning the IDENTIFY.
func (c *fakeConn) handshake(sessionID string) gatewayproto.Identify {
	c.t.Helper()
	c.hello(time.Now(), gatewayproto.DevVersion, time.Minute)
	f := c.expect(gatewayproto.OpIdentify)
	var id gatewayproto.Identify
	require.NoError(c.t, json.Unmarshal(f.D, &id))
	c.ready(sessionID)
	return id
}

// ---------- fakes for the daemon's side ----------

type fakeCreds struct {
	mu       sync.Mutex
	cred     session.Credential
	rejected []string
	revoked  int
	observed []time.Time
	// order records Observe and Current calls, to assert HELLO's clock is sampled before the token.
	order []string
}

func newFakeCreds(instanceURL string) *fakeCreds {
	return &fakeCreds{cred: session.Credential{
		InstanceURL: instanceURL, Username: "ada", AccessToken: "eyJ.access.1", Generation: 1,
	}}
}

func (f *fakeCreds) Current(ctx context.Context) (session.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, "current")
	if err := ctx.Err(); err != nil {
		return session.Credential{}, err
	}
	return f.cred, nil
}

func (f *fakeCreds) Rejected(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejected = append(f.rejected, token)
	f.cred.AccessToken = "eyJ.access.renewed"
}

func (f *fakeCreds) Revoked() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked++
}

func (f *fakeCreds) ObserveServerTime(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observed = append(f.observed, t)
	f.order = append(f.order, "observe")
}

func (f *fakeCreds) set(fn func(f *fakeCreds)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeCreds) get() fakeCreds {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fakeCreds{
		cred: f.cred, rejected: append([]string(nil), f.rejected...), revoked: f.revoked,
		observed: append([]time.Time(nil), f.observed...), order: append([]string(nil), f.order...),
	}
}

type recordingSink struct {
	mu     sync.Mutex
	events []string // "begin:N" or the event type
	panics string   // an event type that makes Dispatch panic
}

func (s *recordingSink) Begin(generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "begin")
}

func (s *recordingSink) Dispatch(eventType string, _ json.RawMessage) {
	s.mu.Lock()
	panics := s.panics
	s.events = append(s.events, eventType)
	s.mu.Unlock()
	if eventType == panics {
		panic("a sink that cannot apply " + eventType)
	}
}

func (s *recordingSink) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type harness struct {
	g      *fakeGateway
	creds  *fakeCreds
	sink   *recordingSink
	logs   *syncBuffer
	cancel context.CancelFunc
	done   chan struct{}
}

func start(t *testing.T, tweak ...func(*Options)) *harness {
	t.Helper()
	h := &harness{g: newFakeGateway(t), sink: &recordingSink{}, logs: &syncBuffer{}}
	h.creds = newFakeCreds(h.g.srv.URL)
	opts := Options{
		Credentials: h.creds, Sink: h.sink, Version: "dev",
		Log:      zerolog.New(h.logs).Level(zerolog.DebugLevel),
		RetryMin: 5 * time.Millisecond, RetryMax: 20 * time.Millisecond,
		RateLimitedFloor: 50 * time.Millisecond, ProtocolFloor: 50 * time.Millisecond,
		VersionHold: time.Hour, HelloTimeout: 2 * time.Second,
	}
	for _, f := range tweak {
		f(&opts)
	}
	ctx, cancel := context.WithCancel(t.Context())
	h.cancel, h.done = cancel, make(chan struct{})
	client := New(opts)
	go func() { defer close(h.done); client.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after its context was canceled")
		}
	})
	return h
}

// ---------- the handshake ----------

func TestItIdentifiesWithTheSessionsTokenAndItsVersion(t *testing.T) {
	h := start(t, func(o *Options) { o.Version = "0.1.0" })
	c := h.g.next()
	c.hello(time.Now(), "0.1.0", time.Minute)
	f := c.expect(gatewayproto.OpIdentify)

	var id gatewayproto.Identify
	require.NoError(t, json.Unmarshal(f.D, &id))
	assert.Equal(t, "eyJ.access.1", id.Token)
	assert.Equal(t, "0.1.0", id.Properties.Version, "an empty version is refused by the server")
	assert.Equal(t, "daemon", id.Properties.Client)
	assert.Equal(t, runtime.GOOS, id.Properties.OS)
	assert.Nil(t, id.Intents, "intents are reserved")

	c.ready("sess-1")
	require.Eventually(t, func() bool { return len(h.sink.seen()) == 2 }, time.Second, time.Millisecond)
	assert.Equal(t, []string{"begin", "READY"}, h.sink.seen(), "a fresh session is announced before its READY")
}

// ADR 0010: the token is checked against the instance's clock, and HELLO is the sample. Asked for after
// it, so a laptop that slept past its token's expiry does not IDENTIFY with it.
func TestHellosClockIsSampledBeforeTheTokenIsChosen(t *testing.T) {
	h := start(t)
	c := h.g.next()
	serverTime := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	c.hello(serverTime, gatewayproto.DevVersion, time.Minute)
	c.expect(gatewayproto.OpIdentify)

	got := h.creds.get()
	require.Len(t, got.observed, 1)
	assert.WithinDuration(t, serverTime, got.observed[0], time.Second)
	last := strings.Join(got.order, ",")
	assert.True(t, strings.HasSuffix(last, "observe,current"), "order was %s", last)
}

// ---------- staying connected ----------

func TestADroppedConnectionIsResumedFromTheLastSequence(t *testing.T) {
	h := start(t)
	c := h.g.next()
	c.handshake("sess-1")
	c.dispatch("GUILD_UPDATE", guild("10"))
	c.dispatch("GUILD_UPDATE", guild("10"))
	require.Eventually(t, func() bool { return len(h.sink.seen()) == 4 }, time.Second, time.Millisecond)
	c.closeWith(websocket.StatusCode(gatewayproto.CloseUnknownError), "")

	c2 := h.g.next()
	c2.hello(time.Now(), gatewayproto.DevVersion, time.Minute)
	f := c2.expect(gatewayproto.OpResume)
	var r gatewayproto.Resume
	require.NoError(t, json.Unmarshal(f.D, &r))
	assert.Equal(t, "sess-1", r.SessionID)
	assert.Equal(t, int64(3), r.Seq, "READY was 1, the two updates 2 and 3")
	assert.Equal(t, "eyJ.access.1", r.Token, "RESUME carries the token as well as the session id (M18)")

	c2.seq = 3
	c2.dispatch("RESUMED", map[string]any{})
	require.Eventually(t, func() bool { return len(h.sink.seen()) == 5 }, time.Second, time.Millisecond)
	assert.NotContains(t, h.sink.seen()[1:], "begin", "a resumed session is the same session")
}

func TestAnInvalidSessionIdentifiesAfreshOnTheSameConnection(t *testing.T) {
	h := start(t)
	c := h.g.next()
	c.handshake("sess-1")
	c.closeWith(websocket.StatusCode(gatewayproto.CloseUnknownError), "")

	c2 := h.g.next()
	c2.hello(time.Now(), gatewayproto.DevVersion, time.Minute)
	c2.expect(gatewayproto.OpResume)
	c2.send(gatewayproto.OpInvalidSess, false, nil, nil)
	c2.expect(gatewayproto.OpIdentify)
	c2.ready("sess-2")

	require.Eventually(t, func() bool { return len(h.sink.seen()) == 4 }, time.Second, time.Millisecond)
	assert.Equal(t, []string{"begin", "READY", "begin", "READY"}, h.sink.seen(),
		"the sink hears the session was lost before the new READY")
}

func TestReconnectOpResumes(t *testing.T) {
	h := start(t)
	c := h.g.next()
	c.handshake("sess-1")
	c.send(gatewayproto.OpReconnect, nil, nil, nil)
	assert.NotEqual(t, websocket.StatusNormalClosure, c.closedWith(),
		"closing with 1000 would tell the server the session may go")

	c2 := h.g.next()
	c2.hello(time.Now(), gatewayproto.DevVersion, time.Minute)
	c2.expect(gatewayproto.OpResume)
}

func TestHeartbeatsCarryTheLastSequence(t *testing.T) {
	h := start(t)
	c := h.g.next()
	c.hello(time.Now(), gatewayproto.DevVersion, 40*time.Millisecond)
	c.expect(gatewayproto.OpIdentify)
	c.ready("sess-1")

	// The first beat can leave before READY has been read, carrying null. Each is acknowledged, or the next
	// would end the connection.
	for range 10 {
		f := c.expect(gatewayproto.OpHeartbeat)
		c.send(gatewayproto.OpHeartbeatAck, nil, nil, nil)
		var last *int64
		require.NoError(t, json.Unmarshal(f.D, &last))
		if last != nil {
			assert.Equal(t, int64(1), *last, "READY was sequence 1")
			return
		}
	}
	t.Fatalf("no heartbeat carried a sequence number; sink saw %v", h.sink.seen())
}

// A heartbeat that goes unanswered means a connection TCP has not noticed is dead: close it, and resume.
func TestAnUnansweredHeartbeatEndsTheConnectionAndResumes(t *testing.T) {
	h := start(t)
	c := h.g.next()
	c.hello(time.Now(), gatewayproto.DevVersion, 40*time.Millisecond)
	c.expect(gatewayproto.OpIdentify)
	c.ready("sess-1")
	c.expect(gatewayproto.OpHeartbeat) // never acknowledged

	c.closedWith()
	c2 := h.g.next()
	c2.hello(time.Now(), gatewayproto.DevVersion, time.Minute)
	c2.expect(gatewayproto.OpResume)
	assert.Contains(t, h.logs.String(), "stopped acknowledging heartbeats")
}

func TestAnAnsweredHeartbeatKeepsTheConnection(t *testing.T) {
	h := start(t)
	c := h.g.next()
	c.hello(time.Now(), gatewayproto.DevVersion, 30*time.Millisecond)
	c.expect(gatewayproto.OpIdentify)
	c.ready("sess-1")
	for range 5 {
		c.expect(gatewayproto.OpHeartbeat)
		c.send(gatewayproto.OpHeartbeatAck, nil, nil, nil)
	}
	h.g.noConnectionWithin(50 * time.Millisecond)
}

// ---------- what each close code asks for ----------

func TestEachCloseCodeGetsItsAnswer(t *testing.T) {
	cases := []struct {
		code     int
		next     gatewayproto.Opcode // what the next connection opens with
		rejected bool
		revoked  bool
	}{
		{gatewayproto.CloseUnknownError, gatewayproto.OpResume, false, false},
		{gatewayproto.CloseSessionTimedOut, gatewayproto.OpResume, false, false},
		{gatewayproto.CloseTooSlow, gatewayproto.OpResume, false, false},
		{int(websocket.StatusServiceRestart), gatewayproto.OpResume, false, false},
		{4999, gatewayproto.OpResume, false, false}, // a code this build has no name for
		{gatewayproto.CloseInvalidSeq, gatewayproto.OpIdentify, false, false},
		{gatewayproto.CloseAuthenticationFailed, gatewayproto.OpResume, true, false},
		{gatewayproto.CloseSessionRevoked, gatewayproto.OpIdentify, false, true},
		{gatewayproto.CloseRateLimited, gatewayproto.OpResume, false, false},
		{gatewayproto.CloseDecodeError, gatewayproto.OpIdentify, false, false},
		{gatewayproto.CloseUnknownOpcode, gatewayproto.OpIdentify, false, false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.code), func(t *testing.T) {
			h := start(t)
			c := h.g.next()
			c.handshake("sess-1")
			c.closeWith(websocket.StatusCode(tc.code), "closing")

			c2 := h.g.next()
			c2.hello(time.Now(), gatewayproto.DevVersion, time.Minute)
			f := c2.expect(tc.next)
			got := h.creds.get()
			if tc.rejected {
				assert.Equal(t, []string{"eyJ.access.1"}, got.rejected, "the refused token is reported")
				var r gatewayproto.Resume
				require.NoError(t, json.Unmarshal(f.D, &r))
				assert.Equal(t, "eyJ.access.renewed", r.Token, "and the next attempt uses its successor")
			} else {
				assert.Empty(t, got.rejected)
			}
			if tc.revoked {
				assert.Equal(t, 1, got.revoked, "the session is told the sign-in ended")
			} else {
				assert.Zero(t, got.revoked)
			}
		})
	}
}

// 4010, and a HELLO naming a version this daemon cannot talk to, are not retried soon: neither changes
// until somebody upgrades something.
func TestAVersionMismatchIsNotRetriedSoon(t *testing.T) {
	t.Run("the daemon refuses the server", func(t *testing.T) {
		h := start(t, func(o *Options) { o.Version = "0.1.0" })
		c := h.g.next()
		c.hello(time.Now(), "0.2.0", time.Minute)
		assert.Equal(t, websocket.StatusNormalClosure, c.closedWith(), "and no IDENTIFY was sent")
		h.g.noConnectionWithin(200 * time.Millisecond)
		assert.Contains(t, h.logs.String(), "upgrade the client")
	})
	t.Run("the server refuses the daemon", func(t *testing.T) {
		h := start(t)
		c := h.g.next()
		c.handshake("sess-1")
		c.closeWith(gatewayproto.CloseVersionMismatch, "upgrade the client")
		h.g.noConnectionWithin(200 * time.Millisecond)
	})
}

// ---------- the daemon's own discipline ----------

func TestASignInThatChangesStartsAFreshSession(t *testing.T) {
	h := start(t)
	c := h.g.next()
	c.handshake("sess-1")

	h.creds.set(func(f *fakeCreds) { f.cred.Generation, f.cred.AccessToken = 2, "eyJ.grace" })
	c.closeWith(websocket.StatusCode(gatewayproto.CloseUnknownError), "")

	c2 := h.g.next()
	c2.hello(time.Now(), gatewayproto.DevVersion, time.Minute)
	f := c2.expect(gatewayproto.OpIdentify)
	var id gatewayproto.Identify
	require.NoError(t, json.Unmarshal(f.D, &id))
	assert.Equal(t, "eyJ.grace", id.Token, "a RESUME would name the previous sign-in's session")
}

func TestAPanickingSinkCostsTheConnectionNotTheDaemon(t *testing.T) {
	h := start(t)
	h.sink.mu.Lock()
	h.sink.panics = "GUILD_UPDATE"
	h.sink.mu.Unlock()
	c := h.g.next()
	c.handshake("sess-1")
	c.dispatch("GUILD_UPDATE", guild("10"))

	c2 := h.g.next()
	c2.hello(time.Now(), gatewayproto.DevVersion, time.Minute)
	c2.expect(gatewayproto.OpIdentify) // not RESUME: that would replay the event that panicked
	assert.Contains(t, h.logs.String(), "panicked")
}

func TestStoppingClosesNormally(t *testing.T) {
	h := start(t)
	c := h.g.next()
	c.handshake("sess-1")
	h.cancel()
	code := c.closedWith()
	assert.True(t, code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway || code == -1,
		"closed with %d", code)
}

// Rule 8: IDENTIFY and RESUME carry the access token. No frame reaches the log, at any level.
func TestNoTokenReachesTheLog(t *testing.T) {
	h := start(t)
	c := h.g.next()
	c.handshake("sess-1")
	c.dispatch("MESSAGE_CREATE", message("20", "30", "hello"))
	c.closeWith(websocket.StatusCode(gatewayproto.CloseAuthenticationFailed), "invalid token")
	c2 := h.g.next()
	c2.hello(time.Now(), gatewayproto.DevVersion, time.Minute)
	c2.expect(gatewayproto.OpResume)

	assert.NotContains(t, h.logs.String(), "eyJ.")
}

// Rule 19: a close reason is the server's text, and the daemon's log is read in a terminal.
func TestACloseReasonIsSanitizedBeforeItIsLogged(t *testing.T) {
	h := start(t)
	c := h.g.next()
	c.handshake("sess-1")
	c.closeWith(gatewayproto.CloseDecodeError, "bad\x1b[2K\u202eframe")
	h.g.next()

	logs := h.logs.String()
	assert.Contains(t, logs, "the gateway closed the connection")
	assert.NotContains(t, logs, "\x1b")
	assert.NotContains(t, logs, "\\u001b", "an escape survives as zerolog's own escaping of it")
	assert.NotContains(t, logs, "\u202e")
}

func TestGatewayURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://chat.example.com":      "wss://chat.example.com/gateway",
		"http://127.0.0.1:8080":         "ws://127.0.0.1:8080/gateway",
		"https://chat.example.com:8443": "wss://chat.example.com:8443/gateway",
	} {
		got, err := gatewayURL(in)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	_, err := gatewayURL("ftp://chat.example.com")
	assert.Error(t, err)
}
