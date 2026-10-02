// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package attach

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/daemon/internal/session"
	"github.com/Alexnex31/Norite/daemon/internal/state"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// ---------- the contract ----------

var (
	schemaOnce                 sync.Once
	daemonSchema, clientSchema *jsonschema.Schema
	schemaErr                  error
)

func schemas(t *testing.T) (daemon, client *jsonschema.Schema) {
	t.Helper()
	schemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.AssertFormat()
		for _, name := range []string{"daemon-ipc.schema.json", "gateway-events.schema.json"} {
			raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", name))
			if err != nil {
				schemaErr = err
				return
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				schemaErr = err
				return
			}
			if schemaErr = c.AddResource("https://norite.example/contracts/"+name, doc); schemaErr != nil {
				return
			}
		}
		const id = "https://norite.example/contracts/daemon-ipc.schema.json"
		if daemonSchema, schemaErr = c.Compile(id + "#/$defs/DaemonFrame"); schemaErr != nil {
			return
		}
		clientSchema, schemaErr = c.Compile(id + "#/$defs/ClientFrame")
	})
	require.NoError(t, schemaErr)
	return daemonSchema, clientSchema
}

func conforms(t *testing.T, s *jsonschema.Schema, data []byte, what string) {
	t.Helper()
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Errorf("%s is not JSON: %v", what, err)
		return
	}
	if err := s.Validate(inst); err != nil {
		t.Errorf("%s does not match the contract: %s\n%v", what, data, err)
	}
}

// ---------- a server under test ----------

type fakeSession struct {
	mu       sync.Mutex
	standing session.Standing
	account  session.Account
}

func (f *fakeSession) Status() (session.Standing, session.Account) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.standing, f.account
}

type relayFunc func(ctx context.Context, req ipc.Request) ipc.Response

func (f relayFunc) Do(ctx context.Context, req ipc.Request) ipc.Response { return f(ctx, req) }

func echoRelay() Relay {
	return relayFunc(func(_ context.Context, req ipc.Request) ipc.Response {
		status := 200
		body, _ := json.Marshal(map[string]string{"method": req.Method, "path": req.Path})
		return ipc.Response{Status: &status, Body: body}
	})
}

type testServer struct {
	*Server
	addr  string
	state *state.State
	sess  *fakeSession
	stop  func()
}

type serverOpt func(*Server)

func newTestServer(t *testing.T, relay Relay, opts ...serverOpt) *testServer {
	t.Helper()
	dir, err := os.MkdirTemp("", "natt")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sess := &fakeSession{standing: session.Live, account: session.Account{
		InstanceURL: "https://chat.example", UserID: "1", Username: "ada\x1b[2J", Generation: 1,
	}}
	st := state.New(zerolog.Nop(), state.DefaultLimits)
	// The state belongs to the session's sign-in, as the gateway client's Begin makes it on a real daemon:
	// events are forwarded only while the two agree.
	st.Begin(1)
	srv := New(Options{Session: sess, State: st, Relay: relay, Version: "dev", Log: zerolog.Nop()})
	for _, o := range opts {
		o(srv)
	}

	l, err := Listen(dir)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(ctx, l) }()
	ts := &testServer{Server: srv, addr: ipc.SocketPath(dir), state: st, sess: sess}
	ts.stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after its context was canceled")
		}
	}
	t.Cleanup(ts.stop)
	return ts
}

// attach connects the CLI-side client, the one daemon/ipc gives the CLI and the GUI.
func (ts *testServer) attach(t *testing.T, events bool) *ipc.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := ipc.DialAt(ctx, ts.addr)
	require.NoError(t, err)
	c, err := ipc.Attach(ctx, conn, ipc.Options{Client: "norite-test", Version: "dev", Events: events})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// rawClient speaks the protocol by hand, validating every frame against the contract in both directions.
type rawClient struct {
	t    *testing.T
	conn net.Conn
}

