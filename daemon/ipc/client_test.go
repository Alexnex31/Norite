// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
)

// fakeDaemon is the other end of a client under test, over net.Pipe. Every frame it sends and receives is
// validated against contracts/daemon-ipc.schema.json, so these tests also hold the Go types to the contract.
type fakeDaemon struct {
	t    *testing.T
	conn net.Conn
}

func newPair(t *testing.T) (client net.Conn, daemon *fakeDaemon) {
	t.Helper()
	c, d := net.Pipe()
	t.Cleanup(func() { _ = c.Close(); _ = d.Close() })
	return c, &fakeDaemon{t: t, conn: d}
}

func (d *fakeDaemon) send(op gatewayproto.Opcode, payload any, seq *int64, typ *string) {
	d.t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(d.t, err)
	encoded, err := json.Marshal(gatewayproto.Frame{Op: op, D: raw, S: seq, T: typ})
	require.NoError(d.t, err)
	daemonSchema, _ := schemas(d.t)
	if !conforms(d.t, daemonSchema, encoded, "a frame the daemon sends") {
		_ = d.conn.Close()
		return
	}
	if err := WriteEncoded(d.conn, encoded); err != nil {
		d.t.Logf("fake daemon: write: %v", err)
	}
}

func (d *fakeDaemon) recv() (gatewayproto.Frame, error) {
	f, err := ReadFrame(d.conn, MaxClientFrame)
	if err != nil {
		return f, err
	}
	encoded, err := json.Marshal(f)
	require.NoError(d.t, err)
	_, clientSchema := schemas(d.t)
	if !conforms(d.t, clientSchema, encoded, "a frame the client sends") {
		_ = d.conn.Close()
		return f, errors.New("the client sent a frame outside the contract")
	}
	return f, nil
}

func ptr[T any](v T) *T { return &v }

// handshake plays the daemon's half: HELLO, IDENTIFY, READY. It returns what the client identified with.
func (d *fakeDaemon) handshake(version string, ready Ready) Identify {
	d.t.Helper()
	d.send(gatewayproto.OpHello, Hello{Version: version}, nil, nil)
	var id Identify
	f, err := d.recv()
	if err == nil && f.Op != gatewayproto.OpIdentify {
		err = errors.New("not IDENTIFY")
	}
	if err == nil {
		err = Decode(f, &id)
	}
	if err != nil {
		// Not require: this runs on the fake's goroutine, where FailNow would leave the client waiting.
		d.t.Errorf("fake daemon: the client did not identify: %v", err)
		_ = d.conn.Close()
		return id
	}
	d.send(gatewayproto.OpDispatch, ready, ptr(int64(1)), ptr("READY"))
	return id
}

func (d *fakeDaemon) close(code int, reason string) {
	d.send(OpClose, Close{Code: code, Reason: reason}, nil, nil)
	_ = d.conn.Close()
}

func signedIn() Ready {
	return Ready{
		Standing: StandingSignedIn,
		Account:  &Account{InstanceURL: "https://chat.example", UserID: "100", Username: "alice"},
		Guilds:   []GuildSummary{{ID: "200", Name: "Guild", OwnerID: "100"}},
	}
}

func attach(t *testing.T, conn net.Conn, opts Options) (*Client, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return Attach(ctx, conn, opts)
}

func TestAttachingIdentifiesWithoutATokenAndReadsReady(t *testing.T) {
	conn, d := newPair(t)
	identified := make(chan Identify, 1)
	go func() { identified <- d.handshake("dev", signedIn()) }()

	c, err := attach(t, conn, Options{Client: "norite", Version: "0.1.0", Events: false})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	id := <-identified
	assert.Equal(t, "norite", id.Properties.Client)
	assert.Equal(t, "0.1.0", id.Properties.Version)
	assert.False(t, id.Events)

	assert.Equal(t, signedIn(), c.Ready())
	assert.Nil(t, c.Events(), "no stream was asked for")
}

func TestASignedOutDaemonsReadyHasNoAccountAndAnEmptyList(t *testing.T) {
	conn, d := newPair(t)
	go d.handshake("dev", Ready{Standing: StandingSignedOut, Guilds: []GuildSummary{}})

	c, err := attach(t, conn, Options{Client: "norite", Version: "dev"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	assert.Nil(t, c.Ready().Account)
}

// TestAnIncompatibleDaemonIsRefusedBeforeIdentifying: the client checks HELLO itself, so it can say what
// to do about it, and sends nothing to a daemon it cannot talk to.
func TestAnIncompatibleDaemonIsRefusedBeforeIdentifying(t *testing.T) {
	conn, d := newPair(t)
	received := make(chan error, 1)
	go func() {
		d.send(gatewayproto.OpHello, Hello{Version: "0.3.0"}, nil, nil)
		_, err := d.recv()
		received <- err
	}()

	_, err := attach(t, conn, Options{Client: "norite", Version: "0.2.0"})
	var ve *VersionError
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "0.3.0", ve.Daemon)
	assert.Contains(t, err.Error(), "norite daemon restart")

	assert.ErrorIs(t, <-received, io.EOF, "the client closed without identifying")
}

// TestADaemonThatRefusesSaysWhy: a Close during the handshake — too many clients, the daemon's own version
// check — comes back with its code and reason.
func TestADaemonThatRefusesSaysWhy(t *testing.T) {
	conn, d := newPair(t)
	go func() {
		d.send(gatewayproto.OpHello, Hello{Version: "dev"}, nil, nil)
		if _, err := d.recv(); err == nil {
			d.close(CloseTooManyClients, "the daemon is already serving 64 clients")
		}
	}()

	_, err := attach(t, conn, Options{Client: "norite", Version: "dev"})
	var ce *CloseError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, CloseTooManyClients, ce.Code)
	assert.Contains(t, err.Error(), "64 clients")
}

