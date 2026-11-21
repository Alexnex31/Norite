// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceworker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
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
	// routeWithin bounds the signaler's first step, answerWithin the provision, and signalLeaveWithin telling
	// the voice server the call is over.
	routeWithin       = 15 * time.Second
	answerWithin      = 15 * time.Second
	signalLeaveWithin = 5 * time.Second
	// offerWithin bounds the worker's offer, and readyWithin the time from its answer to ready, which
	// since M33 holds the gathering too.
	offerWithin = 15 * time.Second
	readyWithin = 25 * time.Second
	// micWithin bounds how long a join waits, once the call is up, for the microphone to be past opening
	// (M32): long enough for a device's first frame, or macOS's question to be on the screen, and never the
	// person's answer to it.
	micWithin = 3 * time.Second
	// processingWithin bounds how long a change of processing waits for the worker to say it runs (M34's
	// manual pass): the chain is rebuilt in a moment, and past this the answer says what runs still.
	processingWithin = 2 * time.Second
)

// Voice is the daemon's voice calls: one at a time, since a person is in one call. A join while in a call
// ends that call first.
type Voice struct {
	opts VoiceOptions

	// joinMu serializes joins. Leave never takes it, so a leave can end a join under way.
	joinMu sync.Mutex
	// last is the latest join's call, under joinMu. A join waits for it to finish, since its run tells the
	// voice server the call is over after it has answered, and that leave, reaching the instance after the
	// next join, would end it (M33's /code-review).
	last *call
	// pubMu orders what Changed is told: each publish sends the status as it is then, so concurrent
	// changes converge on the last.
	pubMu sync.Mutex

	mu      sync.Mutex
	status  ipc.VoiceStatus
	current *call
	closed  bool
	// prefs is what the person has set for their calls (M34), under mu.
	prefs prefs
}

// prefs is what the person has set for their calls (M34): on themselves, whom they muted for themselves
// alone, and how many people they hear at once. It belongs to the sign-in it was set under and is forgotten
// once that ends, as everything standing on a sign-in is (M19); it is never written to disk.
type prefs struct {
	// set is anything having been set, under the sign-in whose Ended is ended.
	set   bool
	ended <-chan struct{}
	self  ipc.VoiceSelf
	// muteBeforeDeaf is the mute an undeafen puts back.
	muteBeforeDeaf bool
	mutes          map[string]bool
	streams        *int
}

// live is p, or nothing if its sign-in has ended.
func (p prefs) live() prefs {
	if !p.set {
		return prefs{}
	}
	select {
	case <-p.ended:
		return prefs{}
	default:
		return p
	}
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
	processing ipc.VoiceJoin
	cancel     context.CancelFunc
	// worker is set under Voice.mu once started.
	worker *Worker
	// finished is closed when the call's goroutine has returned and its worker is reaped.
	finished chan struct{}
	// decided is whether the worker has said the call's start is judged and the call goes on, under
	// Voice.mu.
	decided bool

	// What M34 sends the call, all under Voice.mu but sendMu. account is the call's; server is what the
	// instance says the account may do. answered is the worker having its answer, before which it takes no
	// RIGHTS, SUBSCRIBE or PROCESSING. sentSelf is what the voice server was last told the person set on
	// themselves; sentRights, sentSub and sentProcessing what the worker was last told, so that only a
	// change is sent.
	account        string
	server         voiceipc.Rights
	answered       bool
	sentSelf       ipc.VoiceSelf
	sentRights     voiceipc.Rights
	sentSub        voiceipc.Subscribe
	sentProcessing ipc.VoiceJoin
	// dspChanged is closed, and replaced, each time the worker says what its processing does, under
	// Voice.mu: what a change of processing waits on.
	dspChanged chan struct{}
	// sendMu orders what push sends, so that the last push sends the latest state.
	sendMu sync.Mutex
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
	out := v.status
	p := v.prefs.live()
	out.Self, out.LocalMutes, out.MaxStreams = p.self, sortedKeys(p.mutes), p.streams
	out.Speaking = slices.Clone(out.Speaking)
	if out.Speaking == nil {
		out.Speaking = []string{}
	}
	if p.streams != nil {
		n := *p.streams
		out.MaxStreams = &n
	}
	return out
}

// The person's switches on themselves (M34), for SetSelf.
const (
	SelfMute     = "mute"
	SelfUnmute   = "unmute"
	SelfDeafen   = "deafen"
	SelfUndeafen = "undeafen"
)

