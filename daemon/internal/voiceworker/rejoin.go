// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceworker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/statefile"
)

// A call lost without anybody leaving it is joined again (M36): the daemon went away mid-call and started
// again, its worker crashed, or the instance no longer holds the session the call was signaled on. Every
// other end is somebody's decision, the person's, the instance's or the worker's, and is not undone: a
// rejoin after a moderator's disconnect would undo the moderation.
//
// A rejoin is a join like any other: op 4, authorized afresh by the instance (rule 1), which may refuse it.
// What it joins with is the call as it was: the channel, the processing and the input, and the person's own
// switches (M34), so a caller who had muted themselves comes back muted.
//
// It is bounded twice. By age: past rejoinWithin since the call was last known up nothing is joined, since a
// rejoin opens the microphone without being asked. And by attempts: each is counted on disk before it is
// made, so a rejoin that takes the daemon down with it stops after ipc.MaxVoiceRejoins starts.

// rejoinBackoff spaces the tries after one that could not be made or did not work for a reason that passes:
// no connection to the instance yet, a worker that would not start, a join that timed out. A variable for the
// tests.
var rejoinBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// rejoin is a call being joined again.
type rejoin struct {
	cause string
	// call is what is joined, with the attempts made so far, under Voice.mu.
	call statefile.VoiceCall
	// restore is the person's switches to be taken from call under the sign-in the rejoin finds, which a
	// daemon that started again does not have in memory; under Voice.mu.
	restore bool
	// online is told the instance is reachable again (Voice.Online), the latest only, and backoff is
	// rejoinBackoff as the rejoin began.
	online  chan struct{}
	backoff []time.Duration
	ctx     context.Context
	cancel  context.CancelFunc
	// from is the call lost, nil for one a previous run left; signedIn closes when the sign-in it was lost
	// under ends, and is the first one read for a previous run's. A rejoin is that sign-in's alone: a
	// sign-out ends it, even with the same account signed in again (M36's /code-review). Under Voice.mu.
	from     *call
	signedIn <-chan struct{}
}

func newRejoin(cause string, call statefile.VoiceCall, restore bool) *rejoin {
	ctx, cancel := context.WithCancel(context.Background())
	return &rejoin{cause: cause, call: call, restore: restore, online: make(chan struct{}, 1),
		backoff: slices.Clone(rejoinBackoff), ctx: ctx, cancel: cancel}
}

// Resume takes up the breadcrumb the daemon's previous run left (M36's D8): it is joined again once the
// sign-in it names is read and the instance is reachable, if it is recent enough. One refused as it was
// read, or too old, is removed.
func (v *Voice) Resume(prev *statefile.VoiceCall) {
	if prev == nil {
		return
	}
	switch {
	case prev.Refused() != nil:
		v.opts.Log.Warn().Str("reason", reason(prev.Refused().Error())).Msg("not joining a voice call again")
	case !fresh(prev.Alive):
		v.opts.Log.Info().Str("channel", prev.Channel).Msg("the voice call this daemon was in was lost too " +
			"long ago to join again")
	case v.opts.Signaler == nil || v.opts.Account == nil:
		// A daemon that cannot join leaves the breadcrumb for one that can.
		return
	default:
		v.mu.Lock()
		if v.closed || v.current != nil || v.pending != nil {
			v.mu.Unlock()
			return
		}
		r := newRejoin(ipc.VoiceRejoinDaemonRestarted, prev.Clone(), true)
		v.waitToRejoin(r, "")
		v.rejoins.Add(1)
		v.mu.Unlock()
		v.publish()
		v.opts.Log.Info().Str("channel", prev.Channel).Msg("joining again the voice call this daemon was in")
		go v.rejoinLoop(r)
		return
	}
	v.mu.Lock()
	v.crumb = nil
	v.mu.Unlock()
	v.save()
}

// Online tells a rejoin waiting for the instance that it is reachable: the gateway connection is live on a
// new session (gatewayclient's Online). Waiting is bounded by itself, so a missed one costs a second.
func (v *Voice) Online() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if r := v.pending; r != nil {
		select {
		case r.online <- struct{}{}:
		default:
		}
	}
}

// waitToRejoin makes r the rejoin, the status saying it waits, why the call was lost as its reason. Under
// Voice.mu.
func (v *Voice) waitToRejoin(r *rejoin, why string) {
	v.pending = r
	crumb := r.call.Clone()
	v.crumb = &crumb
	v.status.State, v.status.Channel, v.status.Reason, v.status.EndedInCall = ipc.VoiceJoining, r.call.Channel,
		reason(why), false
	clearCall(&v.status)
	v.status.Rejoin = &ipc.VoiceRejoin{Cause: r.cause, Attempt: r.call.Attempts}
}