func (ts *testServer) raw(t *testing.T) *rawClient {
	t.Helper()
	conn, err := net.Dial("unix", ts.addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return &rawClient{t: t, conn: conn}
}

func (r *rawClient) send(op gatewayproto.Opcode, d any) {
	r.t.Helper()
	f, err := ipc.Encode(op, d)
	require.NoError(r.t, err)
	encoded, err := json.Marshal(f)
	require.NoError(r.t, err)
	if op == gatewayproto.OpIdentify || op == ipc.OpRequest {
		_, client := schemas(r.t)
		conforms(r.t, client, encoded, "the frame the test client sends")
	}
	require.NoError(r.t, ipc.WriteEncoded(r.conn, encoded))
}

// sendOffContract sends a frame the contract forbids, to see what the daemon does with it.
func (r *rawClient) sendOffContract(op gatewayproto.Opcode, d any) {
	r.t.Helper()
	f, err := ipc.Encode(op, d)
	require.NoError(r.t, err)
	require.NoError(r.t, ipc.WriteFrame(r.conn, f))
}

func (r *rawClient) recv() (gatewayproto.Frame, error) {
	r.t.Helper()
	_ = r.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	f, err := ipc.ReadFrame(r.conn, ipc.MaxDaemonFrame)
	if err != nil {
		return f, err
	}
	encoded, err := json.Marshal(f)
	require.NoError(r.t, err)
	daemon, _ := schemas(r.t)
	conforms(r.t, daemon, encoded, "a frame the daemon sends")
	return f, nil
}

func (r *rawClient) expect(op gatewayproto.Opcode) gatewayproto.Frame {
	r.t.Helper()
	f, err := r.recv()
	require.NoError(r.t, err)
	require.Equal(r.t, op, f.Op, "frame: %s", f.D)
	return f
}

func (r *rawClient) identify(version string, events bool) {
	r.t.Helper()
	r.expect(gatewayproto.OpHello)
	r.send(gatewayproto.OpIdentify, ipc.Identify{
		Properties: gatewayproto.IdentifyProperties{OS: "linux", Client: "raw", Version: version},
		Events:     events,
	})
}

// closedWith reads until the daemon's Close and returns it.
func (r *rawClient) closedWith() ipc.Close {
	r.t.Helper()
	for {
		f, err := r.recv()
		require.NoError(r.t, err, "the connection ended without a Close frame")
		if f.Op == ipc.OpClose {
			var c ipc.Close
			require.NoError(r.t, ipc.Decode(f, &c))
			_, err := r.recv()
			assert.ErrorIs(r.t, err, io.EOF, "Close is the last frame")
			return c
		}
	}
}

func guildPayload(id, name string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"id":%q,"name":%q,"owner_id":"1","icon_hash":null,"description":null,`+
		`"message_audit_enabled":false,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`,
		id, name))
}

func messagePayload(id, channelID, content string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"id":%q,"channel_id":%q,"author_id":"1","content":%q,"type":0,`+
		`"reply_to_id":null,"edited_at":null,"created_at":"2026-01-01T00:00:00Z","tags":null}`,
		id, channelID, content))
}

func readyPayload(guilds ...json.RawMessage) json.RawMessage {
	list, _ := json.Marshal(guilds)
	return json.RawMessage(`{"session_id":"s","user":{"id":"1","username":"ada","display_name":"Ada",` +
		`"email":"ada@example.com","created_at":"2026-01-01T00:00:00Z"},"guilds":` + string(list) + `}`)
}

// ---------- the handshake ----------

func TestReadyNamesTheAccountAndTheGuildsWithoutAToken(t *testing.T) {
	ts := newTestServer(t, echoRelay())
	ts.Begin(1)
	ts.Dispatch("READY", readyPayload(guildPayload("20", "Second"), guildPayload("3", "First")))

	r := ts.raw(t)
	r.identify("dev", false)
	f := r.expect(gatewayproto.OpDispatch)
	require.Equal(t, "READY", *f.T)
	assert.Equal(t, int64(1), *f.S)
	assert.NotContains(t, string(f.D), "eyJ")

	var ready ipc.Ready
	require.NoError(t, ipc.Decode(f, &ready))
	assert.Equal(t, ipc.StandingSignedIn, ready.Standing)
	require.NotNil(t, ready.Account)
	assert.Equal(t, "1", ready.Account.UserID)
	assert.NotContains(t, ready.Account.Username, "\x1b", "the stored username is foreign text (rule 19)")
	require.Len(t, ready.Guilds, 2)
	assert.Equal(t, []string{"3", "20"}, []string{ready.Guilds[0].ID, ready.Guilds[1].ID}, "ordered by id")
}

