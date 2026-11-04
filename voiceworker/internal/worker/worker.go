// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package worker is the voice-worker's side of the pipes (daemon/voiceipc): it says who it is, makes the
// call's connection when it is told to join, and ends it when it is told to leave or when the call ends by
// itself.
//
// A worker is one call. It exits once that call is over, whichever way it ended, and the daemon starts a
// new one for the next (architecture.md §6). It exits too the moment its control pipe closes, without
// waiting to be told: every way the daemon ends closes that pipe, crash and exec included, and a worker
// that outlived its daemon would hold the microphone and an SFU connection nobody controls.
package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/voiceipc"
)

// Call is one connection to the SFU: internal/session's, or a test's.
type Call interface {
	// Offer adds the call's sections and returns the offer, every candidate gathered.
	Offer(ctx context.Context) (string, error)
	// Accept applies the SFU's answer, which starts ICE.
	Accept(sdp string) error
	// Ready is closed once the call is connected and the SFU has said ready.
	Ready() <-chan struct{}
	// Ended delivers why the call ended by itself, once.
	Ended() <-chan string
	// Close ends the call. It may block, which is why the worker bounds how long it waits for it.
	Close()
}

// Dial makes a call from a JOIN.
type Dial func(join voiceipc.Join) (Call, error)

// Options configures a run.
type Options struct {
	// Version is this worker's version, sent in HELLO.
	Version string
	Dial    Dial
	// Gather bounds the offer, Connect the time from the answer to ready, and Close how long a call is
	// given to close before the worker exits anyway. Zero values are the defaults.
	Gather, Connect, Close time.Duration
	// Gone, when set, is called from the reading goroutine the moment the control pipe ends, whatever the
	// loop is doing. The program sets it to exit after Close, so a call stuck in a library cannot keep a
	// worker alive past its daemon; the loop may still finish first and return.
	Gone func(err error)
}

// Exit codes. Anything else is a crash: Go exits 2 on a panic.
const (
	// ExitOK is a call that ended, however it ended, or a daemon that went away.
	ExitOK = 0
	// ExitProtocol is a daemon that broke the protocol. Both programs are one install's, so this is a bug.
	ExitProtocol = 1
)

const (
	defaultGather  = 10 * time.Second
	defaultConnect = 20 * time.Second
	defaultClose   = 2 * time.Second
)

// Run speaks the protocol on in and out until the call is over, and returns the exit code.
func Run(ctx context.Context, in io.Reader, out io.Writer, opts Options) int {
	if opts.Gather == 0 {
		opts.Gather = defaultGather
	}
	if opts.Connect == 0 {
		opts.Connect = defaultConnect
	}
	if opts.Close == 0 {
		opts.Close = defaultClose
	}
	r := &run{opts: opts, out: out, frames: make(chan gatewayproto.Frame), gone: make(chan error, 1),
		done: make(chan struct{})}
	defer close(r.done)
	go r.read(in)
	if err := voiceipc.Write(out, voiceipc.OpHello, voiceipc.Hello{Version: opts.Version}); err != nil {
		return ExitOK // the daemon is already gone
	}
	return r.loop(ctx)
}

type run struct {
	opts   Options
	out    io.Writer
	frames chan gatewayproto.Frame
	// gone carries the read loop's end: io.EOF when the daemon closed the pipe, anything else when what it
	// sent could not be read.
	gone chan error
	// done is closed when Run returns, so the reading goroutine stops handing frames to nobody.
	done chan struct{}
	call Call
}

// read hands each frame to the loop. It does nothing else, so a call stuck in a library elsewhere never
// keeps the worker from seeing its daemon go.
func (r *run) read(in io.Reader) {
	for {
		f, err := voiceipc.Read(in, voiceipc.MaxDaemonFrame)
		if err != nil {
			if r.opts.Gone != nil {
				r.opts.Gone(err)
			}
			r.gone <- err
			return
		}
		select {
		case r.frames <- f:
		case <-r.done:
			return
		}
	}
}

// errProtocol is a frame the daemon should not have sent.
var errProtocol = errors.New("the daemon broke the protocol")

func (r *run) loop(ctx context.Context) int {
	defer r.closeCall()

	join, code, ok := r.awaitJoin(ctx)
	if !ok {
		return code
	}
	call, err := r.opts.Dial(join)
	if err != nil {
		return r.end(fmt.Sprintf("the call could not be set up: %v", err))
	}
	r.call = call

	offer, code, ok := r.gather(ctx, call)
	if !ok {
		return code
	}
	if err := voiceipc.Write(r.out, voiceipc.OpOffer, voiceipc.Offer{SDP: offer}); err != nil {
		return ExitOK
	}

	answer, code, ok := r.awaitAnswer(ctx)
	if !ok {
		return code
	}
	if err := call.Accept(answer.SDP); err != nil {
		return r.end(fmt.Sprintf("the SFU's answer could not be applied: %v", err))
	}
	if err := voiceipc.Write(r.out, voiceipc.OpState, voiceipc.State{State: voiceipc.StateConnecting}); err != nil {
		return ExitOK
	}

	connect := time.NewTimer(r.opts.Connect)
	defer connect.Stop()
	ready := call.Ready()
	for {
		select {
		case <-ready:
			ready = nil
			connect.Stop()
			if err := voiceipc.Write(r.out, voiceipc.OpState, voiceipc.State{State: voiceipc.StateReady}); err != nil {
				return ExitOK
			}
		case <-connect.C:
			return r.end("the call did not connect in time")
		case reason := <-call.Ended():
			return r.end(reason)
		case f := <-r.frames:
			if f.Op != voiceipc.OpLeave {
				return r.violation(f)
			}
			var l voiceipc.Leave
			if err := ipc.Decode(f, &l); err != nil {
				return r.violation(f)
			}
			r.closeCall()
			return r.end("left")
		case err := <-r.gone:
			return r.daemonGone(err)
		case <-ctx.Done():
			return r.end("the worker was stopped")
		}
	}
}