// change applies what the person set to the prefs of the sign-in they are signed in under now, and sends
// the call what applies; a sign-in other than the one the prefs were set under starts from nothing.
func (v *Voice) change(apply func(p *prefs, account string) error) (ipc.VoiceStatus, error) {
	signIn, err := v.opts.Account()
	switch {
	case errors.Is(err, ErrSignInPending):
		return v.Status(), &Refusal{Msg: "the daemon has not read its sign-in yet; ask again in a moment",
			Pending: true}
	case err != nil:
		return v.Status(), &Refusal{Msg: "nobody is signed in to this daemon; sign in with `norite login`"}
	}
	v.mu.Lock()
	p := v.prefs.live()
	if !p.set || p.ended != signIn.Ended {
		p = prefs{set: true, ended: signIn.Ended}
	}
	p.mutes = maps.Clone(p.mutes)
	if p.mutes == nil {
		p.mutes = map[string]bool{}
	}
	if err := apply(&p, signIn.Account); err != nil {
		v.mu.Unlock()
		return v.Status(), err
	}
	v.prefs = p
	c := v.current
	v.mu.Unlock()
	v.publish()
	if c != nil {
		v.push(c)
	}
	return v.Status(), nil
}

// SetSelf mutes, unmutes, deafens or undeafens the person (M34): in the call there is, and in every one
// after it until the sign-in ends. Deafening mutes too, and undeafening puts back the mute from before;
// unmuting while deafened undeafens as well, since nobody is heard who cannot hear.
func (v *Voice) SetSelf(what string) (ipc.VoiceStatus, error) {
	return v.change(func(p *prefs, _ string) error {
		switch what {
		case SelfMute:
			p.self.Mute = true
			if p.self.Deaf {
				p.muteBeforeDeaf = true
			}
		case SelfUnmute:
			p.self = ipc.VoiceSelf{}
		case SelfDeafen:
			if !p.self.Deaf {
				p.muteBeforeDeaf = p.self.Mute
			}
			p.self = ipc.VoiceSelf{Mute: true, Deaf: true}
		case SelfUndeafen:
			if p.self.Deaf {
				p.self = ipc.VoiceSelf{Mute: p.muteBeforeDeaf}
			}
		default:
			return fmt.Errorf("voiceworker: no switch %q", what)
		}
		return nil
	})
}

// LocalMute mutes user for this account alone, or hears them again (M34): the voice server stops sending
// them to this call, in every call until the sign-in ends. It is never a report, which is a separate act.
func (v *Voice) LocalMute(user string, mute bool) (ipc.VoiceStatus, error) {
	return v.change(func(p *prefs, account string) error {
		switch {
		case !mute:
			delete(p.mutes, user)
		case user == account:
			return &Refusal{Msg: "you cannot mute yourself for yourself; `norite voice mute` mutes you for everybody"}
		case !p.mutes[user] && len(p.mutes) >= ipc.MaxLocalMutes:
			return &Refusal{Msg: fmt.Sprintf("at most %d people can be muted for yourself; unmute somebody first",
				ipc.MaxLocalMutes)}
		default:
			p.mutes[user] = true
		}
		return nil
	})
}

// Streams sets how many people the person hears at once, 1 to ipc.MaxStreams, or every one the call has
// room for with 0 (M34), until the sign-in ends.
func (v *Voice) Streams(n int) (ipc.VoiceStatus, error) {
	return v.change(func(p *prefs, _ string) error {
		if n == 0 {
			p.streams = nil
		} else {
			p.streams = &n
		}
		return nil
	})
}

// Processing changes how the call's microphone is processed (M34's D5). Without a call it changes nothing:
// the next join brings its own, from the client's config.toml, which the client writes.
//
// It answers once the worker says the processing asked for runs, or failed, or after processingWithin, so
// that the answer says what the microphone goes through: answered at once, it described the processing
// before the change, and read as the change not having taken (M34's manual pass).
func (v *Voice) Processing(p ipc.VoiceJoin) ipc.VoiceStatus {
	v.mu.Lock()
	c := v.current
	if c != nil {
		c.processing = p
		asked := p
		v.status.Processing = &asked
	}
	v.mu.Unlock()
	if c == nil {
		return v.Status()
	}
	v.publish()
	v.push(c)
	timeout := time.NewTimer(processingWithin)
	defer timeout.Stop()
	for {
		v.mu.Lock()
		if v.current != c || runs(v.status.DSP, p) {
			v.mu.Unlock()
			return v.Status()
		}
		changed := c.dspChanged
		v.mu.Unlock()
		select {
		case <-changed:
		case <-c.finished:
			return v.Status()
		case <-timeout.C:
			return v.Status()
		}
	}
}