func TestASignedOutDaemonSaysSo(t *testing.T) {
	ts := newTestServer(t, echoRelay())
	ts.sess.mu.Lock()
	ts.sess.standing, ts.sess.account = session.SignedOut, session.Account{}
	ts.sess.mu.Unlock()

	c := ts.attach(t, false)
	assert.Equal(t, ipc.StandingSignedOut, c.Ready().Standing)
	assert.Nil(t, c.Ready().Account)
	assert.Equal(t, []ipc.GuildSummary{}, c.Ready().Guilds)
}

// TestADaemonStillSigningInIsNotSignedOut: a client must be able to tell a daemon that has not finished
// signing in from one nobody is signed in to, or it sends somebody to log in again a moment after a restart.
func TestADaemonStillSigningInIsNotSignedOut(t *testing.T) {
	ts := newTestServer(t, echoRelay())
	ts.sess.mu.Lock()
	ts.sess.standing, ts.sess.account = session.Starting, session.Account{}
	ts.sess.mu.Unlock()

	c := ts.attach(t, false)
	assert.Equal(t, ipc.StandingStarting, c.Ready().Standing)
	assert.Nil(t, c.Ready().Account)
}

// TestReadyNeverPairsOneAccountWithAnothersGuilds: after a login the session names the new account at once,
// while the state still holds the old sign-in's guilds until the gateway connection notices it ended.
func TestReadyNeverPairsOneAccountWithAnothersGuilds(t *testing.T) {
	ts := newTestServer(t, echoRelay())
	ts.Begin(1)
	ts.Dispatch("READY", readyPayload(guildPayload("20", "First account's")))

	ts.sess.mu.Lock()
	ts.sess.account = session.Account{InstanceURL: "https://chat.example", UserID: "2", Username: "bob",
		Generation: 2}
	ts.sess.mu.Unlock()

	c := ts.attach(t, false)
	require.NotNil(t, c.Ready().Account)
	assert.Equal(t, "2", c.Ready().Account.UserID)
	assert.Empty(t, c.Ready().Guilds, "the guilds belong to the sign-in that just ended")
}

func TestAnIncompatibleClientIsRefusedWithTheReason(t *testing.T) {
	ts := newTestServer(t, echoRelay(), func(s *Server) { s.version = "0.2.0" })
	r := ts.raw(t)
	r.identify("0.3.0", false)
	c := r.closedWith()
	assert.Equal(t, ipc.CloseVersionMismatch, c.Code)
	assert.Contains(t, c.Reason, "restart the daemon")
}

func TestIdentifyMustComeFirstAndOnlyOnce(t *testing.T) {
	ts := newTestServer(t, echoRelay())

	first := ts.raw(t)
	first.expect(gatewayproto.OpHello)
	first.send(ipc.OpRequest, ipc.Request{ID: "1", Method: "GET", Path: "/guilds/1"})
	assert.Equal(t, ipc.CloseNotIdentified, first.closedWith().Code)

	twice := ts.raw(t)
	twice.identify("dev", false)
	twice.expect(gatewayproto.OpDispatch)
	twice.send(gatewayproto.OpIdentify, ipc.Identify{
		Properties: gatewayproto.IdentifyProperties{OS: "linux", Client: "raw", Version: "dev"},
	})
	assert.Equal(t, ipc.CloseAlreadyIdentified, twice.closedWith().Code)
}

