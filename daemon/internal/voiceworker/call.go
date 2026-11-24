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

	"github.com/Alexnex31/Norite/daemon/internal/hotkey"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/statefile"
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
	// Hotkey registers push-to-talk's key while a call by push-to-talk lasts (M35); nil has none to offer.
	Hotkey hotkey.Binder
	// Breadcrumbs keeps the call to join again on disk (M36); nil keeps it in memory alone, which rejoins
	// a call whose worker crashed or whose session was lost, and nothing after a restart.
	Breadcrumbs Breadcrumbs

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
	// keyWithin bounds how long a change of mode waits to hear what became of the key (M35's Mac pass): an OS
	// answers in a moment, and past this the answer says it is still being registered.
	keyWithin = 2 * time.Second
	// selfRetryEvery spaces the sending again of an update the voice server did not take (M34's
	// /code-review): a gateway connection resuming is back within seconds.
	selfRetryEvery = time.Second
	// rejoinWithin bounds how long after a call was last known up it is joined again (M36's D2): a daemon
	// restarted by its service manager is back in seconds, and a machine that lost power mid-call and boots
	// hours later, or a laptop opened the next morning, must not open the microphone in a call nobody asked
	// for. A rejoined call that stays up this long has its attempts counted from nothing again. aliveEvery
	// spaces the stamp's refreshing while a call is up.
	rejoinWithin = time.Minute
	aliveEvery   = 15 * time.Second
	// wallNow is the wall clock the breadcrumb is stamped on: Linux's monotonic clock stops in a suspend, and
	// the bound is about the time that passed. A variable for the tests.
	wallNow = time.Now
)

// Breadcrumbs writes the call to join again (statefile.VoiceCall), or removes it with nil.
type Breadcrumbs interface {
	Save(*statefile.VoiceCall) error
}

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
	// started is a test's, run once a call's worker is started and before its join is sent; nil otherwise.
	started func()

	// pending is the call being joined again (M36), from the moment it was lost to the attempt that is up or
	// the last one, under mu.
	pending *rejoin
	// crumb is the breadcrumb as it should be on disk, under mu; save writes it, saveMu ordering the writes.
	// keepKick tells the keeper to write it, the latest only, and keeperStop ends the keeper, which closes
	// keeperDone once the last is written.
	crumb      *statefile.VoiceCall
	saveMu     sync.Mutex
	keepKick   chan struct{}
	keeperStop chan struct{}
	keeperDone chan struct{}
	stopKeeper sync.Once
	// rejoins counts the rejoin loops running, which Close waits for; added to under mu, before closed is
	// set.
	rejoins sync.WaitGroup
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
	// Instance is the instance signed in to, which a breadcrumb names (M36).
	Instance string
}

// call is one call's worker and the goroutine that owns it.
type call struct {
	channel    string
	instance   string
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
	// selfUnsent is the voice server having refused the last update, or the session having resumed since,
	// which push sends again however sentSelf compares, and retry the timer that pushes it again, both
	// under Voice.mu.
	selfUnsent bool
	retry      *time.Timer
	// selfKick tells sendSelf there are flags to send, the latest only.
	selfKick chan struct{}
	// dspChanged is closed, and replaced, each time the worker says what its processing does, under
	// Voice.mu: what a change of processing waits on.
	dspChanged chan struct{}
	// sendMu orders what push sends, so that the last push sends the latest state.
	sendMu sync.Mutex

	// How the microphone is let through (M35), under Voice.mu: input as asked, held whether the key is down
	// and presses how many times it went down, and sentInput and sentPresses what the worker was last told.
	input       ipc.VoiceInput
	held        bool
	presses     uint64
	sentInput   voiceipc.Input
	sentPresses uint64
	// The key's binding: boundKey is the key bound, "" for none, unbind what releases it, and bindGen the
	// binding's number, which a report from a binding since released does not carry. All three under
	// Voice.mu, and changed only by rebind, which bindMu orders.
	bindMu   sync.Mutex
	boundKey string
	unbind   func()
	bindGen  int
	// bindAgain is a key asked for again after it could not be had, under Voice.mu: rebind binds it anew
	// though it is the key bound.
	bindAgain bool
	// keyChanged is closed, and replaced, each time a binding says what became of the key, under Voice.mu:
	// what a change of mode waits on.
	keyChanged chan struct{}

	// rejoin is the rejoin this call is an attempt of, nil for a join the person asked for, and attempts
	// the rejoins its breadcrumb counts; upAt is when it came up. All under Voice.mu (M36).
	rejoin   *rejoin
	attempts int
	upAt     time.Time
}