// toRejoin makes c, lost for why, a call to join again, if it was up and is still the call, and returns the
// rejoin and c's worker to end; nil when c is not to be rejoined, which its caller ends as before.
func (v *Voice) toRejoin(c *call, why, cause string) (*rejoin, *Worker) {
	v.mu.Lock()
	if v.current != c || v.status.State != ipc.VoiceInCall || v.closed {
		v.mu.Unlock()
		return nil, nil
	}
	v.current = nil
	r := newRejoin(cause, *v.crumbFor(c), false)
	r.from, r.signedIn = c, c.signedIn
	v.waitToRejoin(r, why)
	// Counted under mu, where Close sets closed, so Close's wait sees every loop that will run.
	v.rejoins.Add(1)
	w := c.worker
	v.mu.Unlock()
	v.opts.Log.Warn().Str("channel", c.channel).Str("reason", reason(why)).Msg("the voice call was lost; " +
		"joining it again")
	v.publish()
	v.keep()
	return r, w
}

// cancelRejoin gives up the rejoin there is, for the person's own join or leave, and removes its breadcrumb.
// It reports whether there was one.
func (v *Voice) cancelRejoin() bool {
	v.mu.Lock()
	r := v.pending
	if r == nil {
		v.mu.Unlock()
		return false
	}
	r.cancel()
	v.pending, v.status.Rejoin, v.crumb = nil, nil, nil
	v.mu.Unlock()
	v.save()
	return true
}

// rejoinLoop tries r until a call is up, the person takes over, or it gives up. Close waits for it.
func (v *Voice) rejoinLoop(r *rejoin) {
	defer v.rejoins.Done()
	var wait time.Duration
	for step := 0; ; step++ {
		if wait > 0 {
			v.mu.Lock()
			signedIn := r.signedIn
			v.mu.Unlock()
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-r.online:
			case <-signedIn:
			case <-r.ctx.Done():
				timer.Stop()
				return
			}
			timer.Stop()
		}
		wait = r.backoff[min(step, len(r.backoff)-1)]
		if r.ctx.Err() != nil {
			return
		}
		v.mu.Lock()
		alive, attempts, account, instance := r.call.Alive, r.call.Attempts, r.call.Account, r.call.Instance
		v.mu.Unlock()
		if !fresh(alive) {
			v.giveUp(r, "the call was lost too long ago to join it again")
			return
		}
		signIn, err := v.opts.Account()
		switch {
		case errors.Is(err, ErrSignInPending):
			continue
		case err != nil:
			v.giveUp(r, "")
			return
		case signIn.Account != account || !ipc.SameInstance(signIn.Instance, instance) || !v.signedInAs(r, signIn):
			// Somebody else is signed in now, or the same account again after a sign-out: the call was
			// that sign-in's, which ended it.
			v.giveUp(r, "")
			return
		}
		prepareCtx, cancelPrepare := context.WithTimeout(r.ctx, routeWithin)
		_, err = v.opts.Signaler.Prepare(prepareCtx, signIn.Account)
		cancelPrepare()
		if err != nil {
			continue
		}
		if attempts >= ipc.MaxVoiceRejoins {
			v.giveUp(r, fmt.Sprintf("the call was lost, and joining it again failed %d times", attempts))
			return
		}
		req, ok := v.attempt(r, signIn)
		if !ok {
			return
		}
		_, err = v.join(r.ctx, req.channel, req.join, r)
		if err == nil {
			return
		}
		var refused *Refusal
		if errors.As(err, &refused) && refused.Final {
			v.giveUp(r, refused.Msg)
			return
		}
	}
}

// rejoinRequest is what one attempt joins.
type rejoinRequest struct {
	channel string
	join    ipc.VoiceJoinRequest
}

// attempt counts an attempt of r, on disk before it is made, and takes up the person's switches from the
// breadcrumb when the daemon started again without them. False when r is no longer the rejoin.
func (v *Voice) attempt(r *rejoin, signIn SignIn) (rejoinRequest, bool) {
	v.mu.Lock()
	if v.pending != r {
		v.mu.Unlock()
		return rejoinRequest{}, false
	}
	r.call.Attempts++
	if r.restore {
		if p := v.prefs.live(); !p.set || p.ended != signIn.Ended {
			v.prefs = prefsFrom(r.call, signIn.Ended)
		}
		r.restore = false
	}
	crumb := r.call.Clone()
	v.crumb = &crumb
	v.status.Rejoin = &ipc.VoiceRejoin{Cause: r.cause, Attempt: r.call.Attempts}
	req := rejoinRequest{channel: r.call.Channel,
		join: ipc.VoiceJoinRequest{VoiceJoin: r.call.Processing, VoiceInput: r.call.Input}}
	v.mu.Unlock()
	v.save()
	v.publish()
	return req, true
}

