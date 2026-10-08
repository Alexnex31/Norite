// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package automation

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// A token shaped like a real one that says it is not.
const botToken = "nat_EXAMPLEexampleEXAMPLEexampleEXAMPLEexample0"

// ---------- the contract ----------

const schemaID = "https://norite.example/contracts/daemon-automation.schema.json"

var (
	schemaOnce                 sync.Once
	daemonSchema, scriptSchema *jsonschema.Schema
	schemaErr                  error
)

func schemas(t *testing.T) (daemon, script *jsonschema.Schema) {
	t.Helper()
	schemaOnce.Do(func() {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "daemon-automation.schema.json"))
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
		if schemaErr = c.AddResource(schemaID, doc); schemaErr != nil {
			return
		}
		if daemonSchema, schemaErr = c.Compile(schemaID + "#/$defs/DaemonFrame"); schemaErr != nil {
			return
		}
		scriptSchema, schemaErr = c.Compile(schemaID + "#/$defs/ScriptFrame")
	})
	require.NoError(t, schemaErr)
	return daemonSchema, scriptSchema
}

func conforms(t *testing.T, s *jsonschema.Schema, f gatewayproto.Frame, what string) {
	t.Helper()
	data, err := ipc.Marshal(f)
	require.NoError(t, err)
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	require.NoError(t, err)
	if err := s.Validate(inst); err != nil {
		t.Errorf("%s does not match the contract: %s\n%v", what, data, err)
	}
}

// ---------- the fixture ----------

type seen struct {
	Method, Path, Query, Authorization, UserAgent, ContentType, Body string
}

type fixture struct {
	t        *testing.T
	srv      *Server
	dir      string
	instance *httptest.Server
	cancel   context.CancelFunc
	served   chan struct{}

	mu       sync.Mutex
	requests []seen
	answer   http.HandlerFunc
	signedIn bool
	// absent is why there is no instance while signedIn is false.
	absent error
	clock  time.Time
}

func (f *fixture) InstanceURL() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.signedIn {
		if f.absent != nil {
			return "", f.absent
		}
		return "", ErrSignedOut
	}
	return f.instance.URL + "/norite", nil
}

func (f *fixture) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clock
}

func (f *fixture) advance(d time.Duration) {
	f.mu.Lock()
	f.clock = f.clock.Add(d)
	f.mu.Unlock()
}

func (f *fixture) seen() []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seen(nil), f.requests...)
}

func (f *fixture) answerWith(h http.HandlerFunc) {
	f.mu.Lock()
	f.answer = h
	f.mu.Unlock()
}