// NewVoice builds the daemon's voice calls. Nothing runs until a join.
func NewVoice(opts VoiceOptions) *Voice {
	v := &Voice{opts: opts}
	v.status = ipc.VoiceStatus{State: ipc.VoiceIdle, Available: opts.Signaler != nil, Unavailable: opts.Unavailable}
	if opts.Signaler != nil {
		v.status.Unavailable = ""
	}
	if opts.Breadcrumbs != nil {
		v.keepKick, v.keeperStop, v.keeperDone = make(chan struct{}, 1), make(chan struct{}), make(chan struct{})
		go v.keeper()
	}
	return v
}

// Status is the calls' state now.
func (v *Voice) Status() ipc.VoiceStatus {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := v.status
	p := v.prefs.live()
	out.Self, out.LocalMutes = p.self, sortedKeys(p.mutes)
	out.Speaking = slices.Clone(out.Speaking)
	if out.Speaking == nil {
		out.Speaking = []string{}
	}
	// The input's state is changed in place, under mu, as the key reports (keyEvents, rebind): what is
	// handed out is a copy, which the answer is encoded from outside the lock (a race -race found at M35).
	if out.Input != nil {
		in := *out.Input
		out.Input = &in
	}
	if p.streams != nil {
		n := *p.streams
		out.MaxStreams = &n
	}
	if out.Rejoin != nil {
		r := *out.Rejoin
		out.Rejoin = &r
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
	// A daemon whose credential store did not open has no sign-in to keep anything under (noVoice), and
	// asking it for one panicked the whole daemon (M34's /code-review).
	if v.opts.Account == nil {
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
	// What a rejoin waiting will join with is the person's now, on disk too, and nothing is taken back from
	// the breadcrumb over it (M36).
	if r := v.pending; r != nil {
		withPrefs(&r.call, p)
		r.restore = false
		crumb := r.call.Clone()
		v.crumb = &crumb
	}
	v.mu.Unlock()
	v.publish()
	if c != nil {
		v.push(c)
	}
	v.note()
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
// before the change, and read as the change not having taken (M34's manual pass). It answers at once where
// no report can come (unanswerable).
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
	v.note()
	timeout := time.NewTimer(processingWithin)
	defer timeout.Stop()
	for {
		v.mu.Lock()
		if v.current != c || runs(v.status.DSP, p) || unanswerable(v.status) {
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

// unanswerable is a call whose worker cannot say a change of processing took: one that never said what it
// processes, a worker with no device, or whose microphone delivers nothing, since a change is taken up frame
// by frame. Waiting processingWithin for it said only what was asked, two seconds late (M34's second
// /code-review).
func unanswerable(st ipc.VoiceStatus) bool {
	return st.State == ipc.VoiceInCall &&
		(st.DSP == nil || st.Microphone == nil || st.Microphone.State != ipc.VoiceDeviceOpen)
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

// Input changes how the call's microphone is let through (M35): by voice, or by push-to-talk with key.
// Without a call it changes nothing: the next join brings its own, from the client's config.toml, which the
// client writes.
//
// It answers once the key's binding says what became of it, or after keyWithin, so that the answer describes
// the key: answered at once, a key the OS refused the same moment read as one being registered, with exit 0,
// and only a later status said otherwise (M35's Mac pass).
func (v *Voice) Input(in ipc.VoiceInput) ipc.VoiceStatus {
	v.mu.Lock()
	c := v.current
	if c != nil {
		c.input = in
		next := &ipc.VoiceInputStatus{Mode: in.Mode, Key: in.Key}
		// The same key, already bound, keeps what became of it, which rebind then leaves alone; unless it
		// could not be had, when asking again is how somebody who freed it tries again (M35's manual pass:
		// another program held it at the join, let it go, and push-to-talk asked again changed nothing).
		if was := v.status.Input; in.Mode == ipc.VoiceInputPushToTalk && was != nil && c.boundKey == in.Key {
			if was.Hotkey == ipc.VoiceHotkeyUnavailable {
				c.bindAgain = true
			} else {
				next.Hotkey, next.Reason, next.Trigger, next.Held = was.Hotkey, was.Reason, was.Trigger, was.Held
			}
		}
		v.status.Input = next
	}
	v.mu.Unlock()
	if c == nil {
		return v.Status()
	}
	v.publish()
	v.rebind(c)
	v.push(c)
	v.note()
	timeout := time.NewTimer(keyWithin)
	defer timeout.Stop()
	for {
		v.mu.Lock()
		if st := v.status.Input; v.current != c || st == nil || st.Hotkey != ipc.VoiceHotkeyRegistering {
			v.mu.Unlock()
			return v.Status()
		}
		changed := c.keyChanged
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

// rebind makes c's key binding what c asks for now: its key while it is the call and by push-to-talk,
// none otherwise. A key that goes takes its being down with it. The binder is never asked under Voice.mu,
// since a binding's reports take it.
func (v *Voice) rebind(c *call) {
	c.bindMu.Lock()
	defer c.bindMu.Unlock()
	v.mu.Lock()
	want := ""
	if v.current == c && c.input.Mode == ipc.VoiceInputPushToTalk {
		want = c.input.Key
	}
	again := c.bindAgain && want != ""
	c.bindAgain = false
	if want == c.boundKey && !again {
		v.mu.Unlock()
		return
	}
	c.bindGen++
	gen, stop := c.bindGen, c.unbind
	c.unbind, c.boundKey, c.held = nil, "", false
	if st := v.status.Input; v.current == c && st != nil {
		st.Held = false
		st.Hotkey, st.Reason, st.Trigger = "", "", ""
		if want != "" {
			st.Hotkey = ipc.VoiceHotkeyRegistering
		}
	}
	v.mu.Unlock()
	if stop != nil {
		stop()
	}
	var next func()
	if want != "" {
		binder := v.opts.Hotkey
		if binder == nil {
			binder = hotkey.Unavailable("this daemon has no push-to-talk key to offer")
		}
		next = binder.Bind(want, v.keyEvents(c, gen))
	}
	v.mu.Lock()
	c.unbind, c.boundKey = next, want
	v.mu.Unlock()
	v.publish()
	v.push(c)
}

// keyEvents is what binding gen of c's key reports to: what became of the key, and the key going down and
// up, kept in the status and sent to the worker while gen is still c's binding and c the call.
func (v *Voice) keyEvents(c *call, gen int) hotkey.Events {
	current := func() bool { return v.current == c && c.bindGen == gen && v.status.Input != nil }
	return hotkey.Events{
		Status: func(st hotkey.Status) {
			v.mu.Lock()
			if !current() {
				v.mu.Unlock()
				return
			}
			in := v.status.Input
			// The OS's words, and on the portal the desktop's, are a stranger's (rule 19).
			in.Hotkey, in.Reason, in.Trigger = st.State, reason(st.Reason), reason(st.Trigger)
			close(c.keyChanged)
			c.keyChanged = make(chan struct{})
			v.mu.Unlock()
			v.publish()
		},
		Held: func(down bool) {
			v.mu.Lock()
			if !current() || c.held == down {
				v.mu.Unlock()
				return
			}
			if down {
				c.presses++
			}
			c.held = down
			v.status.Input.Held = down
			v.mu.Unlock()
			v.push(c)
			v.publish()
		},
	}
}

// Busy is a call joined or joining, or one to be joined again (M36): no moment for the updater to restart
// the daemon (M24).
func (v *Voice) Busy() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.current != nil || v.pending != nil
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
	// Final is a join the instance refused for good (InstanceRefusal.Final): a rejoin is not tried again.
	Final bool
}

func (r *Refusal) Error() string { return r.Msg }

// Join joins channel, ending any call first, and returns once the call is up or has failed. The worker
// processes the microphone as processing says, and lets it through by voice.
func (v *Voice) Join(ctx context.Context, channel string, processing ipc.VoiceJoin) (ipc.VoiceStatus, error) {
	return v.JoinWith(ctx, channel, ipc.VoiceJoinRequest{VoiceJoin: processing, VoiceInput: ipc.DefaultVoiceInput()})
}

// JoinWith is Join with the whole of a join's body: the processing, and how the microphone is let through,
// by voice or by push-to-talk with its key (M35). A call being joined again is given up for it (M36): the
// person's join wins.
func (v *Voice) JoinWith(ctx context.Context, channel string, req ipc.VoiceJoinRequest) (ipc.VoiceStatus, error) {
	v.cancelRejoin()
	return v.join(ctx, channel, req, nil)
}

// join is a join, the person's with rj nil, or an attempt of rejoin rj.
func (v *Voice) join(ctx context.Context, channel string, req ipc.VoiceJoinRequest, rj *rejoin) (ipc.VoiceStatus, error) {
	processing := req.VoiceJoin
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
	c := &call{channel: channel, instance: signIn.Instance, processing: processing, cancel: cancel,
		finished: make(chan struct{}), account: signIn.Account, sentProcessing: processing,
		dspChanged: make(chan struct{}), keyChanged: make(chan struct{}), input: req.VoiceInput,
		selfKick: make(chan struct{}, 1)}
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		cancel()
		return v.Status(), &Refusal{Msg: "the daemon is stopping"}
	}
	// A rejoin the person took over while it waited its turn is not joined.
	if rj != nil && v.pending != rj {
		v.mu.Unlock()
		cancel()
		return v.Status(), &Refusal{Msg: "the rejoin was given up before its turn came"}
	}
	if rj != nil {
		c.rejoin, c.attempts = rj, rj.call.Attempts
	}
	v.last = c
	result := make(chan error, 1)
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
	v.status.Input = &ipc.VoiceInputStatus{Mode: req.Mode, Key: req.Key}
	if rj == nil {
		v.status.Rejoin = nil
	}
	v.mu.Unlock()
	v.publish()

	go v.endsWith(c, signIn.Ended, "the sign-in ended", "")
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
	// The key goes before the call is said to be over, which is when a leave returns: another program may
	// have it back the moment the person is out of the call. The call is no longer the current one by then,
	// so rebind releases whatever it had.
	defer v.rebind(c)
	defer c.cancel()
	// Registered as the call starts, a session existing (ADR 0034 §4), and never waited for.
	v.rebind(c)
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
	// final is the instance having refused the join for good, which fail says to a rejoin (M36).
	final := false
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
		result <- &Refusal{Msg: msg, Failed: failed, Final: final}
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
	// The processing is read under the lock Processing writes it under, since a change can come while the
	// join is under way (M34's /code-review); what the join carries is what the worker was told.
	v.mu.Lock()
	c.worker = w
	processing := c.processing
	c.sentProcessing = processing
	// How the microphone is let through as the worker starts: by voice, or by push-to-talk with the key up,
	// whatever it is now, since nothing reaches the worker before its answer, and push sends it then.
	input := voiceipc.Input{Mode: c.input.Mode}
	c.sentInput, c.sentPresses = input, c.presses
	v.mu.Unlock()
	if v.started != nil {
		v.started()
	}
	// However the call goes, its worker is gone when run returns.
	defer func() {
		select {
		case <-w.Done():
		default:
			w.Leave()
		}
	}()

	join := voiceipc.Join{Loopback: route.Loopback, Processing: processing, Input: input}
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
		var refused *InstanceRefusal
		final = errors.As(err, &refused) && refused.Final()
		fail(true, nil, "%v", err)
		return
	}
	v.rights(c, answer.Rights)
	if answer.Lost != nil {
		go v.endsWith(c, answer.Lost, SessionLostReason, ipc.VoiceRejoinSessionLost)
	}
	// The answer carries the instance's rights, and push the person's own on top, straight after: the media
	// starts only once the call is ready, so nothing is sent or played in between.
	if err := w.Send(voiceipc.OpAnswer, voiceipc.Answer{SDP: answer.SDP, ICEServers: orEmpty(answer.ICEServers),
		Bitrate: answer.Bitrate, Rights: answer.Rights}); err != nil {
		<-w.Done()
		fail(true, w.Exit().Devices, "%s", w.Exit().Reason)
		return
	}
	// The person's flags go to the voice server from a goroutine of their own, which ends before the leave
	// is sent (the deferred leave above runs after this), so no update follows it.
	selfCtx, stopSelf := context.WithCancel(ctx)
	selfDone := make(chan struct{})
	go func() {
		defer close(selfDone)
		v.sendSelf(selfCtx, c)
	}()
	defer func() {
		stopSelf()
		<-selfDone
	}()
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
		c.upAt = wallNow()
		// The rejoin this was an attempt of is done; the status keeps saying it rejoined.
		if c.rejoin != nil && v.pending == c.rejoin {
			v.pending = nil
		}
	}
	v.mu.Unlock()
	log.Info().Msg("in a voice call")
	result <- nil
	v.publish()
	// The call is up: from here it is the one to join again if it is lost without anybody leaving it.
	v.note()
	alive := time.NewTicker(aliveEvery)
	defer alive.Stop()

	// The call is up. What the worker says from here is drained, so it never waits on the daemon, until it
	// ends; its end, crash or not, is the call's. What the instance says the account may do goes to it.
	changes, resumed := answer.Changes, answer.Resumed
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
		case <-resumed:
			// What the lost connection took of the person's flags may not have arrived: sent again.
			v.mu.Lock()
			c.selfUnsent = true
			v.mu.Unlock()
			v.push(c)
		case <-alive.C:
			v.note()
		}
	}
	<-w.Done()
	// A worker that crashed, rather than ended the call saying why, takes nobody's decision with it: the call
	// is joined again (M36), once this one's leave has gone, which the join waits for (Voice.last).
	if w.Exit().Crashed {
		if r, _ := v.toRejoin(c, w.Exit().Reason, ipc.VoiceRejoinWorkerCrashed); r != nil {
			go v.rejoinLoop(r)
			return
		}
	}
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
	self, newSelf := p.self, p.self != c.sentSelf || c.selfUnsent
	input := voiceipc.Input{Mode: c.input.Mode, Held: c.input.Mode == ipc.VoiceInputPushToTalk && c.held}
	newInput, pressed := input != c.sentInput || c.presses != c.sentPresses, c.presses != c.sentPresses
	wasHeld := c.sentInput.Held
	c.sentRights, c.sentSub, c.sentProcessing, c.sentSelf = rights, sub, processing, self
	// Asked for below, once: a send that fails marks it again (selfSent).
	c.selfUnsent = false
	c.sentInput, c.sentPresses = input, c.presses
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
	if newInput {
		// A press already released by the time this push read the key still went down, and the worker,
		// which counts each time the key goes down, is told it did: one let go at once is a word cut to
		// nothing otherwise, and M25 saw the portal deliver a press and its release at one instant.
		if pressed && !input.Held && input.Mode == voiceipc.InputPushToTalk {
			if wasHeld {
				_ = w.Send(voiceipc.OpInput, voiceipc.Input{Mode: input.Mode})
			}
			_ = w.Send(voiceipc.OpInput, voiceipc.Input{Mode: input.Mode, Held: true})
		}
		_ = w.Send(voiceipc.OpInput, input)
	}
	// The voice server is told by sendSelf, never here: push runs on the call's event loop and on the key's
	// thread, and a gateway write takes up to signalLeaveWithin on a connection going bad, which stalled
	// both, the worker's events past their buffer included (M34's /code-review).
	if newSelf {
		select {
		case c.selfKick <- struct{}{}:
		default:
		}
	}
}

// sendSelf sends the voice server the person's flags, the latest each time push says they changed, until
// ctx ends.
func (v *Voice) sendSelf(ctx context.Context, c *call) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.selfKick:
		}
		v.mu.Lock()
		self := v.prefs.live().self
		c.sentSelf = self
		v.mu.Unlock()
		sendCtx, cancel := context.WithTimeout(ctx, signalLeaveWithin)
		err := v.opts.Signaler.Update(sendCtx, c.account, c.channel, self)
		cancel()
		v.selfSent(c, err == nil)
	}
}

