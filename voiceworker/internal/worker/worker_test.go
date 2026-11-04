// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/voiceipc"
)

// fakeCall is a Call whose every step the test decides.
type fakeCall struct {
	offerErr error
	ready    chan struct{}
	ended    chan string
	closed   chan struct{}
	// stuck makes Close block for ever, as a library stuck in C would.
	stuck bool
	// slowGather makes Offer wait for its context, as a gather waiting out an unreachable TURN does.
	slowGather bool
}

func newFakeCall() *fakeCall {
	return &fakeCall{ready: make(chan struct{}), ended: make(chan string, 1), closed: make(chan struct{})}
}

func (c *fakeCall) Offer(ctx context.Context) (string, error) {
	if c.slowGather {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return "v=0\r\noffer\r\n", c.offerErr
}
func (c *fakeCall) Accept(string) error    { return nil }
func (c *fakeCall) Ready() <-chan struct{} { return c.ready }
func (c *fakeCall) Ended() <-chan string   { return c.ended }
func (c *fakeCall) Close() {
	close(c.closed)
	if c.stuck {
		select {}
	}
}

// daemon is the test's end of the pipes: it reads what the worker sends, holding every frame to the
// contract, and writes what a daemon would.
type daemon struct {
	t      *testing.T
	toW    *io.PipeWriter
	fromW  *io.PipeReader
	schema *jsonschema.Schema
	code   chan int
}

func start(t *testing.T, call *fakeCall, opts Options) *daemon {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	d := &daemon{t: t, toW: inW, fromW: outR, schema: workerSchema(t), code: make(chan int, 1)}
	opts.Version = "0.4.0"
	opts.Dial = func(voiceipc.Join) (Call, error) { return call, nil }
	go func() {
		d.code <- Run(context.Background(), inR, outW, opts)
		_ = outW.Close()
	}()
	t.Cleanup(func() { _ = inW.Close(); _ = outR.Close() })
	return d
}

func workerSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "voice-worker.schema.json"))
	require.NoError(t, err)
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	require.NoError(t, err)
	c := jsonschema.NewCompiler()
	const id = "https://norite.example/contracts/voice-worker.schema.json"
	require.NoError(t, c.AddResource(id, doc))
	s, err := c.Compile(id + "#/$defs/WorkerFrame")
	require.NoError(t, err)
	return s
}

// next reads the worker's next frame, held to the contract.
func (d *daemon) next() gatewayproto.Frame {
	d.t.Helper()
	type res struct {
		f   gatewayproto.Frame
		err error
	}
	got := make(chan res, 1)
	go func() {
		f, err := voiceipc.Read(d.fromW, voiceipc.MaxWorkerFrame)
		got <- res{f, err}
	}()
	select {
	case r := <-got:
		require.NoError(d.t, r.err)
		raw, err := ipc.Marshal(r.f)
		require.NoError(d.t, err)
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		require.NoError(d.t, err)
		require.NoError(d.t, d.schema.Validate(doc), "%s", raw)
		return r.f
	case <-time.After(5 * time.Second):
		d.t.Fatal("the worker sent nothing")
		return gatewayproto.Frame{}
	}
}

func (d *daemon) send(op gatewayproto.Opcode, v any) {
	d.t.Helper()
	require.NoError(d.t, voiceipc.Write(d.toW, op, v))
}

func (d *daemon) state() voiceipc.State {
	d.t.Helper()
	f := d.next()
	require.Equal(d.t, voiceipc.OpState, f.Op)
	var s voiceipc.State
	require.NoError(d.t, ipc.Decode(f, &s))
	return s
}

func (d *daemon) exit() int {
	d.t.Helper()
	select {
	case c := <-d.code:
		return c
	case <-time.After(5 * time.Second):
		d.t.Fatal("the worker did not exit")
		return -1
	}
}