func TestAFrameTooLargeIsRefusedUnread(t *testing.T) {
	ts := newTestServer(t, echoRelay())
	r := ts.raw(t)
	r.identify("dev", false)
	r.expect(gatewayproto.OpDispatch)

	var prefix [4]byte
	prefix[0] = 0xff // far past MaxClientFrame, and nothing behind it
	_, err := r.conn.Write(prefix[:])
	require.NoError(t, err)
	assert.Equal(t, ipc.CloseDecodeError, r.closedWith().Code)
}

func TestTheClientPastTheLimitIsRefused(t *testing.T) {
	ts := newTestServer(t, echoRelay(), func(s *Server) { s.maxClients = 2 })
	ts.attach(t, false)
	ts.attach(t, false)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := ipc.DialAt(ctx, ts.addr)
	require.NoError(t, err)
	_, err = ipc.Attach(ctx, conn, ipc.Options{Client: "norite-test", Version: "dev"})
	var ce *ipc.CloseError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, ipc.CloseTooManyClients, ce.Code)
}

// TestAClientRunningAsAnotherAccountIsRefusedUnheard: the uid check refuses before HELLO, so such a client
// learns nothing, not even the daemon's version. Another account cannot be had in a test, so the daemon is
// told to expect one this process is not.
func TestAClientRunningAsAnotherAccountIsRefusedUnheard(t *testing.T) {
	ts := newTestServer(t, echoRelay(), func(s *Server) { s.wantUID = os.Getuid() + 1 })
	r := ts.raw(t)
	_, err := r.recv()
	assert.ErrorIs(t, err, io.EOF, "closed without a word")
}

// ---------- the listener ----------

func TestAStaleSocketIsReplacedAndAnythingElseIsNot(t *testing.T) {
	dir, err := os.MkdirTemp("", "natt")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := ipc.SocketPath(dir)

	stale, err := net.Listen("unix", path)
	require.NoError(t, err)
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, stale.Close())

	l, err := Listen(dir)
	require.NoError(t, err, "a socket left by a crashed daemon is replaced")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	require.NoError(t, l.Close())

	require.NoError(t, os.WriteFile(path, []byte("not mine"), 0o600))
	_, err = Listen(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a socket")
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "not mine", string(got), "left alone")
}

// ---------- the fan-out ----------

// TestEveryClientThatAskedReceivesEachEventOnce: in order, numbered from READY, with the payload as the
// gateway sent it — and a client that asked for none receives none.
func TestEveryClientThatAskedReceivesEachEventOnce(t *testing.T) {
	ts := newTestServer(t, echoRelay())
	watcher := ts.attach(t, true)
	other := ts.attach(t, true)
	quiet := ts.raw(t)
	quiet.identify("dev", false)
	quiet.expect(gatewayproto.OpDispatch)

	sent := []struct {
		typ  string
		data json.RawMessage
	}{
		{"GUILD_CREATE", guildPayload("10", "Guild")},
		{"MESSAGE_CREATE", messagePayload("100", "30", "hello \u202e there")},
		{"MESSAGE_DELETE", json.RawMessage(`{"id":"100","channel_id":"30","guild_id":"10"}`)},
	}
	for _, e := range sent {
		ts.Dispatch(e.typ, e.data)
	}

	for _, c := range []*ipc.Client{watcher, other} {
		for i, e := range sent {
			select {
			case got := <-c.Events():
				assert.Equal(t, e.typ, got.Type)
				assert.Equal(t, int64(i+2), got.Seq, "READY is 1")
				assert.JSONEq(t, string(e.data), string(got.Data), "forwarded as the gateway sent it")
			case <-time.After(5 * time.Second):
				t.Fatalf("event %d did not arrive", i)
			}
		}
	}

	// The quiet client's next frame is the answer to its request, not an event.
	quiet.send(ipc.OpRequest, ipc.Request{ID: "q", Method: "GET", Path: "/guilds/10"})
	assert.Equal(t, ipc.OpResponse, quiet.expect(ipc.OpResponse).Op)
}