// selfSent notes whether the voice server took c's update, and when it did not, as while the gateway
// connection is down and about to resume, pushes again in selfRetryEvery for as long as c is the call.
// Recorded as sent before it went, a mute made in that gap stayed unseen by everybody else for the rest of
// the call (M34's /code-review). The timer is set under the lock its callback takes (M27).
func (v *Voice) selfSent(c *call, ok bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	c.selfUnsent = !ok
	if ok || c.retry != nil || v.current != c {
		return
	}
	c.retry = time.AfterFunc(selfRetryEvery, func() {
		v.mu.Lock()
		c.retry = nil
		current := v.current == c
		v.mu.Unlock()
		if current {
			v.push(c)
		}
	})
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
	// An attempt of a rejoin that has more to make leaves it waiting, saying why this one failed (M36).
	if c.rejoin != nil && v.pending == c.rejoin {
		v.status.State, v.status.Reason, v.status.EndedInCall = ipc.VoiceJoining, reason(why), false
		v.status.Microphone, v.status.Speakers, v.status.Rights = nil, nil, nil
		v.status.Speaking, v.status.DSP, v.status.Input = nil, nil, nil
		v.mu.Unlock()
		v.publish()
		return
	}
	// Any other end is one to stay out of: what the instance, the worker or the person decided. The call was
	// not lost, and the status says nothing more of any rejoin it came from.
	v.crumb, v.status.Rejoin = nil, nil
	v.status.EndedInCall = v.status.State == ipc.VoiceInCall
	v.status.State, v.status.Channel, v.status.Reason, v.status.Processing = ipc.VoiceIdle, "", reason(why), nil
	v.status.Microphone, v.status.Speakers, v.status.Rights = nil, nil, nil
	v.status.Speaking, v.status.DSP, v.status.Input = nil, nil, nil
	if by != nil {
		v.status.Microphone, v.status.Speakers = statusDevices(*by)
	}
	v.mu.Unlock()
	v.save()
	v.publish()
}