// runs is whether what the worker says of its processing answers a change to p: p running, or no
// processing at all, which no change brings back.
func runs(d *ipc.VoiceDSP, p ipc.VoiceJoin) bool {
	switch {
	case d == nil:
		return false
	case d.State == ipc.VoiceDSPFailed:
		return true
	}
	return d.State == ipc.VoiceDSPRunning && d.EchoCancellation == p.EchoCancellation &&
		d.NoiseSuppression == p.NoiseSuppression && d.AutoGain == p.AutoGain
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
func (v *Voice) Join(ctx context.Context, channel string, processing ipc.VoiceJoin) (ipc.VoiceStatus, error) {
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
	v.leaveCurrent()
	if v.last != nil {
		<-v.last.finished
	}

	callCtx, cancel := context.WithCancel(context.Background())
	c := &call{channel: channel, processing: processing, cancel: cancel, finished: make(chan struct{}),
		account: signIn.Account, sentProcessing: processing, dspChanged: make(chan struct{})}
	v.last = c
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
	asked := processing
	v.status.State, v.status.Channel, v.status.Reason, v.status.Processing = ipc.VoiceJoining, channel, "", &asked
	v.status.EndedInCall = false
	v.status.Microphone, v.status.Speakers, v.status.Rights = nil, nil, nil
	v.status.Speaking, v.status.DSP = nil, nil
	v.mu.Unlock()
	v.publish()

	go v.endsWith(c, signIn.Ended, "the sign-in ended")
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
	//
	// by is the devices that ended the call, when the failure is the worker's end and its devices ended it:
	// said by the caller, never guessed here, since a worker that ended on its devices as the daemon gave up
	// on it leaves the daemon's reason, which they would contradict.
	fail := func(failed bool, by *voiceipc.Devices, format string, args ...any) {
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
			msg, failed, by, ev = "the call was left before it was up", false, nil, log.Info()
		}
		ev.Str("reason", msg).Msg("the voice call could not be joined")
		v.ended(c, msg, by)
		result <- &Refusal{Msg: msg, Failed: failed}
	}

	routeCtx, cancelRoute := context.WithTimeout(ctx, routeWithin)
	route, err := v.opts.Signaler.Prepare(routeCtx, account)
	cancelRoute()
	if err != nil {
		fail(true, nil, "%v", err)
		return
	}

	w, err = Start(ctx, Options{Program: v.opts.Program, Version: v.opts.Version, Log: log,
		Args: v.opts.Args, Env: v.opts.Env})
	if err != nil {
		w = nil
		fail(true, nil, "%v", err)
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

	join := voiceipc.Join{Loopback: route.Loopback, Processing: c.processing}
	if err := w.Send(voiceipc.OpJoin, join); err != nil {
		<-w.Done()
		fail(true, w.Exit().Devices, "%s", w.Exit().Reason)
		return
	}
	offer, why, by := v.await(ctx, w, offerWithin, func(ev Event) (bool, string) {
		if ev.Offer != nil {
			return true, ""
		}
		return false, "the voice worker reported a state or its devices before its offer"
	})
	if offer == nil {
		fail(true, by, "%s", why)
		return
	}

	// From the moment the join may have been sent, however the call ends, the voice server is told, just
	// before the worker is reaped (the deferred reaping above runs after this). A join given up while the
	// instance still works on it, by a leave, the bound or an answer this daemon refuses, is otherwise
	// established there and kept for the session's life (M33's /code-review). Its row and the channel's
	// view of the call go at once rather than when the session ends.
	defer func() {
		leaveCtx, cancelLeave := context.WithTimeout(context.Background(), signalLeaveWithin)
		defer cancelLeave()
		v.opts.Signaler.Leave(leaveCtx, account)
	}()
	v.mu.Lock()
	self := v.prefs.live().self
	c.sentSelf = self
	v.mu.Unlock()
	answerCtx, cancelAnswer := context.WithTimeout(ctx, answerWithin)
	answer, err := v.opts.Signaler.Join(answerCtx, account, c.channel, offer.Offer.SDP, self)
	cancelAnswer()
	if err != nil {
		fail(true, nil, "%v", err)
		return
	}
	v.rights(c, answer.Rights)
	if answer.Lost != nil {
		go v.endsWith(c, answer.Lost, SessionLostReason)
	}
	// The answer carries the instance's rights, and push the person's own on top, straight after: the media
	// starts only once the call is ready, so nothing is sent or played in between.
	if err := w.Send(voiceipc.OpAnswer, voiceipc.Answer{SDP: answer.SDP, ICEServers: orEmpty(answer.ICEServers),
		Bitrate: answer.Bitrate, Rights: answer.Rights}); err != nil {
		<-w.Done()
		fail(true, w.Exit().Devices, "%s", w.Exit().Reason)
		return
	}
	// The worker has its answer: from here what the person changes reaches it, and what they changed while
	// the join was on its way goes now, the self the voice server was joined with included.
	v.mu.Lock()
	c.answered, c.sentRights, c.sentSub = true, answer.Rights, voiceipc.Subscribe{Drop: []string{}}
	v.mu.Unlock()
	v.push(c)
	ready, why, by := v.await(ctx, w, readyWithin, func(ev Event) (bool, string) {
		switch {
		case ev.Offer != nil:
			return false, "the voice worker offered twice"
		case ev.State != nil && ev.State.State == voiceipc.StateReady:
			return true, ""
		case ev.Devices != nil:
			// Said while connecting: macOS's question is asked as the call starts.
			v.devices(c, *ev.Devices)
		default:
			v.control(c, ev)
		}
		return false, "" // connecting: keep waiting
	})
	if ready == nil {
		fail(true, by, "%s", why)
		return
	}
	if why, by, up := v.awaitMicrophone(ctx, c, w); !up {
		fail(true, by, "%s", why)
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
	// ends; its end, crash or not, is the call's. What the instance says the account may do goes to it.
	changes := answer.Changes
	for events := w.Events(); events != nil; {
		select {
		case ev, ok := <-events:
			switch {
			case !ok:
				events = nil
			case ev.Offer != nil:
				w.Kill("the voice worker broke the protocol: it offered again in a call")
			case ev.Devices != nil:
				v.devices(c, *ev.Devices)
			default:
				v.control(c, ev)
			}
		case r := <-changes:
			v.rights(c, r)
			// A worker gone already ends the call by its events; nothing more to do here.
			v.push(c)
		}
	}
	<-w.Done()
	v.ended(c, w.Exit().Reason, w.Exit().Devices)
}

// await waits for the event want accepts, the worker's end, the bound, or the call's cancellation. A
// non-empty reason from want refuses the event and ends the worker; an ended state is the call's end, and
// its devices, when they ended it, come back with its reason. Every other failure is the daemon's, and
// names none.
func (v *Voice) await(ctx context.Context, w *Worker, within time.Duration,
	want func(Event) (bool, string)) (*Event, string, *voiceipc.Devices) {
	timer := time.NewTimer(within)
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-w.Events():
			if !ok || (ev.State != nil && ev.State.State == voiceipc.StateEnded) {
				<-w.Done()
				return nil, w.Exit().Reason, w.Exit().Devices
			}
			done, why := want(ev)
			if why != "" {
				w.Kill("the voice worker broke the protocol: " + why)
				<-w.Done()
				return nil, w.Exit().Reason, nil
			}
			if done {
				return &ev, "", nil
			}
		case <-timer.C:
			w.Leave()
			return nil, fmt.Sprintf("the voice call did not connect within %s", within), nil
		case <-ctx.Done():
			w.Leave()
			return nil, "left", nil
		}
	}
}

