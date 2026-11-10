// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceworker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/termsafe"
	"github.com/Alexnex31/Norite/daemon/voiceipc"
)

// VoiceOptions configures the daemon's voice calls.
type VoiceOptions struct {
	// Program is the worker's path, and Version the daemon's.
	Program string
	Version string
	// Signaler reaches the voice server; nil refuses every join, and Unavailable says why.
	Signaler    Signaler
	Unavailable string
	// Account is the sign-in a call is joined under, or ErrSignedOut, or ErrSignInPending while the daemon
	// has not read its sign-in yet.
	Account func() (SignIn, error)
	// Changed is told the status each time it changes. It must not block.
	Changed func(ipc.VoiceStatus)
	Log     zerolog.Logger

	// Args and Env are the worker's, for a stand-in (Options). Tests only.
	Args, Env []string
}

// Bounds on a join, past which it is abandoned. Variables for the tests.
var (
	// routeWithin bounds asking for a credential, and answerWithin the provision.
	routeWithin  = 15 * time.Second
	answerWithin = 15 * time.Second
	// offerWithin bounds the worker's gather, and readyWithin the time from its answer to ready.
	offerWithin = 15 * time.Second
	readyWithin = 25 * time.Second
)

// Voice is the daemon's voice calls: one at a time, since a person is in one call. A join while in a call
// ends that call first.
type Voice struct {
	opts VoiceOptions

	// joinMu serializes joins. Leave never takes it, so a leave can end a join under way.
	joinMu sync.Mutex
	// pubMu orders what Changed is told: each publish sends the status as it is then, so concurrent
	// changes converge on the last.
	pubMu sync.Mutex

	mu      sync.Mutex
	status  ipc.VoiceStatus
	current *call
	closed  bool
}

// SignIn is the sign-in a call is joined under: the account, and a channel closed once that sign-in is
// over, signed out, refused or replaced by another (session.Source.Ended). The call ends with it, M19's rule
// that a sign-in that ends takes down everything standing on it. It is the session's, not the gateway
// connection's: a sign-out while the instance is unreachable is still the end of the sign-in, and the voice
// server may be reachable all the same.
type SignIn struct {
	Account string
	Ended   <-chan struct{}
}

// call is one call's worker and the goroutine that owns it.
type call struct {
	channel    string
	processing voiceipc.Processing
	cancel     context.CancelFunc
	// worker is set under Voice.mu once started.
	worker *Worker
	// finished is closed when the call's goroutine has returned and its worker is reaped.
	finished chan struct{}
}

// NewVoice builds the daemon's voice calls. Nothing runs until a join.
func NewVoice(opts VoiceOptions) *Voice {
	v := &Voice{opts: opts}
	v.status = ipc.VoiceStatus{State: ipc.VoiceIdle, Available: opts.Signaler != nil, Unavailable: opts.Unavailable}
	if opts.Signaler != nil {
		v.status.Unavailable = ""
	}
	return v
}

// Status is the calls' state now.
func (v *Voice) Status() ipc.VoiceStatus {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.status
}

// Busy is a call joined or joining: no moment for the updater to restart the daemon (M24).
func (v *Voice) Busy() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.current != nil
}

// The answers Account gives besides an id.
var (
	ErrSignedOut = errors.New("signed out")
	// ErrSignInPending is a daemon that has not read its sign-in yet: a keyring not yet unlocked, a store
	// not yet read. Not being signed out, which would send somebody to `norite login` over a good sign-in
	// (M22's automation.ErrSignInPending).
	ErrSignInPending = errors.New("the sign-in is not read yet")
)

// Refusal is a join that was not done, and why, for a person. Unavailable is one that could not be on this
// daemon at all; Pending one that may be asked again in a moment.
type Refusal struct {
	Msg         string
	Unavailable bool
	Pending     bool
	// Failed is a join that was tried and did not work: the worker or the voice server failed. Otherwise
	// the daemon understood and did not do it.
	Failed bool
}

func (r *Refusal) Error() string { return r.Msg }