// TestTheForwardedFramesAreTheContracts reads the fan-out's hand-assembled bytes through the schema, since
// they are built without the encoder.
func TestTheForwardedFramesAreTheContracts(t *testing.T) {
	ts := newTestServer(t, echoRelay())
	r := ts.raw(t)
	r.identify("dev", true)
	r.expect(gatewayproto.OpDispatch)

	ts.Dispatch("MESSAGE_CREATE", messagePayload("100", "30", `quote " and \ backslash`))
	f := r.expect(gatewayproto.OpDispatch)
	assert.Equal(t, "MESSAGE_CREATE", *f.T)
	assert.Equal(t, int64(2), *f.S)
}

// TestAFrozenClientIsDroppedWithoutStallingAHealthyOne is M20's second done-when. The frozen client
// identifies for events and then never reads again; the healthy one reads everything. Every event must reach
// the healthy client, the frozen one must be dropped as too slow, and the fan-out must not wait for it.
//
// Proved by removal: with a blocking enqueue, the fan-out stops at the frozen client's full queue and the
// healthy client's events stop with it.
func TestAFrozenClientIsDroppedWithoutStallingAHealthyOne(t *testing.T) {
	ts := newTestServer(t, echoRelay())

	frozen := ts.raw(t)
	frozen.identify("dev", true)
	frozen.expect(gatewayproto.OpDispatch)
	healthy := ts.attach(t, true)

	// Enough, in number and in bytes, to fill the frozen client's socket buffer and then its queue.
	const events = 2000
	content := strings.Repeat("x", 3500)
	fanned := make(chan struct{})
	go func() {
		defer close(fanned)
		for i := range events {
			ts.Dispatch("MESSAGE_CREATE", messagePayload(fmt.Sprint(1000+i), "30", content))
		}
	}()

	received := 0
	deadline := time.After(20 * time.Second)
	for received < events {
		select {
		case ev, ok := <-healthy.Events():
			require.True(t, ok, "the healthy client was closed: %v", healthy.Err())
			received++
			require.Equal(t, int64(received+1), ev.Seq, "in order, none missing")
		case <-deadline:
			t.Fatalf("the healthy client stalled after %d of %d events", received, events)
		}
	}
	select {
	case <-fanned:
	case <-time.After(5 * time.Second):
		t.Fatal("the fan-out is still blocked")
	}

	assert.Equal(t, ipc.CloseTooSlow, frozen.closedWith().Code,
		"the frozen client is told why, once it reads again")
	require.Eventually(t, func() bool { return ts.Clients() == 1 }, 5*time.Second, 10*time.Millisecond)
}

// TestAClientWatchingIsResyncedWhenTheDaemonsSessionChanges: a fresh session, its READY, and the sign-in
// ending each close every watching client with resync, and leave a client that watches nothing alone.
func TestAClientWatchingIsResyncedWhenTheDaemonsSessionChanges(t *testing.T) {
	ts := newTestServer(t, echoRelay())

	for _, change := range []struct {
		what string
		do   func()
	}{
		{"a fresh session", func() { ts.Begin(2) }},
		{"its READY", func() { ts.Dispatch("READY", readyPayload()) }},
		{"the sign-in ending", func() { ts.End() }},
	} {
		t.Run(change.what, func(t *testing.T) {
			watching := ts.attach(t, true)
			quiet := ts.attach(t, false)

			change.do()
			select {
			case <-watching.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("the watching client was not resynced")
			}
			var ce *ipc.CloseError
			require.ErrorAs(t, watching.Err(), &ce)
			assert.Equal(t, ipc.CloseResync, ce.Code)

			res, err := quiet.Do(context.Background(), "GET", "/guilds/1", nil)
			require.NoError(t, err, "a client holding no view is left alone")
			assert.Equal(t, 200, res.Status)
		})
	}
}

// TestADispatchWithNoPayloadIsNotForwarded: a hostile instance sending a dispatch without `d` must not be
// able to end every watching client's connection with a frame they cannot parse.
func TestADispatchWithNoPayloadIsNotForwarded(t *testing.T) {
	ts := newTestServer(t, echoRelay())
	c := ts.attach(t, true)
	ts.Dispatch("MESSAGE_DELETE", nil)
	ts.Dispatch("MESSAGE_CREATE", messagePayload("1", "30", "after"))
	select {
	case ev, ok := <-c.Events():
		require.True(t, ok, "the client was disconnected: %v", c.Err())
		assert.Equal(t, "MESSAGE_CREATE", ev.Type)
		assert.Equal(t, int64(2), ev.Seq)
	case <-time.After(5 * time.Second):
		t.Fatal("nothing arrived")
	}
}