func newFixture(t *testing.T, tune func(*Options)) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: t.TempDir(), signedIn: true, clock: time.Unix(1_800_000_000, 0), served: make(chan struct{})}
	f.answer = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"30","type":1}`))
	}
	f.instance = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, seen{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Authorization: r.Header.Get("Authorization"),
			UserAgent: r.Header.Get("User-Agent"), ContentType: r.Header.Get("Content-Type"), Body: string(body),
		})
		h := f.answer
		f.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(f.instance.Close)

	// http.DefaultClient follows redirects. Open must not let that decide: see
	// TestTheInstancesAnswerComesBackAsItIs, whose 302 is asked once.
	opts := Options{
		StateDir: f.dir, Instance: f, EnabledFor: f.instance.URL + "/norite/", Version: "1.2.3",
		Log: zerolog.Nop(), now: f.now, HTTP: http.DefaultClient,
	}
	if tune != nil {
		tune(&opts)
	}
	srv, err := Open(opts)
	require.NoError(t, err)
	f.srv = srv

	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	go func() {
		srv.Serve(ctx)
		close(f.served)
	}()
	t.Cleanup(f.stop)
	return f
}

func (f *fixture) stop() {
	f.cancel()
	select {
	case <-f.served:
	case <-time.After(10 * time.Second):
		f.t.Error("Serve did not return after its context ended")
	}
	f.srv.Close()
}

func (f *fixture) file() ipc.AutomationFile {
	f.t.Helper()
	file, err := ipc.LoadAutomationFile(f.dir)
	require.NoError(f.t, err)
	return file
}

// script is a client that knows the secrets and holds every frame, in both directions, to the contract.
func (f *fixture) script() *ipc.AutomationClient {
	f.t.Helper()
	c, err := ipc.DialAutomation(context.Background(), f.file(), botToken)
	require.NoError(f.t, err)
	f.t.Cleanup(func() { _ = c.Close() })
	return c
}

// raw is a connection that speaks frames by hand, for what a correct client never sends.
type raw struct {
	t    *testing.T
	conn net.Conn
}

func (f *fixture) raw() *raw {
	f.t.Helper()
	conn, err := net.Dial("tcp4", f.srv.Address())
	require.NoError(f.t, err)
	f.t.Cleanup(func() { _ = conn.Close() })
	return &raw{t: f.t, conn: conn}
}

func (r *raw) send(op gatewayproto.Opcode, d any) {
	r.t.Helper()
	frame, err := ipc.Encode(op, d)
	require.NoError(r.t, err)
	require.NoError(r.t, ipc.WriteFrame(r.conn, frame))
}

// next reads the daemon's next frame, checked against the contract.
func (r *raw) next() gatewayproto.Frame {
	r.t.Helper()
	_ = r.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	frame, err := ipc.ReadFrame(r.conn, ipc.MaxDaemonFrame)
	require.NoError(r.t, err)
	daemon, _ := schemas(r.t)
	conforms(r.t, daemon, frame, "a frame from the daemon")
	return frame
}

func (r *raw) closed() ipc.Close {
	r.t.Helper()
	frame := r.next()
	require.Equal(r.t, ipc.OpClose, frame.Op, "%s", frame.D)
	var cl ipc.Close
	require.NoError(r.t, ipc.Decode(frame, &cl))
	// And then the stream ends.
	_ = r.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, err := r.conn.Read(make([]byte, 1))
	require.ErrorIs(r.t, err, io.EOF)
	return cl
}

// silence reports that the daemon sent nothing at all and closed, or sent nothing within the wait.
//
// Closed is an end of stream or a reset: a connection closed with bytes it never read is reset rather than
// finished, which is what happens to a request longer than the four bytes the daemon looked at.
func (r *raw) silence(wait time.Duration) (closed bool) {
	r.t.Helper()
	_ = r.conn.SetReadDeadline(time.Now().Add(wait))
	n, err := r.conn.Read(make([]byte, 64))
	require.Zero(r.t, n, "the daemon said something to a connection that had not presented the secret")
	require.Error(r.t, err)
	return !errors.Is(err, os.ErrDeadlineExceeded)
}

func (r *raw) identify(f *fixture) {
	r.t.Helper()
	id := ipc.AutomationIdentify{Secret: f.file().Secret, Token: botToken}
	frame, err := ipc.Encode(ipc.OpAutomationIdentify, id)
	require.NoError(r.t, err)
	_, script := schemas(r.t)
	conforms(r.t, script, frame, "identify")
	require.NoError(r.t, ipc.WriteFrame(r.conn, frame))
	require.Equal(r.t, ipc.OpAutomationReady, r.next().Op)
}

// request sends one request and reads its answer. What it sends is held to the contract unless the path is
// one the contract itself forbids, which is what a hostile script sends.
func (r *raw) request(id, method, path string, body string) ipc.Response {
	r.t.Helper()
	frame, err := ipc.Encode(ipc.OpRequest, ipc.Request{ID: id, Method: method, Path: path, Body: json.RawMessage(body)})
	require.NoError(r.t, err)
	if strings.HasPrefix(path, "/") {
		_, script := schemas(r.t)
		conforms(r.t, script, frame, "a request")
	}
	require.NoError(r.t, ipc.WriteFrame(r.conn, frame))
	answer := r.next()
	require.Equal(r.t, ipc.OpResponse, answer.Op, "%s", answer.D)
	var resp ipc.Response
	require.NoError(r.t, ipc.Decode(answer, &resp))
	require.Equal(r.t, id, resp.ID)
	return resp
}

// ---------- the port doing its job ----------

// TestAScriptsRequestGoesToTheInstanceWithItsOwnToken is the milestone's sentence: a script presents the
// port secret and an API token, and its request arrives at the instance carrying that token.
func TestAScriptsRequestGoesToTheInstanceWithItsOwnToken(t *testing.T) {
	f := newFixture(t, nil)
	c := f.script()
	assert.Equal(t, "1.2.3", c.Version)

	resp, err := c.Do(context.Background(), "POST", "/channels/20/messages?x=1", json.RawMessage(`{"content":"<hi> & bye"}`))
	require.NoError(t, err)
	require.Nil(t, resp.Error)
	require.NotNil(t, resp.Status)
	assert.Equal(t, http.StatusCreated, *resp.Status)
	assert.JSONEq(t, `{"id":"30","type":1}`, string(resp.Body))

	got := f.seen()
	require.Len(t, got, 1)
	assert.Equal(t, seen{
		Method: "POST", Path: "/norite/api/v1/channels/20/messages", Query: "x=1",
		Authorization: "Bearer " + botToken, UserAgent: "norite-daemon/1.2.3 (automation)",
		ContentType: "application/json", Body: `{"content":"<hi> & bye"}`,
	}, got[0], "the instance's path prefix is kept, and the body arrives as it was sent")

	// A request without a body sends none, and says so by sending no content type.
	resp, err = c.Do(context.Background(), "GET", "/users/@me", nil)
	require.NoError(t, err)
	require.Nil(t, resp.Error)
	got = f.seen()
	require.Len(t, got, 2)
	assert.Empty(t, got[1].Body)
	assert.Empty(t, got[1].ContentType)
}

// TestTheInstancesAnswerComesBackAsItIs: whatever the status. A 401 is the script's to deal with, and the
// daemon asks once: there is no session here to renew anything, which is the difference from the relay.
func TestTheInstancesAnswerComesBackAsItIs(t *testing.T) {
	f := newFixture(t, nil)
	c := f.script()

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusFound, http.StatusInternalServerError} {
		f.answerWith(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "/api/v1/elsewhere")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"code":"x","message":"m","request_id":"r"}}`))
		})
		before := len(f.seen())
		resp, err := c.Do(context.Background(), "GET", "/users/@me", nil)
		require.NoError(t, err)
		require.Nil(t, resp.Error)
		assert.Equal(t, status, *resp.Status)
		assert.Len(t, f.seen(), before+1, "status %d must be asked once and not followed or retried", status)
	}

	// Something that is not JSON, which a proxy in front of an instance sends when it fails.
	f.answerWith(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>bad gateway</html>"))
	})
	resp, err := c.Do(context.Background(), "GET", "/users/@me", nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadGateway, *resp.Status)
	assert.Equal(t, "null", string(resp.Body), "a body that is not JSON is not carried")
}