// joined walks a worker to the answer.
func (d *daemon) joined() {
	d.t.Helper()
	f := d.next()
	require.Equal(d.t, voiceipc.OpHello, f.Op)
	d.send(voiceipc.OpJoin, voiceipc.Join{ICEServers: []voiceipc.ICEServer{}})
	require.Equal(d.t, voiceipc.OpOffer, d.next().Op)
	d.send(voiceipc.OpAnswer, voiceipc.Answer{SDP: "v=0\r\nanswer\r\n"})
	require.Equal(d.t, voiceipc.StateConnecting, d.state().State)
}

func TestACallGoesFromJoinToReadyAndEndsOnLeave(t *testing.T) {
	call := newFakeCall()
	d := start(t, call, Options{})
	d.joined()
	close(call.ready)
	require.Equal(t, voiceipc.StateReady, d.state().State)

	d.send(voiceipc.OpLeave, voiceipc.Leave{})
	s := d.state()
	require.Equal(t, voiceipc.StateEnded, s.State)
	require.Equal(t, "left", s.Reason)
	require.Equal(t, ExitOK, d.exit())
	<-call.closed
}

func TestACallThatEndsByItselfSaysWhyAndExits(t *testing.T) {
	call := newFakeCall()
	d := start(t, call, Options{})
	d.joined()
	close(call.ready)
	require.Equal(t, voiceipc.StateReady, d.state().State)
	call.ended <- "the voice server closed the call"
	s := d.state()
	require.Equal(t, voiceipc.State{State: voiceipc.StateEnded, Reason: "the voice server closed the call"}, s)
	require.Equal(t, ExitOK, d.exit())
}

func TestACallThatNeverConnectsEnds(t *testing.T) {
	d := start(t, newFakeCall(), Options{Connect: 50 * time.Millisecond})
	d.joined()
	s := d.state()
	require.Equal(t, voiceipc.StateEnded, s.State)
	require.Contains(t, s.Reason, "did not connect")
	require.Equal(t, ExitOK, d.exit())
}

func TestAFailedGatherEnds(t *testing.T) {
	call := newFakeCall()
	call.offerErr = errors.New("no network")
	d := start(t, call, Options{})
	require.Equal(t, voiceipc.OpHello, d.next().Op)
	d.send(voiceipc.OpJoin, voiceipc.Join{ICEServers: []voiceipc.ICEServer{}})
	s := d.state()
	require.Equal(t, voiceipc.StateEnded, s.State)
	require.Contains(t, s.Reason, "no network")
}

// The daemon going away is the end of the worker, at every step, with no frame owed to anybody.
func TestTheWorkerExitsWhenItsDaemonGoes(t *testing.T) {
	t.Run("before joining", func(t *testing.T) {
		d := start(t, newFakeCall(), Options{})
		require.Equal(t, voiceipc.OpHello, d.next().Op)
		require.NoError(t, d.toW.Close())
		require.Equal(t, ExitOK, d.exit())
	})
	t.Run("waiting for the answer", func(t *testing.T) {
		call := newFakeCall()
		d := start(t, call, Options{})
		require.Equal(t, voiceipc.OpHello, d.next().Op)
		d.send(voiceipc.OpJoin, voiceipc.Join{ICEServers: []voiceipc.ICEServer{}})
		require.Equal(t, voiceipc.OpOffer, d.next().Op)
		require.NoError(t, d.toW.Close())
		require.Equal(t, ExitOK, d.exit())
		<-call.closed
	})
	t.Run("in a call", func(t *testing.T) {
		call := newFakeCall()
		d := start(t, call, Options{})
		d.joined()
		require.NoError(t, d.toW.Close())
		require.Equal(t, ExitOK, d.exit())
		<-call.closed
	})
}