// awaitMicrophone waits, at most micWithin, for the call's microphone to be past opening, keeping the
// devices' state as the worker says it (D6): so a join answers with whether the person will be heard, a
// refusal or macOS's question included, rather than "in call" over a microphone nobody can use. A
// microphone that failed is waited past until the worker says the call's start is judged and the call goes
// on (Devices.decided), or ends it: both failing as the call starts end it, and that end is the join's
// failure, its reason and the devices that ended it returned with up false. A worker that says nothing of
// its devices is waited for to the bound, and the call goes on.
func (v *Voice) awaitMicrophone(ctx context.Context, c *call, w *Worker) (string, *voiceipc.Devices, bool) {
	timer := time.NewTimer(micWithin)
	defer timer.Stop()
	for {
		v.mu.Lock()
		mic, goesOn := v.status.Microphone, c.decided
		v.mu.Unlock()
		if decided(mic, goesOn) {
			return "", nil, true
		}
		select {
		case ev, ok := <-w.Events():
			switch {
			case !ok || (ev.State != nil && ev.State.State == voiceipc.StateEnded):
				<-w.Done()
				return w.Exit().Reason, w.Exit().Devices, false
			case ev.Offer != nil:
				w.Kill("the voice worker broke the protocol: it offered again in a call")
				<-w.Done()
				return w.Exit().Reason, nil, false
			case ev.Devices != nil:
				v.devices(c, *ev.Devices)
			default:
				v.control(c, ev)
			}
		case <-timer.C:
			return "", nil, true
		case <-ctx.Done():
			w.Leave()
			return "left", nil, false
		}
	}
}