func TestAnAnswerPastTheBoundIsNotCarried(t *testing.T) {
	f := newFixture(t, nil)
	f.answerWith(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`"` + strings.Repeat("a", ipc.MaxResponseBody) + `"`))
	})
	resp, err := f.script().Do(context.Background(), "GET", "/users/@me", nil)
	require.NoError(t, err)
	require.NotNil(t, resp.Error)
	assert.Equal(t, ipc.RelayTooLarge, resp.Error.Code)
}

func TestASignedOutDaemonHasNowhereToSendARequest(t *testing.T) {
	f := newFixture(t, nil)
	f.mu.Lock()
	f.signedIn = false
	f.mu.Unlock()

	resp, err := f.script().Do(context.Background(), "GET", "/users/@me", nil)
	require.NoError(t, err)
	require.NotNil(t, resp.Error)
	assert.Equal(t, ipc.RelayNotSignedIn, resp.Error.Code)
	assert.Empty(t, f.seen())
}

// TestADaemonStillReadingItsSignInIsNotCalledSignedOut: a daemon waiting on its keyring is signed in, and
// "run norite login" would have its user replace a good sign-in. The script is told to try again.
func TestADaemonStillReadingItsSignInIsNotCalledSignedOut(t *testing.T) {
	f := newFixture(t, nil)
	f.mu.Lock()
	f.signedIn, f.absent = false, ErrSignInPending
	f.mu.Unlock()

	resp, err := f.script().Do(context.Background(), "GET", "/users/@me", nil)
	require.NoError(t, err)
	require.NotNil(t, resp.Error)
	assert.Equal(t, ipc.RelayUnreachable, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "try again")
	assert.NotContains(t, resp.Error.Message, "norite login")
	assert.Empty(t, f.seen())
}

func TestAnInstanceThatDoesNotAnswerIsUnreachableAndNamesNothing(t *testing.T) {
	f := newFixture(t, nil)
	f.instance.Close()
	resp, err := f.script().Do(context.Background(), "GET", "/users/@me", nil)
	require.NoError(t, err)
	require.NotNil(t, resp.Error)
	assert.Equal(t, ipc.RelayUnreachable, resp.Error.Code)
	assert.NotContains(t, resp.Error.Message, "127.0.0.1", "the daemon's own wording, not the dialer's")
}

// ---------- the tier ----------

// TestNothingIsSaidBeforeThePortSecret is what a stranger on the port learns: nothing. Not a version, not
// a reason. Each of these is something that can reach a loopback port without the secret.
func TestNothingIsSaidBeforeThePortSecret(t *testing.T) {
	f := newFixture(t, nil)

	// Connecting and saying nothing gets nothing back.
	assert.False(t, f.raw().silence(300*time.Millisecond), "a quiet connection is waited on, not answered")

	frame := func(op gatewayproto.Opcode, d string) []byte {
		payload, err := ipc.Marshal(gatewayproto.Frame{Op: op, D: json.RawMessage(d)})
		require.NoError(t, err)
		var buf bytes.Buffer
		require.NoError(t, ipc.WriteEncoded(&buf, payload))
		return buf.Bytes()
	}
	for name, sent := range map[string][]byte{
		// A browser, or anything else speaking HTTP: "POST" read as a length is over a gigabyte.
		"an HTTP request":            []byte("POST /api/v1/channels/20/messages HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n{}"),
		"a GET":                      []byte("GET / HTTP/1.1\r\n\r\n"),
		"a first frame past 4 KiB":   frame(ipc.OpAutomationIdentify, `{"secret":"`+strings.Repeat("a", 5000)+`","token":"nat_a"}`),
		"a request before identify":  frame(ipc.OpRequest, `{"id":"1","method":"GET","path":"/users/@me","body":null}`),
		"the attach socket's op":     frame(gatewayproto.OpIdentify, `{"properties":{},"events":true}`),
		"an identify with a field":   frame(ipc.OpAutomationIdentify, `{"secret":"a","token":"nat_a","version":"1"}`),
		"an identify that is a list": frame(ipc.OpAutomationIdentify, `["a","nat_a"]`),
		// Decoding alone would take this one: the payload is exactly an identify's. The op is what says so.
		"the right secrets, wrong op": frame(ipc.OpRequest, `{"secret":"`+f.file().Secret+`","token":"`+botToken+`"}`),
		"a frame that is not JSON":    {0, 0, 0, 4, 'n', 'o', 'p', 'e'},
		"an empty frame":              {0, 0, 0, 0},
	} {
		r := f.raw()
		_, err := r.conn.Write(sent)
		require.NoError(t, err, name)
		assert.True(t, r.silence(10*time.Second), "%s must be closed without a word", name)
	}
	assert.Empty(t, f.seen())
}