// A call stuck inside its close does not keep the worker past its bound, and Gone is told the moment the
// pipe ends, whatever the loop is doing.
func TestAStuckCallDoesNotKeepTheWorker(t *testing.T) {
	call := newFakeCall()
	call.stuck = true
	gone := make(chan error, 1)
	d := start(t, call, Options{Close: 50 * time.Millisecond, Gone: func(err error) { gone <- err }})
	d.joined()
	require.NoError(t, d.toW.Close())
	select {
	case err := <-gone:
		require.ErrorIs(t, err, io.EOF)
	case <-time.After(5 * time.Second):
		t.Fatal("Gone was not called")
	}
	require.Equal(t, ExitOK, d.exit())
}

// A frame out of order is a bug in a program of the same install: the worker says so and exits non-zero.
func TestAFrameOutOfOrderIsRefused(t *testing.T) {
	for name, send := range map[string]func(d *daemon){
		"an answer before the join": func(d *daemon) { d.send(voiceipc.OpAnswer, voiceipc.Answer{SDP: "x"}) },
		"a worker's own op":         func(d *daemon) { d.send(voiceipc.OpHello, voiceipc.Hello{Version: "x"}) },
		"a join with a field nobody declared": func(d *daemon) {
			d.send(voiceipc.OpJoin, map[string]any{"ice_servers": []any{}, "loopback": false, "token": "x"})
		},
		"a join naming too many servers": func(d *daemon) {
			many := make([]voiceipc.ICEServer, voiceipc.MaxICEServers+1)
			for i := range many {
				many[i] = voiceipc.ICEServer{URLs: []string{"turn:192.0.2.1:3478"}}
			}
			d.send(voiceipc.OpJoin, voiceipc.Join{ICEServers: many})
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := start(t, newFakeCall(), Options{})
			require.Equal(t, voiceipc.OpHello, d.next().Op)
			send(d)
			s := d.state()
			require.Equal(t, voiceipc.StateEnded, s.State)
			require.Contains(t, s.Reason, "protocol")
			require.Equal(t, ExitProtocol, d.exit())
		})
	}
}

// A leave or a closed pipe while gathering is answered at once, not when the gather ends: the daemon kills
// a worker that has not left within three seconds, and a gather can take longer.
func TestALeaveWhileGatheringIsAnsweredAtOnce(t *testing.T) {
	t.Run("a leave", func(t *testing.T) {
		call := newFakeCall()
		call.slowGather = true
		d := start(t, call, Options{Gather: time.Minute})
		require.Equal(t, voiceipc.OpHello, d.next().Op)
		d.send(voiceipc.OpJoin, voiceipc.Join{ICEServers: []voiceipc.ICEServer{}})
		started := time.Now()
		d.send(voiceipc.OpLeave, voiceipc.Leave{})
		require.Equal(t, voiceipc.State{State: voiceipc.StateEnded, Reason: "left"}, d.state())
		require.Equal(t, ExitOK, d.exit())
		require.Less(t, time.Since(started), time.Second)
	})
	t.Run("a closed pipe", func(t *testing.T) {
		call := newFakeCall()
		call.slowGather = true
		d := start(t, call, Options{Gather: time.Minute})
		require.Equal(t, voiceipc.OpHello, d.next().Op)
		d.send(voiceipc.OpJoin, voiceipc.Join{ICEServers: []voiceipc.ICEServer{}})
		started := time.Now()
		require.NoError(t, d.toW.Close())
		require.Equal(t, ExitOK, d.exit())
		require.Less(t, time.Since(started), time.Second)
	})
}

// A leave before the join is a leave: the daemon can be asked to leave between starting a worker and
// sending its join, and the two writes race for the pipe.
func TestALeaveBeforeTheJoinIsALeave(t *testing.T) {
	d := start(t, newFakeCall(), Options{})
	require.Equal(t, voiceipc.OpHello, d.next().Op)
	d.send(voiceipc.OpLeave, voiceipc.Leave{})
	require.Equal(t, voiceipc.State{State: voiceipc.StateEnded, Reason: "left"}, d.state())
	require.Equal(t, ExitOK, d.exit())
}