// decided is whether a join can answer on this microphone: past opening, and, when it failed, with the
// worker having said the call's start is judged and the call goes on. A start judged to end it is said by
// the ended state instead, which is the join's failure.
func decided(mic *ipc.VoiceDevice, goesOn bool) bool {
	if mic == nil || mic.State == ipc.VoiceDeviceOpening {
		return false
	}
	return !voiceipc.Failing(mic.State) || goesOn
}

// devices makes what the worker says of c's devices the status's, if c is still the call. The reasons are
// the worker's words, cleaned and cut as any of its reasons is.
func (v *Voice) devices(c *call, d voiceipc.Devices) {
	v.mu.Lock()
	if v.current != c {
		v.mu.Unlock()
		return
	}
	v.status.Microphone, v.status.Speakers = statusDevices(d)
	c.decided = d.Decided
	v.mu.Unlock()
	v.publish()
}

// rights makes what the account may do in c the status's, if c is still the call.
func (v *Voice) rights(c *call, r voiceipc.Rights) {
	v.mu.Lock()
	if v.current != c {
		v.mu.Unlock()
		return
	}
	v.status.Rights = &ipc.VoiceRights{Send: r.Send, Hear: r.Hear}
	c.server = r
	v.mu.Unlock()
	v.publish()
}

// control keeps what the worker says of who speaks and of its processing (M34), if c is still the call.
// Who speaks is ids the worker checked; the processing's reason is its words, cleaned and cut.
func (v *Voice) control(c *call, ev Event) {
	v.mu.Lock()
	if v.current != c {
		v.mu.Unlock()
		return
	}
	switch {
	case ev.Speaking != nil:
		v.status.Speaking = slices.Clone(ev.Speaking.Users)
	case ev.DSP != nil:
		v.status.DSP = &ipc.VoiceDSP{State: ev.DSP.State, Reason: reason(ev.DSP.Reason),
			EchoCancellation: ev.DSP.EchoCancellation, NoiseSuppression: ev.DSP.NoiseSuppression,
			AutoGain: ev.DSP.AutoGain}
		close(c.dspChanged)
		c.dspChanged = make(chan struct{})
	default:
		v.mu.Unlock()
		return
	}
	v.mu.Unlock()
	v.publish()
}

// effective is what the worker applies: what the instance lets the account do, less what the person set on
// themselves. A deafen mutes too, as people expect of it.
func effective(r voiceipc.Rights, self ipc.VoiceSelf) voiceipc.Rights {
	return voiceipc.Rights{Send: r.Send && !self.Mute && !self.Deaf, Hear: r.Hear && !self.Deaf}
}

// push sends c's worker what applies now, once it has its answer: the rights, less the person's own
// switches, whom they muted and how many they hear, how they want the microphone processed if that
// changed, and the voice server what they set on themselves if that did. sendMu orders pushes and each
// reads the state as it is when it sends, so the last one sends the latest: a change racing the answer,
// or another change, cannot land an older state after a newer.
func (v *Voice) push(c *call) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	v.mu.Lock()
	w, answered := c.worker, c.answered
	p := v.prefs.live()
	rights := effective(c.server, p.self)
	sub := voiceipc.Subscribe{Drop: sortedKeys(p.mutes), Cap: p.streams}
	if !answered || w == nil {
		v.mu.Unlock()
		return
	}
	newRights, newSub := rights != c.sentRights, !sameSubscribe(sub, c.sentSub)
	processing, newProcessing := c.processing, c.processing != c.sentProcessing
	self, newSelf := p.self, p.self != c.sentSelf
	c.sentRights, c.sentSub, c.sentProcessing, c.sentSelf = rights, sub, processing, self
	v.mu.Unlock()
	// A worker gone already ends the call by its events; a send it misses changes nothing.
	if newRights {
		_ = w.Send(voiceipc.OpRights, rights)
	}
	if newSub {
		_ = w.Send(voiceipc.OpSubscribe, sub)
	}
	if newProcessing {
		_ = w.Send(voiceipc.OpProcessing, voiceipc.Processing{Processing: processing})
	}
	if newSelf {
		ctx, cancel := context.WithTimeout(context.Background(), signalLeaveWithin)
		defer cancel()
		v.opts.Signaler.Update(ctx, c.account, c.channel, self)
	}
}