// Join joins channel, ending any call first, and returns once the call is up or has failed. The worker
// processes the microphone as processing says.
func (v *Voice) Join(ctx context.Context, channel string, processing voiceipc.Processing) (ipc.VoiceStatus, error) {
	if v.opts.Signaler == nil {
		return v.Status(), &Refusal{Msg: v.opts.Unavailable, Unavailable: true}
	}
	signIn, err := v.opts.Account()
	switch {
	case errors.Is(err, ErrSignInPending):
		return v.Status(), &Refusal{Msg: "the daemon has not read its sign-in yet; ask again in a moment",
			Pending: true}
	case err != nil:
		return v.Status(), &Refusal{Msg: "nobody is signed in to this daemon; sign in with `norite login`"}
	}

	v.joinMu.Lock()
	defer v.joinMu.Unlock()
	// A join waits here behind another for as long as that one takes. One whose asker has gone by then
	// is not done: it would end the call the other join just made, for a channel nobody wants any more.
	if ctx.Err() != nil {
		return v.Status(), &Refusal{Msg: "the join was given up before its turn came"}
	}
	v.Leave()

	callCtx, cancel := context.WithCancel(context.Background())
	c := &call{channel: channel, processing: processing, cancel: cancel, finished: make(chan struct{})}
	result := make(chan error, 1)
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		cancel()
		return v.Status(), &Refusal{Msg: "the daemon is stopping"}
	}
	// The sign-in was read before the join waited its turn, and may have ended since: the join would go on
	// as an account nobody here is signed in as. Past this point the call watches it (endsWith).
	select {
	case <-signIn.Ended:
		v.mu.Unlock()
		cancel()
		return v.Status(), &Refusal{Msg: "the sign-in changed while the join waited; join again", Pending: true}
	default:
	}
	v.current = c
	asked := ipc.VoiceJoin(processing)
	v.status.State, v.status.Channel, v.status.Reason, v.status.Processing = ipc.VoiceJoining, channel, "", &asked
	v.mu.Unlock()
	v.publish()

	go v.endsWith(c, signIn.Ended)
	go v.run(callCtx, c, signIn.Account, result)
	select {
	case err := <-result:
		return v.Status(), err
	case <-ctx.Done():
		// The asker went away. The join goes on, and the status tells whoever asks next how it went.
		return v.Status(), ctx.Err()
	}
}

// run is one call, from the worker's start to its reaping. It reports the join's outcome on result once.
func (v *Voice) run(ctx context.Context, c *call, account string, result chan<- error) {
	defer close(c.finished)
	defer c.cancel()
	log := v.opts.Log.With().Str("channel", c.channel).Logger()

	var w *Worker
	// fail ends the join: the worker is reaped first, so that once the asker is answered and the status
	// says idle, nothing of this call is still running. A retry otherwise started a second worker beside
	// one still leaving, and a daemon stopping in between went on without it.
	//
	// The status is made idle before the asker is answered, so the answer and the status agree, and a retry
	// that comes at once finds no call of this one's to leave, which would put "left" over the reason.
	fail := func(failed bool, format string, args ...any) {
		msg := reason(fmt.Sprintf(format, args...))
		if w != nil {
			select {
			case <-w.Done():
			default:
				w.Leave()
			}
		}
		ev := log.Warn()
		if ctx.Err() != nil {
			// Left, by a leave, a join after it or the sign-in's end, which said so in the status: not a failure.
			msg, failed, ev = "the call was left before it was up", false, log.Info()
		}
		ev.Str("reason", msg).Msg("the voice call could not be joined")
		v.ended(c, msg)
		result <- &Refusal{Msg: msg, Failed: failed}
	}

	routeCtx, cancelRoute := context.WithTimeout(ctx, routeWithin)
	route, err := v.opts.Signaler.Route(routeCtx, account)
	cancelRoute()
	if err != nil {
		fail(true, "%v", err)
		return
	}

	w, err = Start(ctx, Options{Program: v.opts.Program, Version: v.opts.Version, Log: log,
		Args: v.opts.Args, Env: v.opts.Env})
	if err != nil {
		w = nil
		fail(true, "%v", err)
		return
	}
	v.mu.Lock()
	c.worker = w
	v.mu.Unlock()
	// However the call goes, its worker is gone when run returns.
	defer func() {
		select {
		case <-w.Done():
		default:
			w.Leave()
		}
	}()

	join := voiceipc.Join{ICEServers: orEmpty(route.ICEServers), Loopback: route.Loopback, Processing: c.processing}
	if err := w.Send(voiceipc.OpJoin, join); err != nil {
		<-w.Done()
		fail(true, "%s", w.Exit().Reason)
		return
	}
	offer, why := v.await(ctx, w, offerWithin, func(ev Event) (bool, string) {
		if ev.Offer != nil {
			return true, ""
		}
		return false, "the voice worker reported a state before its offer"
	})
	if offer == nil {
		fail(true, "%s", why)
		return
	}

	answerCtx, cancelAnswer := context.WithTimeout(ctx, answerWithin)
	answer, err := v.opts.Signaler.Join(answerCtx, account, c.channel, offer.Offer.SDP)
	cancelAnswer()
	if err != nil {
		fail(true, "%v", err)
		return
	}
	if err := w.Send(voiceipc.OpAnswer, voiceipc.Answer{SDP: answer}); err != nil {
		<-w.Done()
		fail(true, "%s", w.Exit().Reason)
		return
	}
	ready, why := v.await(ctx, w, readyWithin, func(ev Event) (bool, string) {
		switch {
		case ev.Offer != nil:
			return false, "the voice worker offered twice"
		case ev.State.State == voiceipc.StateReady:
			return true, ""
		}
		return false, "" // connecting: keep waiting
	})
	if ready == nil {
		fail(true, "%s", why)
		return
	}

	v.mu.Lock()
	if v.current == c {
		v.status.State = ipc.VoiceInCall
	}
	v.mu.Unlock()
	log.Info().Msg("in a voice call")
	result <- nil
	v.publish()

	// The call is up. What the worker says from here is drained, so it never waits on the daemon, until it
	// ends; its end, crash or not, is the call's.
	for ev := range w.Events() {
		if ev.Offer != nil {
			w.Kill("the voice worker broke the protocol: it offered again in a call")
		}
	}
	<-w.Done()
	v.ended(c, w.Exit().Reason)
}