// TestAWrongSecretIsRefusedTheSameWhateverIsWrongWithIt: one code and one sentence, so the answer says
// nothing about how close a guess was. Each of these is paired with a perfectly good token.
func TestAWrongSecretIsRefusedTheSameWhateverIsWrongWithIt(t *testing.T) {
	f := newFixture(t, nil)
	right := f.file().Secret

	var answers []ipc.Close
	for _, secret := range []string{
		"", "x", right[:len(right)-1], right + "A", strings.ToUpper(right), strings.Repeat("A", len(right)),
		right[:len(right)-1] + flip(right[len(right)-1]),
	} {
		require.NotEqual(t, right, secret)
		r := f.raw()
		r.send(ipc.OpAutomationIdentify, ipc.AutomationIdentify{Secret: secret, Token: botToken})
		answers = append(answers, r.closed())
	}
	for _, a := range answers {
		assert.Equal(t, ipc.CloseAutomationRefused, a.Code)
		assert.Equal(t, answers[0].Reason, a.Reason)
	}
	assert.Empty(t, f.seen())
}

func flip(b byte) string {
	if b == 'A' {
		return "B"
	}
	return "A"
}

// TestOnlyAnAPITokenIsForwarded: an access token is a person's sign-in, with every reach the account has.
// A script that pasted one is refused rather than served, and so is anything that could not be a header.
func TestOnlyAnAPITokenIsForwarded(t *testing.T) {
	f := newFixture(t, nil)
	for name, tok := range map[string]string{
		"an access token":           "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2ln",
		"a refresh token":           "nrt_EXAMPLEexampleEXAMPLEexampleEXAMPLEexample0",
		"nothing":                   "",
		"the prefix alone":          "nat_",
		"a token with a line break": botToken + "\r\nX-Injected: 1",
		"a token with a space":      "nat_abc def",
		"a token far too long":      "nat_" + strings.Repeat("a", 300),
		"bearer and a token":        "Bearer " + botToken,
	} {
		r := f.raw()
		r.send(ipc.OpAutomationIdentify, ipc.AutomationIdentify{Secret: f.file().Secret, Token: tok})
		cl := r.closed()
		assert.Equal(t, ipc.CloseAutomationRefused, cl.Code, name)
		assert.Contains(t, cl.Reason, "not an API token", name)
		assert.NotContains(t, cl.Reason, "eyJ", "the reason must not repeat what was sent")
	}
	assert.Empty(t, f.seen())
}

// TestWhatTheDaemonAnswersItselfIsNotReachableWithASecret holds the ledger's condition: the config toggle
// is served to first-party clients because whoever can open the attach socket is the account. A script is
// not, so nothing under /@daemon/ is carried, and neither is any surface that manages credentials.
func TestWhatTheDaemonAnswersItselfIsNotReachableWithASecret(t *testing.T) {
	f := newFixture(t, nil)
	r := f.raw()
	r.identify(f)

	for i, path := range []string{
		ipc.PathConfig, ipc.PathConfigSplit, ipc.PathConfigUnsplit, "/@daemon/automation", "/@DAEMON/config",
		"/auth/tokens", "/auth/tokens/1", "/auth/login", "/auth/logout/all", "/AUTH/tokens",
		"/instance/bootstrap", "/users/@me/sessions", "/users/@me/sessions/1",
		"//evil.example/x", "/../etc/passwd", "/guilds/1/../../auth/tokens", "/guilds//1", "https://evil.example/x",
	} {
		resp := r.request("r"+string(rune('a'+i)), "POST", path, "null")
		require.NotNil(t, resp.Error, path)
		assert.Equal(t, ipc.RelayRefused, resp.Error.Code, path)
	}
	assert.Empty(t, f.seen(), "a refused path reaches nothing")

	// And the connection is still good for something it may ask.
	resp := r.request("ok", "GET", "/users/@me", "null")
	assert.Nil(t, resp.Error)
}