func sameSubscribe(a, b voiceipc.Subscribe) bool {
	return slices.Equal(a.Drop, b.Drop) && (a.Cap == nil) == (b.Cap == nil) && (a.Cap == nil || *a.Cap == *b.Cap)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// statusDevices is d as the status carries it, the worker's reasons cleaned and cut as any of its reasons
// is.
func statusDevices(d voiceipc.Devices) (mic, spk *ipc.VoiceDevice) {
	return &ipc.VoiceDevice{State: d.Microphone.State, Reason: reason(d.Microphone.Reason)},
		&ipc.VoiceDevice{State: d.Speakers.State, Reason: reason(d.Speakers.Reason)}
}

// ended makes c's end the status, if c is still the call: a call left or replaced has already said so.
// The devices stay only when the worker's ended state names them as why, both failing (voiceipc checks it),
// so that a client can word it, and they are the ones it names rather than the last DEVICES read. After
// any other end they would describe devices nothing has open any more, both failing by then included,
// which a session that ended mid-call leaves them.
func (v *Voice) ended(c *call, why string, by *voiceipc.Devices) {
	v.mu.Lock()
	if v.current != c {
		v.mu.Unlock()
		return
	}
	v.current = nil
	v.status.EndedInCall = v.status.State == ipc.VoiceInCall
	v.status.State, v.status.Channel, v.status.Reason, v.status.Processing = ipc.VoiceIdle, "", reason(why), nil
	v.status.Microphone, v.status.Speakers, v.status.Rights = nil, nil, nil
	v.status.Speaking, v.status.DSP = nil, nil
	if by != nil {
		v.status.Microphone, v.status.Speakers = statusDevices(*by)
	}
	v.mu.Unlock()
	v.publish()
}

// endsWith ends c for why when ended closes, its sign-in's end or its session's, unless c has finished
// first.
func (v *Voice) endsWith(c *call, ended <-chan struct{}, why string) {
	select {
	case <-ended:
		v.leave(c, why)
	case <-c.finished:
	}
}

// Leave ends the call, if there is one, and returns once its worker is reaped.
func (v *Voice) Leave() ipc.VoiceStatus {
	v.mu.Lock()
	c := v.current
	if c == nil {
		// The devices a failed join kept are an earlier call's, and a leave clears them as it would a call's
		// (the contract). The reason stays: it is why the last join failed, which a leave changes nothing of.
		kept := v.status.Microphone != nil || v.status.Speakers != nil
		v.status.Microphone, v.status.Speakers = nil, nil
		v.mu.Unlock()
		if kept {
			v.publish()
		}
		return v.Status()
	}
	v.mu.Unlock()
	return v.leave(c, "left")
}

// leaveCurrent ends the call there is, if any, and leaves the status alone otherwise: a join, which sets
// the status itself, publishing a leave of nothing first would be a status nobody's call had.
func (v *Voice) leaveCurrent() {
	v.mu.Lock()
	c := v.current
	v.mu.Unlock()
	if c != nil {
		v.leave(c, "left")
	}
}

// leave ends c, if it is still the call, giving why as the status's reason.
func (v *Voice) leave(c *call, why string) ipc.VoiceStatus {
	v.mu.Lock()
	if v.current != c {
		v.mu.Unlock()
		return v.Status()
	}
	v.current = nil
	v.status.EndedInCall = v.status.State == ipc.VoiceInCall
	v.status.State, v.status.Channel, v.status.Reason, v.status.Processing = ipc.VoiceIdle, "", why, nil
	v.status.Microphone, v.status.Speakers, v.status.Rights = nil, nil, nil
	v.status.Speaking, v.status.DSP = nil, nil
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