func TestResumedIsNotForwardedAndChangesNothing(t *testing.T) {
	ts := newTestServer(t, echoRelay())
	c := ts.attach(t, true)
	ts.Dispatch("RESUMED", json.RawMessage(`{}`))
	ts.Dispatch("MESSAGE_CREATE", messagePayload("1", "30", "after"))
	select {
	case ev := <-c.Events():
		assert.Equal(t, "MESSAGE_CREATE", ev.Type)
		assert.Equal(t, int64(2), ev.Seq)
	case <-time.After(5 * time.Second):
		t.Fatal("nothing arrived")
	}
}

func TestStoppingTheDaemonSaysSo(t *testing.T) {
	ts := newTestServer(t, echoRelay())
	r := ts.raw(t)
	r.identify("dev", true)
	r.expect(gatewayproto.OpDispatch)

	go ts.stop()
	c := r.closedWith()
	assert.Equal(t, ipc.CloseGoingAway, c.Code)
}

// ---------- requests ----------

func TestRequestsAreAnsweredByID(t *testing.T) {
	ts := newTestServer(t, echoRelay())
	c := ts.attach(t, false)
	res, err := c.Do(context.Background(), "PATCH", "/guilds/1", map[string]string{"name": "x"})
	require.NoError(t, err)
	assert.Equal(t, 200, res.Status)
	assert.JSONEq(t, `{"method":"PATCH","path":"/guilds/1"}`, string(res.Body))
}

func TestRequestsBeyondTheBoundAreRefusedNotQueued(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, ipc.MaxInFlight)
	ts := newTestServer(t, relayFunc(func(ctx context.Context, _ ipc.Request) ipc.Response {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		status := 204
		return ipc.Response{Status: &status}
	}))
	defer close(release)

	r := ts.raw(t)
	r.identify("dev", false)
	r.expect(gatewayproto.OpDispatch)
	for i := range ipc.MaxInFlight {
		r.send(ipc.OpRequest, ipc.Request{ID: fmt.Sprint(i), Method: "GET", Path: "/guilds/1"})
	}
	for range ipc.MaxInFlight {
		<-started
	}

	r.send(ipc.OpRequest, ipc.Request{ID: "0", Method: "GET", Path: "/guilds/1"})
	r.send(ipc.OpRequest, ipc.Request{ID: "extra", Method: "GET", Path: "/guilds/1"})
	r.sendOffContract(ipc.OpRequest, ipc.Request{ID: "verb", Method: "TRACE", Path: "/guilds/1"})

	codes := map[string]string{}
	for range 3 {
		var resp ipc.Response
		require.NoError(t, ipc.Decode(r.expect(ipc.OpResponse), &resp))
		require.NotNil(t, resp.Error)
		codes[resp.ID] = resp.Error.Code
	}
	assert.Equal(t, map[string]string{
		"0": ipc.RelayBadRequest, "extra": ipc.RelayTooManyRequests, "verb": ipc.RelayBadRequest,
	}, codes)
}

// TestAClientThatLeavesCancelsItsRequests: the relay's context ends with the connection, so a request
// nobody will read the answer to stops rather than finishing for nobody.
func TestAClientThatLeavesCancelsItsRequests(t *testing.T) {
	canceled := make(chan struct{})
	ts := newTestServer(t, relayFunc(func(ctx context.Context, _ ipc.Request) ipc.Response {
		<-ctx.Done()
		close(canceled)
		return ipc.Response{Error: &ipc.RelayError{Code: ipc.RelayUnreachable, Message: "canceled"}}
	}))
	r := ts.raw(t)
	r.identify("dev", false)
	r.expect(gatewayproto.OpDispatch)
	r.send(ipc.OpRequest, ipc.Request{ID: "1", Method: "GET", Path: "/guilds/1"})
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, r.conn.Close())

	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("the request outlived its client")
	}
}