// TestThePackageHoldsNoCredentialSource is the structural half of "the daemon's own token is never used on
// this path": nothing here imports what holds one. A forwarder that cannot name the session cannot lend
// its reach by mistake.
func TestThePackageHoldsNoCredentialSource(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	var checked int
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		parsed, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		require.NoError(t, err)
		for _, imp := range parsed.Imports {
			for _, banned := range []string{"/internal/session", "/daemon/credentials", "/internal/gatewayclient", "/internal/state"} {
				assert.NotContains(t, imp.Path.Value, banned, "%s imports %s", name, imp.Path.Value)
			}
		}
	}
	require.NotZero(t, checked)
}

// ---------- what a script may not do once in ----------

func TestWhatClosesAnIdentifiedConnection(t *testing.T) {
	f := newFixture(t, nil)

	second := f.raw()
	second.identify(f)
	second.send(ipc.OpAutomationIdentify, ipc.AutomationIdentify{Secret: f.file().Secret, Token: botToken})
	assert.Equal(t, ipc.CloseAlreadyIdentified, second.closed().Code)

	unknown := f.raw()
	unknown.identify(f)
	unknown.send(gatewayproto.OpIdentify, map[string]any{})
	assert.Equal(t, ipc.CloseUnknownOpcode, unknown.closed().Code)

	extra := f.raw()
	extra.identify(f)
	extra.send(ipc.OpRequest, map[string]any{"id": "1", "method": "GET", "path": "/users/@me", "body": nil, "token": "nat_other"})
	assert.Equal(t, ipc.CloseDecodeError, extra.closed().Code)

	big := f.raw()
	big.identify(f)
	_, err := big.conn.Write([]byte{0x7f, 0xff, 0xff, 0xff})
	require.NoError(t, err)
	assert.Equal(t, ipc.CloseDecodeError, big.closed().Code)

	// The same with the frame's bytes behind its length, as a real script sends one. Closed with those
	// unread, the connection is reset and the script never learns why (M22 /code-review).
	sent := f.raw()
	sent.identify(f)
	oversized := make([]byte, 4+256<<10)
	binary.BigEndian.PutUint32(oversized, uint32(ipc.MaxClientFrame+1))
	_, err = sent.conn.Write(oversized)
	require.NoError(t, err)
	assert.Equal(t, ipc.CloseDecodeError, sent.closed().Code)

	method := f.raw()
	method.identify(f)
	// Not through request(), which holds what it sends to the contract: this is what the contract forbids.
	method.send(ipc.OpRequest, ipc.Request{ID: "1", Method: "TRACE", Path: "/users/@me", Body: json.RawMessage("null")})
	answer := method.next()
	var resp ipc.Response
	require.NoError(t, ipc.Decode(answer, &resp))
	require.NotNil(t, resp.Error)
	assert.Equal(t, ipc.RelayBadRequest, resp.Error.Code)

	assert.Empty(t, f.seen())
}

// TestTheRateIsSharedAndARefusedRequestDoesNotSpendIt: the budget is the port's, not a connection's, or a
// script would open another connection. A request the daemon refuses costs none of it.
func TestTheRateIsSharedAndARefusedRequestDoesNotSpendIt(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.Rate, o.Burst = 1, 2 })
	a, b := f.raw(), f.raw()
	a.identify(f)
	b.identify(f)

	for i := range 5 {
		resp := a.request("refused"+string(rune('0'+i)), "GET", "/auth/login", "null")
		require.Equal(t, ipc.RelayRefused, resp.Error.Code)
	}
	require.Nil(t, a.request("1", "GET", "/users/@me", "null").Error)
	require.Nil(t, b.request("2", "GET", "/users/@me", "null").Error)

	for _, r := range []*raw{a, b} {
		resp := r.request("3", "GET", "/users/@me", "null")
		require.NotNil(t, resp.Error)
		assert.Equal(t, ipc.RelayTooManyRequests, resp.Error.Code)
	}
	assert.Len(t, f.seen(), 2, "a request over the rate never reaches the instance")

	f.advance(time.Second)
	require.Nil(t, b.request("4", "GET", "/users/@me", "null").Error)
	require.NotNil(t, a.request("5", "GET", "/users/@me", "null").Error, "one second buys one request")

	// Nor does one refused because the daemon has nowhere to send it: a script retrying while its user is
	// signed out must not leave every other script rate-limited afterwards.
	f.advance(2 * time.Second)
	f.mu.Lock()
	f.signedIn = false
	f.mu.Unlock()
	for i := range 5 {
		resp := a.request("nowhere"+string(rune('0'+i)), "GET", "/users/@me", "null")
		require.Equal(t, ipc.RelayNotSignedIn, resp.Error.Code)
	}
	f.mu.Lock()
	f.signedIn = true
	f.mu.Unlock()
	require.Nil(t, a.request("6", "GET", "/users/@me", "null").Error)
	require.Nil(t, b.request("7", "GET", "/users/@me", "null").Error)
}