func TestAHandshakeIsBoundedByItsContext(t *testing.T) {
	conn, _ := newPair(t) // a daemon that never says HELLO

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := Attach(ctx, conn, Options{Client: "norite", Version: "dev"})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestRequestsInFlightAreAnsweredByTheirOwnIDs: two at once, answered in the opposite order.
func TestRequestsInFlightAreAnsweredByTheirOwnIDs(t *testing.T) {
	conn, d := newPair(t)
	go func() {
		d.handshake("dev", signedIn())
		var reqs []Request
		for range 2 {
			f, err := d.recv()
			if err != nil {
				return
			}
			var r Request
			require.NoError(t, Decode(f, &r))
			reqs = append(reqs, r)
		}
		for i := len(reqs) - 1; i >= 0; i-- {
			body, _ := json.Marshal(map[string]string{"path": reqs[i].Path})
			d.send(OpResponse, Response{ID: reqs[i].ID, Status: ptr(200), Body: body}, nil, nil)
		}
	}()

	c, err := attach(t, conn, Options{Client: "norite", Version: "dev"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	type answer struct {
		path string
		res  Result
		err  error
	}
	answers := make(chan answer, 2)
	for _, path := range []string{"/guilds/1", "/guilds/2"} {
		go func() {
			res, err := c.Do(context.Background(), "GET", path, nil)
			answers <- answer{path, res, err}
		}()
	}
	for range 2 {
		a := <-answers
		require.NoError(t, a.err)
		assert.Equal(t, 200, a.res.Status)
		assert.JSONEq(t, `{"path":"`+a.path+`"}`, string(a.res.Body), "each caller gets its own answer")
	}
}

func TestAStatusIsAResultAndARelayFailureIsAnError(t *testing.T) {
	conn, d := newPair(t)
	go func() {
		d.handshake("dev", signedIn())
		for _, resp := range []Response{
			{Status: ptr(404), Body: json.RawMessage(`{"code":"not_found","message":"no","request_id":"r"}`)},
			{Error: &RelayError{Code: RelayNotSignedIn, Message: "nobody is signed in"}},
		} {
			f, err := d.recv()
			if err != nil {
				return
			}
			var r Request
			require.NoError(t, Decode(f, &r))
			assert.Equal(t, "POST", r.Method)
			assert.JSONEq(t, `{"name":"x"}`, string(r.Body))
			resp.ID = r.ID
			d.send(OpResponse, resp, nil, nil)
		}
	}()

	c, err := attach(t, conn, Options{Client: "norite", Version: "dev"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	res, err := c.Do(context.Background(), "POST", "/guilds", map[string]string{"name": "x"})
	require.NoError(t, err, "a 404 is the instance's answer, not a failure to ask")
	assert.Equal(t, 404, res.Status)

	_, err = c.Do(context.Background(), "POST", "/guilds", map[string]string{"name": "x"})
	var re *RelayError
	require.ErrorAs(t, err, &re)
	assert.Equal(t, RelayNotSignedIn, re.Code)
}

// TestARequestOutlivedByItsConnectionFails: the daemon stops with a request in flight.
func TestARequestOutlivedByItsConnectionFails(t *testing.T) {
	conn, d := newPair(t)
	go func() {
		d.handshake("dev", signedIn())
		if _, err := d.recv(); err == nil {
			d.close(CloseGoingAway, "daemon stopping")
		}
	}()

	c, err := attach(t, conn, Options{Client: "norite", Version: "dev"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	_, err = c.Do(context.Background(), "GET", "/guilds/1", nil)
	var ce *CloseError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, CloseGoingAway, ce.Code)

	_, err = c.Do(context.Background(), "GET", "/guilds/1", nil)
	require.ErrorAs(t, err, &ce, "and so does every request after it")
}

func TestEventsArriveInOrderAndEndWithTheConnection(t *testing.T) {
	conn, d := newPair(t)
	go func() {
		id := d.handshake("dev", signedIn())
		require.True(t, id.Events)
		msg := json.RawMessage(`{"id":"300","channel_id":"201","author_id":"100",` +
			`"author":{"id":"100","username":"alice","display_name":"Alice"},` +
			`"content":"hi","type":0,"reply_to_id":null,"created_at":"2026-10-02T00:00:00Z","edited_at":null,` +
			`"tags":null}`)
		d.send(gatewayproto.OpDispatch, msg, ptr(int64(2)), ptr("MESSAGE_CREATE"))
		d.send(gatewayproto.OpDispatch, json.RawMessage(`{"id":"300","channel_id":"201","guild_id":"200"}`),
			ptr(int64(3)), ptr("MESSAGE_DELETE"))
		d.close(CloseResync, "the daemon's session started afresh")
	}()

	c, err := attach(t, conn, Options{Client: "norite-tui", Version: "dev", Events: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	var got []Event
	for ev := range c.Events() {
		got = append(got, ev)
	}
	require.Len(t, got, 2)
	assert.Equal(t, "MESSAGE_CREATE", got[0].Type)
	assert.Equal(t, int64(2), got[0].Seq)
	assert.Equal(t, "MESSAGE_DELETE", got[1].Type)

	var ce *CloseError
	require.ErrorAs(t, c.Err(), &ce)
	assert.Equal(t, CloseResync, ce.Code)
}

// TestADaemonSpeakingOutOfTurnEndsTheConnection: a dispatch to a client that asked for none is a daemon
// this client does not understand, and the one parser stops rather than skipping it.
func TestADaemonSpeakingOutOfTurnEndsTheConnection(t *testing.T) {
	conn, d := newPair(t)
	go func() {
		d.handshake("dev", signedIn())
		d.send(gatewayproto.OpDispatch, json.RawMessage(`{"id":"300","channel_id":"201","guild_id":"200"}`),
			ptr(int64(2)), ptr("MESSAGE_DELETE"))
	}()

	c, err := attach(t, conn, Options{Client: "norite", Version: "dev"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the connection stayed open")
	}
	require.Error(t, c.Err())
	assert.Contains(t, c.Err().Error(), "asked for none")
}

func TestClosingTheClientFailsWhatIsWaiting(t *testing.T) {
	conn, d := newPair(t)
	go func() {
		d.handshake("dev", signedIn())
		_, _ = d.recv() // and never answer
		_, _ = io.Copy(io.Discard, d.conn)
	}()

	c, err := attach(t, conn, Options{Client: "norite", Version: "dev"})
	require.NoError(t, err)

	failed := make(chan error, 1)
	go func() {
		_, err := c.Do(context.Background(), "GET", "/guilds/1", nil)
		failed <- err
	}()
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, c.Close())

	select {
	case err := <-failed:
		assert.True(t, errors.Is(err, ErrClosed), "got %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the request outlived its connection")
	}
}

// TestANewerDaemonsExtraFieldStillGetsTheVersionAnswer: the version is read before the payload is decoded
// strictly, so a field a later release added to HELLO surfaces as "restart the daemon", not a decode error.
func TestANewerDaemonsExtraFieldStillGetsTheVersionAnswer(t *testing.T) {
	conn, d := newPair(t)
	go func() {
		// Not d.send: this HELLO is off the contract on purpose, as a newer daemon's would be.
		f, _ := Encode(1, nil)
		f.Op = 10
		f.D = json.RawMessage(`{"version":"0.3.0","added_later":true}`)
		_ = WriteFrame(d.conn, f)
	}()
	_, err := attach(t, conn, Options{Client: "norite", Version: "0.2.0"})
	var ve *VersionError
	require.ErrorAs(t, err, &ve, "got %v", err)
}

// TestAWriteCutShortEndsTheConnection: half a frame may be on the stream, and the next request's bytes would
// be read as the rest of it.
func TestAWriteCutShortEndsTheConnection(t *testing.T) {
	conn, d := newPair(t)
	go d.handshake("dev", signedIn()) // and then never reads again

	c, err := attach(t, conn, Options{Client: "norite", Version: "dev"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = c.Do(ctx, "POST", "/guilds", map[string]string{"name": "x"})
	require.Error(t, err)

	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the connection outlived a write that may have left half a frame behind")
	}
}

// TestNoBodyIsNil: "no body" crosses the socket as JSON null, and Result.Body is nil for it, as documented.
func TestNoBodyIsNil(t *testing.T) {
	conn, d := newPair(t)
	go func() {
		d.handshake("dev", signedIn())
		f, err := d.recv()
		if err != nil {
			return
		}
		var r Request
		_ = Decode(f, &r)
		d.send(OpResponse, Response{ID: r.ID, Status: ptr(200)}, nil, nil)
	}()
	c, err := attach(t, conn, Options{Client: "norite", Version: "dev"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	res, err := c.Do(context.Background(), "GET", "/guilds/1", nil)
	require.NoError(t, err)
	assert.Equal(t, 200, res.Status)
	assert.Nil(t, res.Body)
}