// giveUp ends r, saying why in the status; with no reason, it leaves no trace of itself, as for a sign-in
// that ended or changed.
func (v *Voice) giveUp(r *rejoin, why string) {
	v.mu.Lock()
	if v.pending != r {
		v.mu.Unlock()
		return
	}
	r.cancel()
	v.pending, v.crumb = nil, nil
	v.status.State, v.status.Channel, v.status.Reason, v.status.EndedInCall = ipc.VoiceIdle, "", reason(why), false
	clearCall(&v.status)
	if why == "" {
		v.status.Rejoin = nil
	}
	v.mu.Unlock()
	if why != "" {
		v.opts.Log.Warn().Str("reason", reason(why)).Msg("not joining the voice call again")
	}
	v.save()
	v.publish()
}

// note makes the breadcrumb the call that is up, stamped now, and has the keeper write it. A call up for
// rejoinWithin has its attempts counted from nothing again.
func (v *Voice) note() {
	v.mu.Lock()
	c := v.current
	if c == nil || v.status.State != ipc.VoiceInCall || v.closed {
		v.mu.Unlock()
		return
	}
	if c.attempts > 0 && wallNow().Sub(c.upAt) >= rejoinWithin {
		c.attempts = 0
	}
	v.crumb = v.crumbFor(c)
	v.mu.Unlock()
	v.keep()
}

// crumbFor is c as a breadcrumb, stamped now: the call, and what the person has set. Under Voice.mu.
func (v *Voice) crumbFor(c *call) *statefile.VoiceCall {
	out := &statefile.VoiceCall{Instance: c.instance, Account: c.account, Channel: c.channel, Alive: wallNow(),
		Attempts: c.attempts, Processing: c.processing, Input: c.input}
	withPrefs(out, v.prefs.live())
	return out
}

// withPrefs writes what the person has set into a breadcrumb.
func withPrefs(out *statefile.VoiceCall, p prefs) {
	out.Self, out.MuteBeforeDeaf = p.self, p.self.Deaf && p.muteBeforeDeaf
	out.LocalMutes = sortedKeys(p.mutes)
	out.MaxStreams = 0
	if p.streams != nil {
		out.MaxStreams = *p.streams
	}
}

// prefsFrom is what a breadcrumb says the person had set, under the sign-in whose end ended closes.
func prefsFrom(c statefile.VoiceCall, ended <-chan struct{}) prefs {
	p := prefs{set: true, ended: ended, self: c.Self, muteBeforeDeaf: c.MuteBeforeDeaf, mutes: map[string]bool{}}
	for _, u := range c.LocalMutes {
		p.mutes[u] = true
	}
	if c.MaxStreams > 0 {
		n := c.MaxStreams
		p.streams = &n
	}
	return p
}

// fresh is a call last known up within rejoinWithin, on the wall clock, either way: a stamp in the future
// is a clock moved, which says nothing of how long ago the call was.
func fresh(alive time.Time) bool {
	d := wallNow().Sub(alive)
	return d <= rejoinWithin && d >= -rejoinWithin
}

// signedInAs is whether signIn is the sign-in r was lost under, taking it as that one for a previous run's
// breadcrumb, which the first sign-in read stands for.
func (v *Voice) signedInAs(r *rejoin, signIn SignIn) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if r.signedIn == nil {
		r.signedIn = signIn.Ended
	}
	return r.signedIn == signIn.Ended
}

// keep has the keeper write the breadcrumb, the latest only. Never waits.
func (v *Voice) keep() {
	if v.keepKick == nil {
		return
	}
	select {
	case v.keepKick <- struct{}{}:
	default:
	}
}

// keeper writes the breadcrumb each time keep asks, from a goroutine of its own: a state file's lock waits
// up to seconds, and nothing that asks may wait on it (M34's /code-review, on a gateway write stalling the
// call's event loop). It writes once more as it stops.
func (v *Voice) keeper() {
	defer close(v.keeperDone)
	for {
		select {
		case <-v.keepKick:
			v.save()
		case <-v.keeperStop:
			v.save()
			return
		}
	}
}

// saveRetry is how long after a write that failed the keeper writes again: a removal left undone would
// join a call the person left at the next start (M36's /code-review). A variable for the tests.
var saveRetry = time.Second

// save writes the breadcrumb as it is now, or removes it. A write that fails is logged once, and the keeper
// writes the latest again every saveRetry until one is done; the call goes on meanwhile.
func (v *Voice) save() {
	if v.opts.Breadcrumbs == nil {
		return
	}
	v.saveMu.Lock()
	defer v.saveMu.Unlock()
	v.mu.Lock()
	var crumb *statefile.VoiceCall
	if v.crumb != nil {
		c := v.crumb.Clone()
		crumb = &c
	}
	v.mu.Unlock()
	if err := v.opts.Breadcrumbs.Save(crumb); err != nil {
		if !v.saveFailing {
			v.opts.Log.Warn().Err(err).Msg("cannot record the voice call to join again after a restart; " +
				"trying again")
		}
		v.saveFailing = true
		time.AfterFunc(saveRetry, v.keep)
		return
	}
	if v.saveFailing {
		v.opts.Log.Info().Msg("recorded the voice call to join again after a restart")
	}
	v.saveFailing = false
}