func TestTheBucketNeverHoldsMoreThanItsBurstAndSurvivesAClockGoingBack(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b := newBucket(10, 3, func() time.Time { return now })
	now = now.Add(time.Hour)
	for i := range 3 {
		require.True(t, b.take(), "%d", i)
	}
	require.False(t, b.take(), "an hour of quiet is still a burst of three")

	now = now.Add(-time.Hour)
	require.False(t, b.take())
	now = now.Add(100 * time.Millisecond)
	require.True(t, b.take())
}

// TestTheSeventeenthScriptIsToldThePortIsFullOnceItHasProvedItIsOne: the reason goes to something that
// presented the secret, and only then. A connection over the count that has presented nothing is a
// stranger, and hears nothing.
func TestTheSeventeenthScriptIsToldThePortIsFullOnceItHasProvedItIsOne(t *testing.T) {
	f := newFixture(t, nil)
	for range ipc.MaxAutomationConns {
		f.raw().identify(f)
	}
	over := f.raw()
	assert.False(t, over.silence(300*time.Millisecond), "nothing is said to it before its secrets")
	over.send(ipc.OpAutomationIdentify, ipc.AutomationIdentify{Secret: f.file().Secret, Token: botToken})
	assert.Equal(t, ipc.CloseTooManyClients, over.closed().Code)
}

// TestStrangersHoldingConnectionsOpenDoNotTakeAScriptsPlace: connections that never present the secret are
// counted apart. Thirty-two of them, the most the port keeps waiting, leave every script's place free, and
// the one after is closed without a word like any other stranger.
func TestStrangersHoldingConnectionsOpenDoNotTakeAScriptsPlace(t *testing.T) {
	f := newFixture(t, nil)
	for range maxPending {
		f.raw()
	}
	// The accept loop takes them in order, so once this one is refused all thirty-two are counted.
	// Well inside the five seconds every quiet connection is given, so this is the count closing it and
	// not the wait running out.
	assert.True(t, f.raw().silence(2*time.Second), "the thirty-third stranger is closed at once, silently")
}

func TestAScriptIsServedWhileStrangersWait(t *testing.T) {
	f := newFixture(t, nil)
	for range maxPending - 1 {
		f.raw()
	}
	r := f.raw()
	r.identify(f)
	require.Nil(t, r.request("1", "GET", "/users/@me", "null").Error)
	// Having identified, it no longer counts among those waiting: there is room for another.
	f.raw().identify(f)
}

// TestATokenIsNotSentToAnInstanceThePortWasNotEnabledFor: a token is a credential for the instance that
// minted it, and the daemon cannot tell which that was. Signing in to another instance must not hand the
// first one's tokens to the second, on a new connection or on one already open.
func TestATokenIsNotSentToAnInstanceThePortWasNotEnabledFor(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the other instance was sent a request, and with it the script's token")
	}))
	defer other.Close()

	f := newFixture(t, nil)
	open := f.raw()
	open.identify(f)
	require.Nil(t, open.request("1", "GET", "/users/@me", "null").Error)

	// The user signs in somewhere else.
	real := f.instance
	f.mu.Lock()
	f.instance = other
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.instance = real
		f.mu.Unlock()
	}()

	for _, r := range []*raw{open, func() *raw { r := f.raw(); r.identify(f); return r }()} {
		resp := r.request("2", "GET", "/users/@me", "null")
		require.NotNil(t, resp.Error)
		assert.Equal(t, ipc.RelayRefused, resp.Error.Code)
		assert.Contains(t, resp.Error.Message, "norite automation enable")
	}
	assert.Len(t, f.seen(), 1)
}

func TestWhichURLsNameOneInstance(t *testing.T) {
	assert.True(t, SameInstance("https://chat.example", "https://chat.example/"))
	assert.True(t, SameInstance("https://Chat.Example/norite", "https://chat.example/norite/"))
	for _, pair := range [][2]string{
		{"https://chat.example", "http://chat.example"},
		{"https://chat.example", "https://chat.example:8443"},
		{"https://chat.example", "https://chat.example.evil.test"},
		{"https://chat.example/a", "https://chat.example/b"},
		{"https://chat.example", ""},
		{"", ""},
		{"chat.example", "chat.example"},
	} {
		assert.False(t, SameInstance(pair[0], pair[1]), "%q and %q", pair[0], pair[1])
	}
}