// gather makes the offer while still hearing the daemon: gathering can take seconds (M28's pass on Windows
// saw about eight, Pion waiting out a TURN allocation that could not succeed), and a leave or a closed
// pipe in that time is answered at once, not after it. The daemon kills a worker that has not left
// within three seconds, and an ordinary leave must not read as a crash.
func (r *run) gather(ctx context.Context, call Call) (string, int, bool) {
	gatherCtx, cancel := context.WithTimeout(ctx, r.opts.Gather)
	defer cancel()
	type result struct {
		sdp string
		err error
	}
	done := make(chan result, 1)
	go func() {
		sdp, err := call.Offer(gatherCtx)
		done <- result{sdp, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			return "", r.end(fmt.Sprintf("gathering candidates failed: %v", res.err)), false
		}
		return res.sdp, 0, true
	case f := <-r.frames:
		cancel()
		var l voiceipc.Leave
		if f.Op != voiceipc.OpLeave || ipc.Decode(f, &l) != nil {
			return "", r.violation(f), false
		}
		r.closeCall()
		return "", r.end("left"), false
	case err := <-r.gone:
		cancel()
		return "", r.daemonGone(err), false
	case <-ctx.Done():
		return "", r.end("the worker was stopped"), false
	}
}

func (r *run) awaitJoin(ctx context.Context) (voiceipc.Join, int, bool) {
	select {
	case f := <-r.frames:
		// A leave may come first: the daemon can be asked to leave between starting the worker and sending
		// its join, and the two writes then race for the pipe. Ended as asked, as at every later step.
		var l voiceipc.Leave
		if f.Op == voiceipc.OpLeave && ipc.Decode(f, &l) == nil {
			return voiceipc.Join{}, r.end("left"), false
		}
		var j voiceipc.Join
		if f.Op != voiceipc.OpJoin || ipc.Decode(f, &j) != nil || j.Check() != nil {
			return j, r.violation(f), false
		}
		return j, 0, true
	case err := <-r.gone:
		return voiceipc.Join{}, r.daemonGone(err), false
	case <-ctx.Done():
		return voiceipc.Join{}, ExitOK, false
	}
}

func (r *run) awaitAnswer(ctx context.Context) (voiceipc.Answer, int, bool) {
	for {
		select {
		case f := <-r.frames:
			if f.Op == voiceipc.OpLeave {
				r.closeCall()
				return voiceipc.Answer{}, r.end("left"), false
			}
			var a voiceipc.Answer
			if f.Op != voiceipc.OpAnswer || ipc.Decode(f, &a) != nil || a.SDP == "" {
				return a, r.violation(f), false
			}
			return a, 0, true
		case reason := <-r.call.Ended():
			return voiceipc.Answer{}, r.end(reason), false
		case err := <-r.gone:
			return voiceipc.Answer{}, r.daemonGone(err), false
		case <-ctx.Done():
			return voiceipc.Answer{}, r.end("the worker was stopped"), false
		}
	}
}

// end reports the call ended, and why, as the worker's last frame.
func (r *run) end(reason string) int {
	reason = voiceipc.CutReason(reason)
	_ = voiceipc.Write(r.out, voiceipc.OpState, voiceipc.State{State: voiceipc.StateEnded, Reason: reason})
	return ExitOK
}

// violation is a frame out of order or malformed. The worker says so and exits: its daemon is of the same
// install, so this is a bug, and guessing what was meant would hide it.
func (r *run) violation(f gatewayproto.Frame) int {
	_ = voiceipc.Write(r.out, voiceipc.OpState, voiceipc.State{State: voiceipc.StateEnded,
		Reason: fmt.Sprintf("%v: op %d where it was not expected, or malformed", errProtocol, f.Op)})
	return ExitProtocol
}

// daemonGone is the control pipe's end. EOF is the daemon going away, by a leave that closed it or by its
// own end; anything else is a frame that could not be read. Either way there is nobody to report to.
func (r *run) daemonGone(err error) int {
	if errors.Is(err, io.EOF) {
		return ExitOK
	}
	return ExitProtocol
}

// closeCall closes the call once, waiting at most opts.Close: a library stuck inside it must not keep a
// worker whose daemon is gone alive.
func (r *run) closeCall() {
	if r.call == nil {
		return
	}
	call := r.call
	r.call = nil
	done := make(chan struct{})
	go func() {
		defer close(done)
		call.Close()
	}()
	select {
	case <-done:
	case <-time.After(r.opts.Close):
	}
}