// TestANewerClientsExtraFieldStillGetsTheVersionAnswer: the daemon reads IDENTIFY's version before decoding
// it strictly, and a field the same version does not know is still refused as malformed.
func TestANewerClientsExtraFieldStillGetsTheVersionAnswer(t *testing.T) {
	ts := newTestServer(t, echoRelay(), func(s *Server) { s.version = "0.2.0" })
	identify := func(version string) ipc.Close {
		r := ts.raw(t)
		r.expect(gatewayproto.OpHello)
		r.sendOffContract(gatewayproto.OpIdentify, map[string]any{
			"properties": map[string]string{"os": "linux", "client": "raw", "version": version},
			"events":     false, "added_later": true,
		})
		return r.closedWith()
	}
	assert.Equal(t, ipc.CloseVersionMismatch, identify("0.3.0").Code)
	assert.Equal(t, ipc.CloseDecodeError, identify("0.2.0").Code)
}

// TestAnEventFromASignInThatEndedIsNotForwarded: after a login the session names the new account at once,
// while the gateway connection may still deliver the old one's events until it notices; a client told it is
// the new account must not be sent them.
func TestAnEventFromASignInThatEndedIsNotForwarded(t *testing.T) {
	ts := newTestServer(t, echoRelay())
	c := ts.attach(t, true)

	ts.sess.mu.Lock()
	ts.sess.account.Generation = 2
	ts.sess.mu.Unlock()
	ts.Dispatch("MESSAGE_CREATE", messagePayload("1", "30", "the old account's"))

	ts.state.Begin(2)
	ts.Dispatch("MESSAGE_CREATE", messagePayload("2", "30", "the new account's"))
	select {
	case ev, ok := <-c.Events():
		require.True(t, ok, "closed: %v", c.Err())
		assert.Contains(t, string(ev.Data), "the new account's", "the old sign-in's event was forwarded first")
	case <-time.After(5 * time.Second):
		t.Fatal("nothing arrived")
	}
}

// TestAClientArrivingAsTheDaemonStopsIsToldSo: not that the daemon is full.
func TestAClientArrivingAsTheDaemonStopsIsToldSo(t *testing.T) {
	ts := newTestServer(t, echoRelay())
	ts.mu.Lock()
	ts.closing = true
	ts.mu.Unlock()

	r := ts.raw(t)
	f, err := r.recv()
	require.NoError(t, err)
	require.Equal(t, ipc.OpClose, f.Op)
	var cl ipc.Close
	require.NoError(t, ipc.Decode(f, &cl))
	assert.Equal(t, ipc.CloseGoingAway, cl.Code)

	ts.mu.Lock()
	ts.closing = false
	ts.mu.Unlock()
}

// TestARelayedBodyCrossesTheSocketUnescaped: json.Marshal rewrites < > & as six-byte escapes inside a raw
// body too, which let a hostile instance's answer grow past the queue's bound and drop its client.
func TestARelayedBodyCrossesTheSocketUnescaped(t *testing.T) {
	body := json.RawMessage(`"` + strings.Repeat("<&>", 1000) + `"`)
	ts := newTestServer(t, relayFunc(func(context.Context, ipc.Request) ipc.Response {
		status := 200
		return ipc.Response{Status: &status, Body: body}
	}))
	r := ts.raw(t)
	r.identify("dev", false)
	r.expect(gatewayproto.OpDispatch)
	r.send(ipc.OpRequest, ipc.Request{ID: "1", Method: "GET", Path: "/guilds/1"})

	_ = r.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var prefix [4]byte
	_, err := io.ReadFull(r.conn, prefix[:])
	require.NoError(t, err)
	n := int(prefix[0])<<24 | int(prefix[1])<<16 | int(prefix[2])<<8 | int(prefix[3])
	assert.Less(t, n, len(body)+200, "the body grew on the way: %d bytes for a %d-byte body", n, len(body))
}