// TestARequestWhoseIDCouldNotBeEchoedIsNotPerformed: the id goes back in the answer, so one the contract
// forbids would make the answer a frame the contract forbids. Refused before the instance is asked.
func TestARequestWhoseIDCouldNotBeEchoedIsNotPerformed(t *testing.T) {
	f := newFixture(t, nil)
	for _, id := range []string{"", "has space", strings.Repeat("a", 65), "new\nline", strings.Repeat("a", 100_000)} {
		r := f.raw()
		r.identify(f)
		r.send(ipc.OpRequest, ipc.Request{ID: id, Method: "POST", Path: "/channels/20/messages", Body: json.RawMessage(`{"content":"x"}`)})
		assert.Equal(t, ipc.CloseDecodeError, r.closed().Code, "%.20q", id)
	}
	assert.Empty(t, f.seen())
}

// flaky is a listener whose Accept fails a few times before it works, as one out of descriptors does.
type flaky struct {
	net.Listener
	mu    sync.Mutex
	fails int
}

func (l *flaky) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.fails > 0 {
		l.fails--
		l.mu.Unlock()
		return nil, errors.New("accept: too many open files")
	}
	l.mu.Unlock()
	return l.Listener.Accept()
}

// TestAFailedAcceptDoesNotEndThePort: leaving the loop on the first error would end the port for the rest
// of the daemon's run, with its file still naming it.
func TestAFailedAcceptDoesNotEndThePort(t *testing.T) {
	instance := &fixture{signedIn: true}
	instance.instance = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer instance.instance.Close()

	dir := t.TempDir()
	srv, err := Open(Options{StateDir: dir, Instance: instance, EnabledFor: instance.instance.URL + "/norite",
		HTTP: http.DefaultClient, Log: zerolog.Nop()})
	require.NoError(t, err)
	defer srv.Close()
	srv.listener = &flaky{Listener: srv.listener, fails: 3}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { srv.Serve(ctx); close(served) }()
	defer func() { cancel(); <-served }()

	file, err := ipc.LoadAutomationFile(dir)
	require.NoError(t, err)
	c, err := ipc.DialAutomation(context.Background(), file, botToken)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	resp, err := c.Do(context.Background(), "GET", "/users/@me", nil)
	require.NoError(t, err)
	require.Nil(t, resp.Error)
}

// TestStoppingTellsAWaitingScriptAndReturns: a script blocked on its next request is told, and Serve does
// not wait out the idle time.
func TestStoppingTellsAWaitingScriptAndReturns(t *testing.T) {
	f := newFixture(t, nil)
	r := f.raw()
	r.identify(f)
	require.Nil(t, r.request("1", "GET", "/users/@me", "null").Error)

	f.cancel()
	assert.Equal(t, ipc.CloseGoingAway, r.closed().Code)
	select {
	case <-f.served:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
}

// TestStoppingDoesNotWaitOutTheIdleTimeBehindARequestInFlight: a script in the middle of a request when
// the daemon stops is answered that the instance did not answer, and then told the daemon is stopping. Its
// connection must not go back to waiting five minutes for a next request.
func TestStoppingDoesNotWaitOutTheIdleTimeBehindARequestInFlight(t *testing.T) {
	f := newFixture(t, nil)
	arrived, release := make(chan struct{}), make(chan struct{})
	f.answerWith(func(http.ResponseWriter, *http.Request) {
		close(arrived)
		<-release
	})
	defer close(release)

	r := f.raw()
	r.identify(f)
	frame, err := ipc.Encode(ipc.OpRequest, ipc.Request{ID: "1", Method: "GET", Path: "/users/@me", Body: json.RawMessage("null")})
	require.NoError(t, err)
	require.NoError(t, ipc.WriteFrame(r.conn, frame))
	<-arrived

	f.cancel()
	var resp ipc.Response
	require.NoError(t, ipc.Decode(r.next(), &resp))
	require.NotNil(t, resp.Error)
	assert.Equal(t, ipc.RelayUnreachable, resp.Error.Code)
	assert.Equal(t, ipc.CloseGoingAway, r.closed().Code)
	select {
	case <-f.served:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve waited behind a connection that had gone back to idling")
	}
}

// ---------- the file and the address ----------

// TestTheFileNamesTheBoundPortAndIsGoneOnClose: the secret is this run's, the file is its owner's alone,
// and nothing is left naming a port that is no longer open.
func TestTheFileNamesTheBoundPortAndIsGoneOnClose(t *testing.T) {
	f := newFixture(t, nil)
	file := f.file()
	assert.Equal(t, f.srv.Address(), file.Address)
	host, _, err := net.SplitHostPort(file.Address)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", host, "the port is on the loopback literal and nowhere else")
	assert.Len(t, file.Secret, 43, "32 random bytes, unpadded")

	path := ipc.AutomationFilePath(f.dir)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	// Two runs never share a secret.
	other := newFixture(t, nil)
	assert.NotEqual(t, file.Secret, other.file().Secret)

	f.stop()
	_, err = os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist)
	_, err = ipc.LoadAutomationFile(f.dir)
	assert.ErrorIs(t, err, ipc.ErrAutomationOff)
}