// await waits for the event want accepts, the worker's end, the bound, or the call's cancellation. A
// non-empty reason from want refuses the event and ends the worker; an ended state is the call's end.
func (v *Voice) await(ctx context.Context, w *Worker, within time.Duration,
	want func(Event) (bool, string)) (*Event, string) {
	timer := time.NewTimer(within)
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-w.Events():
			if !ok {
				<-w.Done()
				return nil, w.Exit().Reason
			}
			if ev.State != nil && ev.State.State == voiceipc.StateEnded {
				<-w.Done()
				return nil, w.Exit().Reason
			}
			done, why := want(ev)
			if why != "" {
				w.Kill("the voice worker broke the protocol: " + why)
				<-w.Done()
				return nil, w.Exit().Reason
			}
			if done {
				return &ev, ""
			}
		case <-timer.C:
			w.Leave()
			return nil, fmt.Sprintf("the voice call did not connect within %s", within)
		case <-ctx.Done():
			w.Leave()
			return nil, "left"
		}
	}
}

// ended makes c's end the status, if c is still the call: a call left or replaced has already said so.
func (v *Voice) ended(c *call, why string) {
	v.mu.Lock()
	if v.current != c {
		v.mu.Unlock()
		return
	}
	v.current = nil
	v.status.State, v.status.Channel, v.status.Reason, v.status.Processing = ipc.VoiceIdle, "", reason(why), nil
	v.mu.Unlock()
	v.publish()
}

// endsWith ends c when its sign-in ends, unless c has finished first.
func (v *Voice) endsWith(c *call, ended <-chan struct{}) {
	select {
	case <-ended:
		v.leave(c, "the sign-in ended")
	case <-c.finished:
	}
}

// Leave ends the call, if there is one, and returns once its worker is reaped.
func (v *Voice) Leave() ipc.VoiceStatus {
	v.mu.Lock()
	c := v.current
	v.mu.Unlock()
	if c == nil {
		return v.Status()
	}
	return v.leave(c, "left")
}

// leave ends c, if it is still the call, giving why as the status's reason.
func (v *Voice) leave(c *call, why string) ipc.VoiceStatus {
	v.mu.Lock()
	if v.current != c {
		v.mu.Unlock()
		return v.Status()
	}
	v.current = nil
	v.status.State, v.status.Channel, v.status.Reason, v.status.Processing = ipc.VoiceIdle, "", why, nil
	w := c.worker
	v.mu.Unlock()
	v.publish()

	c.cancel()
	if w != nil {
		w.Leave()
	}
	<-c.finished
	return v.Status()
}

// Close ends the call and refuses any join after it: the daemon is stopping.
func (v *Voice) Close() {
	v.mu.Lock()
	v.closed = true
	v.mu.Unlock()
	v.Leave()
}

func (v *Voice) publish() {
	if v.opts.Changed == nil {
		return
	}
	v.pubMu.Lock()
	defer v.pubMu.Unlock()
	v.opts.Changed(v.Status())
}

// reason is a reason fit to show and to keep: cleaned, since a worker's and a voice server's words are
// foreign, and cut to what a STATE may carry.
func reason(s string) string {
	return voiceipc.CutReason(termsafe.Text(s))
}

func orEmpty(s []voiceipc.ICEServer) []voiceipc.ICEServer {
	if s == nil {
		return []voiceipc.ICEServer{}
	}
	return s
}