// endsWith ends c for why when ended closes, its sign-in's end or its session's, unless c has finished
// first. With a cause, a call that was up is joined again (M36).
func (v *Voice) endsWith(c *call, ended <-chan struct{}, why, cause string) {
	select {
	case <-ended:
	case <-c.finished:
		return
	}
	if cause != "" {
		if r, w := v.toRejoin(c, why, cause); r != nil {
			c.cancel()
			if w != nil {
				w.Leave()
			}
			<-c.finished
			go v.rejoinLoop(r)
			return
		}
	}
	v.leave(c, why)
}

// Leave ends the call, if there is one, and returns once its worker is reaped. A call being joined again is
// given up (M36), and nothing is left on disk to join after a restart.
func (v *Voice) Leave() ipc.VoiceStatus {
	if v.cancelRejoin() {
		v.mu.Lock()
		if v.current == nil {
			v.status.State, v.status.Channel, v.status.Reason, v.status.EndedInCall = ipc.VoiceIdle, "", "left", false
			v.mu.Unlock()
			v.publish()
			return v.Status()
		}
		v.mu.Unlock()
	}
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
	v.status.Speaking, v.status.DSP, v.status.Input = nil, nil, nil
	w := c.worker
	// A call left is not joined again, unless the daemon is what is stopping (M36's D6): that keeps the
	// breadcrumb, for the start that follows a restart.
	closing := v.closed
	if !closing {
		v.crumb, v.status.Rejoin = nil, nil
	}
	v.mu.Unlock()
	if !closing {
		v.save()
	}
	v.publish()

	c.cancel()
	if w != nil {
		w.Leave()
	}
	<-c.finished
	return v.Status()
}

// Close ends the call and refuses any join after it: the daemon is stopping. The call to join again stays on
// disk (M36), and the keeper has written it before Close returns.
func (v *Voice) Close() {
	v.mu.Lock()
	v.closed = true
	if r := v.pending; r != nil {
		r.cancel()
		v.pending = nil
	}
	c := v.current
	v.mu.Unlock()
	if c != nil {
		v.leave(c, "left")
	}
	v.rejoins.Wait()
	if v.keeperStop != nil {
		v.stopKeeper.Do(func() { close(v.keeperStop) })
		<-v.keeperDone
	}
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