// TestCloseLeavesAFileItDidNotWrite: a daemon that replaced this one has written its own.
func TestCloseLeavesAFileItDidNotWrite(t *testing.T) {
	f := newFixture(t, nil)
	theirs := `{"address":"127.0.0.1:7717","secret":"somebody-elses"}` + "\n"
	require.NoError(t, os.WriteFile(ipc.AutomationFilePath(f.dir), []byte(theirs), 0o600))
	f.stop()
	got, err := os.ReadFile(ipc.AutomationFilePath(f.dir))
	require.NoError(t, err)
	assert.Equal(t, theirs, string(got))
}

// TestATakenPortIsReportedAndNothingIsWritten: the daemon does not move to another port, which a script
// would not find, and writes no file naming a port it does not hold. A file already there is not this
// call's to remove: it may be the port this daemon is still serving, which the new one was to replace.
func TestATakenPortIsReportedAndNothingIsWritten(t *testing.T) {
	squatter, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = squatter.Close() }()
	port := squatter.Addr().(*net.TCPAddr).Port

	dir := t.TempDir()
	open := func(port int) (*Server, error) {
		return Open(Options{Port: port, StateDir: dir, Instance: &fixture{}, EnabledFor: "https://chat.example",
			HTTP: http.DefaultClient, Log: zerolog.Nop()})
	}
	_, err = open(port)
	require.ErrorIs(t, err, ErrPortTaken)
	_, err = os.Stat(ipc.AutomationFilePath(dir))
	assert.ErrorIs(t, err, os.ErrNotExist, "no file may name a port this daemon does not hold")

	serving, err := open(0)
	require.NoError(t, err)
	defer serving.Close()
	before, err := ipc.LoadAutomationFile(dir)
	require.NoError(t, err)
	_, err = open(port)
	require.ErrorIs(t, err, ErrPortTaken)
	after, err := ipc.LoadAutomationFile(dir)
	require.NoError(t, err, "a refused port took the file of the one still serving")
	assert.Equal(t, before, after)
}

// TestOnlyAPortInUseIsATakenPort: "taken" sends somebody looking for the process that has it. A port this
// account may not bind, or any other failure, has no such process.
func TestOnlyAPortInUseIsATakenPort(t *testing.T) {
	held, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = held.Close() }()
	_, err = net.Listen("tcp4", held.Addr().String())
	require.Error(t, err)
	assert.True(t, addressInUse(err))

	_, err = net.Listen("tcp4", "203.0.113.1:0") // not an address of this machine
	require.Error(t, err)
	assert.False(t, addressInUse(err), "%v", err)
	assert.False(t, addressInUse(os.ErrPermission))

	// And Open says so: an error that is not ErrPortTaken, with nothing left behind.
	real := listen
	listen = func(string, string) (net.Listener, error) {
		return nil, &net.OpError{Op: "listen", Net: "tcp4", Err: os.ErrPermission}
	}
	defer func() { listen = real }()
	dir := t.TempDir()
	_, err = Open(Options{Port: 80, StateDir: dir, Instance: &fixture{}, EnabledFor: "https://chat.example",
		HTTP: http.DefaultClient, Log: zerolog.Nop()})
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrPortTaken)
	assert.Contains(t, err.Error(), "permission denied")
	_, err = os.Stat(ipc.AutomationFilePath(dir))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestOpeningNeedsToKnowWhichInstanceThePortIsFor(t *testing.T) {
	for _, enabledFor := range []string{"", "chat.example", "/just/a/path"} {
		_, err := Open(Options{StateDir: t.TempDir(), Instance: &fixture{}, EnabledFor: enabledFor,
			HTTP: http.DefaultClient, Log: zerolog.Nop()})
		require.Error(t, err, "%q", enabledFor)
	}
}

func TestTheFileReplacesALinkRatherThanFollowingIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need a privilege on Windows")
	}
	dir := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "readable-by-others")
	require.NoError(t, os.WriteFile(elsewhere, []byte("untouched\n"), 0o644))
	require.NoError(t, os.Symlink(elsewhere, ipc.AutomationFilePath(dir)))

	srv, err := Open(Options{StateDir: dir, Instance: &fixture{}, EnabledFor: "https://chat.example",
		HTTP: http.DefaultClient, Log: zerolog.Nop()})
	require.NoError(t, err)
	defer srv.Close()

	got, err := os.ReadFile(elsewhere)
	require.NoError(t, err)
	assert.Equal(t, "untouched\n", string(got), "the secret must not be written through a link")
	info, err := os.Lstat(ipc.AutomationFilePath(dir))
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular())
}
