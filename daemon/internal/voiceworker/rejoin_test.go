// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceworker

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/statefile"
	"github.com/Alexnex31/Norite/daemon/voiceipc"
)

// crumbs keeps every breadcrumb written, nil for one removed.
type crumbs struct {
	mu    sync.Mutex
	saved []*statefile.VoiceCall
	// saving is told each write as it is made, when set.
	saving func(*statefile.VoiceCall)
}

func (c *crumbs) Save(v *statefile.VoiceCall) error {
	if v != nil {
		cp := v.Clone()
		v = &cp
	}
	c.mu.Lock()
	c.saved = append(c.saved, v)
	saving := c.saving
	c.mu.Unlock()
	if saving != nil {
		saving(v)
	}
	return nil
}

// last is the breadcrumb on disk now, and whether anything was written.
func (c *crumbs) last() (*statefile.VoiceCall, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.saved) == 0 {
		return nil, false
	}
	return c.saved[len(c.saved)-1], true
}

// rejoinServer answers every join, recording the channel and the person's switches, while it is connected;
// each answer's Lost is the session's, closed by lose. refuse, when set, is asked for each join by its number,
// from 1, under the server's lock with that join's Lost made, and refuses it with what it returns.
type rejoinServer struct {
	mu           sync.Mutex
	disconnected bool
	joins        []joined
	lost         chan struct{}
	refuse       func(n int) error
}

type joined struct {
	channel string
	self    ipc.VoiceSelf
}

func (s *rejoinServer) Prepare(context.Context, string) (Route, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disconnected {
		return Route{}, errors.New("the daemon is not connected to its instance")
	}
	return Route{}, nil
}

func (s *rejoinServer) Join(_ context.Context, _, channel, _ string, self ipc.VoiceSelf) (Answer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.joins = append(s.joins, joined{channel, self})
	s.lost = make(chan struct{})
	if s.refuse != nil {
		if err := s.refuse(len(s.joins)); err != nil {
			return Answer{}, err
		}
	}
	return Answer{SDP: "v=0\r\nanswer\r\n", Rights: voiceipc.Rights{Send: true, Hear: true}, Lost: s.lost,
		ICEServers: []voiceipc.ICEServer{{URLs: []string{"turn:turn.example:3478"}, Username: "1764000000:42",
			Credential: "the-turn-credential"}}}, nil
}

func (*rejoinServer) Update(context.Context, string, string, ipc.VoiceSelf) error { return nil }
func (*rejoinServer) Leave(context.Context, string)                               {}

func (s *rejoinServer) seen() []joined {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]joined(nil), s.joins...)
}

func (s *rejoinServer) setConnected(up bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.disconnected = !up
}

// lose ends the session the last call was signaled on, as a fresh READY does (Gateway.SessionLost).
func (s *rejoinServer) lose() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.lost)
}

// rejoinFast makes a rejoin's waits short.
func rejoinFast(t *testing.T) {
	t.Helper()
	fast(t)
	b, a, m := rejoinBackoff, aliveEvery, micWithin
	rejoinBackoff, aliveEvery, micWithin = []time.Duration{20 * time.Millisecond}, time.Hour, 50*time.Millisecond
	t.Cleanup(func() { rejoinBackoff, aliveEvery, micWithin = b, a, m })
}

const testInstance = "https://chat.example"

// rejoinVoice is a Voice over the stand-in playing mode, signed in as 42 on testInstance, keeping its
// breadcrumbs in kept.
func rejoinVoice(t *testing.T, mode string, srv Signaler, kept *crumbs, env ...string) *Voice {
	t.Helper()
	o := standinOptions(t, mode, nil)
	ended := make(chan struct{})
	v := NewVoice(VoiceOptions{Program: o.Program, Version: o.Version, Signaler: srv,
		Env: append(o.Env, env...), Breadcrumbs: kept, Log: zerolog.Nop(),
		Account: func() (SignIn, error) {
			return SignIn{Account: "42", Ended: ended, Instance: testInstance}, nil
		}})
	t.Cleanup(v.Close)
	return v
}

func waitState(t *testing.T, v *Voice, ok func(ipc.VoiceStatus) bool, what string) ipc.VoiceStatus {
	t.Helper()
	var st ipc.VoiceStatus
	require.Eventually(t, func() bool {
		st = v.Status()
		return ok(st)
	}, 15*time.Second, 5*time.Millisecond, what)
	return st
}

func rejoinedAs(cause string, attempt int) func(ipc.VoiceStatus) bool {
	return func(st ipc.VoiceStatus) bool {
		return st.State == ipc.VoiceInCall && st.Rejoin != nil && *st.Rejoin == ipc.VoiceRejoin{Cause: cause, Attempt: attempt}
	}
}

// A worker that crashes takes nobody's decision with it: the call is joined again at once, with what the
// person had set (M36's D1), and the status says so until the person next joins or leaves.
func TestACrashedWorkersCallIsJoinedAgain(t *testing.T) {
	rejoinFast(t)
	srv, kept := &rejoinServer{}, &crumbs{}
	v := rejoinVoice(t, "crashonce", srv, kept, standinMarkVariable+"="+filepath.Join(t.TempDir(), "mark"))
	_, err := v.SetSelf(SelfMute)
	require.NoError(t, err)
	_, err = v.LocalMute("7", true)
	require.NoError(t, err)
	st, err := v.Join(context.Background(), "1001", ipc.VoiceAllOn())
	require.NoError(t, err)
	require.Nil(t, st.Rejoin, "a call the person joined")

	st = waitState(t, v, rejoinedAs(ipc.VoiceRejoinWorkerCrashed, 1), "the call was not joined again")
	require.Equal(t, "1001", st.Channel)
	require.Equal(t, ipc.VoiceSelf{Mute: true}, st.Self)
	require.Equal(t, []joined{{"1001", ipc.VoiceSelf{Mute: true}}, {"1001", ipc.VoiceSelf{Mute: true}}}, srv.seen())
	require.True(t, v.Busy())

	// The person's next leave ends it, says nothing more of the rejoin, and leaves nothing to join after a
	// restart.
	st = v.Leave()
	require.Equal(t, ipc.VoiceIdle, st.State)
	require.Nil(t, st.Rejoin)
	last, _ := kept.last()
	require.Nil(t, last)
}

// A session the instance no longer holds ends its call there. The call waits for the instance to be
// reachable on the new session, starting no worker before, and is joined again once it is (M36's D8).
func TestALostSessionsCallIsJoinedAgainOnceTheInstanceIsBack(t *testing.T) {
	rejoinFast(t)
	rejoinBackoff = []time.Duration{time.Hour}
	srv := &rejoinServer{}
	v := rejoinVoice(t, "good", srv, &crumbs{})
	_, err := v.Join(context.Background(), "1001", ipc.VoiceAllOn())
	require.NoError(t, err)

	srv.setConnected(false)
	srv.lose()
	st := waitState(t, v, func(st ipc.VoiceStatus) bool {
		return st.State == ipc.VoiceJoining && st.Rejoin != nil
	}, "the lost call is not waiting to be joined again")
	require.Equal(t, ipc.VoiceRejoin{Cause: ipc.VoiceRejoinSessionLost}, *st.Rejoin)
	require.Equal(t, SessionLostReason, st.Reason)
	require.Equal(t, "1001", st.Channel)
	require.True(t, v.Busy())
	time.Sleep(100 * time.Millisecond)
	require.Len(t, srv.seen(), 1, "a join was made with no connection to the instance")

	srv.setConnected(true)
	v.Online()
	waitState(t, v, rejoinedAs(ipc.VoiceRejoinSessionLost, 1), "the call was not joined again once online")
	require.Len(t, srv.seen(), 2)
}

// A rejoin that takes its worker down every time is not tried for ever: three attempts, then the status
// says why, and nothing is left to join after a restart (M36's D5).
func TestACallThatCrashesEveryTimeIsGivenUpAfterThreeAttempts(t *testing.T) {
	rejoinFast(t)
	srv, kept := &rejoinServer{}, &crumbs{}
	v := rejoinVoice(t, "crashlater", srv, kept)
	_, err := v.Join(context.Background(), "1001", ipc.VoiceAllOn())
	require.NoError(t, err)
	st := waitState(t, v, func(st ipc.VoiceStatus) bool { return st.State == ipc.VoiceIdle },
		"the rejoins were not given up")
	require.Equal(t, "the call was lost, and joining it again failed 3 times", st.Reason)
	require.Equal(t, &ipc.VoiceRejoin{Cause: ipc.VoiceRejoinWorkerCrashed, Attempt: ipc.MaxVoiceRejoins}, st.Rejoin)
	require.Len(t, srv.seen(), 1+ipc.MaxVoiceRejoins)
	require.False(t, v.Busy())
	last, _ := kept.last()
	require.Nil(t, last)
}

// Every attempt is on disk before it is made, so a rejoin that takes the daemon down with it counts the
// start it cost: the next run sees one more.
func TestEachAttemptIsCountedOnDiskBeforeItIsMade(t *testing.T) {
	rejoinFast(t)
	srv, kept := &rejoinServer{}, &crumbs{}
	v := rejoinVoice(t, "good", srv, kept)
	counted := make(chan int, 4)
	kept.saving = func(c *statefile.VoiceCall) {
		if c != nil {
			counted <- c.Attempts
		}
	}
	srv.refuse = func(int) error {
		select {
		case n := <-counted:
			if n != 3 {
				return errors.New("the attempt on disk was not counted")
			}
		default:
			return errors.New("nothing was written before the attempt")
		}
		return nil
	}
	prev := &statefile.VoiceCall{Instance: testInstance, Account: "42", Channel: "1001", Alive: wallNow(),
		Attempts: 2, Processing: ipc.VoiceAllOn(), Input: ipc.DefaultVoiceInput(), LocalMutes: []string{}}
	v.Resume(prev)
	waitState(t, v, rejoinedAs(ipc.VoiceRejoinDaemonRestarted, 3), "the call was not joined again")
}

// A daemon that started again joins the call its previous run was in, with what the person had set, which
// it has nowhere but the breadcrumb: a caller who had muted themselves comes back muted (M36's done-when),
// their local mutes, cap and input mode with them.
func TestADaemonStartedAgainJoinsItsCallAsItWas(t *testing.T) {
	rejoinFast(t)
	srv, kept := &rejoinServer{}, &crumbs{}
	v := rejoinVoice(t, "good", srv, kept)
	prev := &statefile.VoiceCall{Instance: testInstance + "/", Account: "42", Channel: "1001",
		Alive: wallNow().Add(-20 * time.Second), Processing: ipc.VoiceJoin{EchoCancellation: true},
		Input: ipc.VoiceInput{Mode: ipc.VoiceInputPushToTalk, Key: "ctrl+f5"},
		Self:  ipc.VoiceSelf{Mute: true, Deaf: true}, MuteBeforeDeaf: true, LocalMutes: []string{"7"}, MaxStreams: 2}
	v.Resume(prev)
	st := waitState(t, v, rejoinedAs(ipc.VoiceRejoinDaemonRestarted, 1), "the call was not joined again")
	require.Equal(t, "1001", st.Channel)
	require.Equal(t, ipc.VoiceSelf{Mute: true, Deaf: true}, st.Self)
	require.Equal(t, []string{"7"}, st.LocalMutes)
	require.Equal(t, 2, *st.MaxStreams)
	require.Equal(t, &ipc.VoiceJoin{EchoCancellation: true}, st.Processing)
	require.Equal(t, ipc.VoiceInputPushToTalk, st.Input.Mode)
	require.Equal(t, "ctrl+f5", st.Input.Key)
	require.Equal(t, []joined{{"1001", ipc.VoiceSelf{Mute: true, Deaf: true}}}, srv.seen())

	// The undeafen puts back the mute from before the deafen, which came through the restart too.
	st, err := v.SetSelf(SelfUndeafen)
	require.NoError(t, err)
	require.Equal(t, ipc.VoiceSelf{Mute: true}, st.Self)
}

// What the breadcrumb is from: nothing a person, the instance or the worker decided is joined again, and a
// breadcrumb too old, refused as it was read, or another account's or instance's, is removed unjoined.
func TestWhatIsNotJoinedAgain(t *testing.T) {
	good := func() *statefile.VoiceCall {
		return &statefile.VoiceCall{Instance: testInstance, Account: "42", Channel: "1001", Alive: wallNow(),
			Processing: ipc.VoiceAllOn(), Input: ipc.DefaultVoiceInput(), LocalMutes: []string{}}
	}
	refused := &statefile.VoiceCall{}
	require.NoError(t, json.Unmarshal([]byte(`{"channel":"general"}`), refused))
	require.Error(t, refused.Refused())
	for name, prev := range map[string]func() *statefile.VoiceCall{
		"too old":          func() *statefile.VoiceCall { c := good(); c.Alive = c.Alive.Add(-2 * time.Minute); return c },
		"from the future":  func() *statefile.VoiceCall { c := good(); c.Alive = c.Alive.Add(2 * time.Minute); return c },
		"refused":          func() *statefile.VoiceCall { return refused },
		"another account":  func() *statefile.VoiceCall { c := good(); c.Account = "43"; return c },
		"another instance": func() *statefile.VoiceCall { c := good(); c.Instance = "https://other.example"; return c },
		"attempts used up": func() *statefile.VoiceCall { c := good(); c.Attempts = ipc.MaxVoiceRejoins; return c },
	} {
		t.Run("a breadcrumb "+name, func(t *testing.T) {
			rejoinFast(t)
			srv, kept := &rejoinServer{}, &crumbs{}
			v := rejoinVoice(t, "good", srv, kept)
			v.Resume(prev())
			waitState(t, v, func(st ipc.VoiceStatus) bool { return st.State == ipc.VoiceIdle && !v.Busy() },
				"the breadcrumb is still being joined")
			require.Eventually(t, func() bool { c, ok := kept.last(); return ok && c == nil }, 5*time.Second,
				5*time.Millisecond, "the breadcrumb was not removed")
			require.Empty(t, srv.seen())
		})
	}

	for name, end := range map[string]func(t *testing.T, v *Voice, srv *rejoinServer){
		"a call the worker ended saying why": nil,
		"a call the person left":             func(_ *testing.T, v *Voice, _ *rejoinServer) { v.Leave() },
	} {
		t.Run(name, func(t *testing.T) {
			rejoinFast(t)
			srv, kept := &rejoinServer{}, &crumbs{}
			// devdropped is a call up, then ended by the voice server, the worker saying so.
			mode := "good"
			if end == nil {
				mode = "devdropped"
			}
			v := rejoinVoice(t, mode, srv, kept)
			_, err := v.Join(context.Background(), "1001", ipc.VoiceAllOn())
			require.NoError(t, err)
			require.Eventually(t, func() bool { c, _ := kept.last(); return c != nil }, 5*time.Second,
				5*time.Millisecond, "the call up was never the one to join again")
			if end != nil {
				end(t, v, srv)
			}
			waitState(t, v, func(st ipc.VoiceStatus) bool { return st.State == ipc.VoiceIdle }, "the call is not over")
			time.Sleep(100 * time.Millisecond)
			require.Nil(t, v.Status().Rejoin)
			require.False(t, v.Busy())
			require.Len(t, srv.seen(), 1)
			c, _ := kept.last()
			require.Nil(t, c)
		})
	}

	t.Run("a call whose session was lost before it was up", func(t *testing.T) {
		rejoinFast(t)
		srv, kept := &rejoinServer{}, &crumbs{}
		// The session goes as the answer comes: the join is the person's to make again, who is waiting on it.
		srv.refuse = func(int) error {
			close(srv.lost)
			return nil
		}
		v := rejoinVoice(t, "good", srv, kept)
		_, _ = v.Join(context.Background(), "1001", ipc.VoiceAllOn())
		waitState(t, v, func(st ipc.VoiceStatus) bool { return st.State == ipc.VoiceIdle }, "the call is not over")
		time.Sleep(100 * time.Millisecond)
		require.Nil(t, v.Status().Rejoin)
		require.False(t, v.Busy())
		require.Len(t, srv.seen(), 1)
	})

	t.Run("a rejoin the instance refuses for good", func(t *testing.T) {
		rejoinFast(t)
		srv, kept := &rejoinServer{}, &crumbs{}
		srv.refuse = func(n int) error {
			if n > 1 {
				return &InstanceRefusal{Code: gatewayproto.VoiceForbidden}
			}
			return nil
		}
		v := rejoinVoice(t, "good", srv, kept)
		_, err := v.Join(context.Background(), "1001", ipc.VoiceAllOn())
		require.NoError(t, err)
		srv.lose()
		st := waitState(t, v, func(st ipc.VoiceStatus) bool { return st.State == ipc.VoiceIdle },
			"the refused rejoin was not given up")
		require.Equal(t, "this account may not join that voice channel", st.Reason)
		require.Equal(t, &ipc.VoiceRejoin{Cause: ipc.VoiceRejoinSessionLost, Attempt: 1}, st.Rejoin)
		require.Len(t, srv.seen(), 2, "a final refusal was tried again")
		c, _ := kept.last()
		require.Nil(t, c)
	})
}

// The person's own join or leave, while a rejoin waits, wins: the rejoin is given up, and nothing joins
// behind them when the instance comes back.
func TestThePersonsJoinOrLeaveWinsOverARejoin(t *testing.T) {
	for _, joins := range []bool{false, true} {
		rejoinFast(t)
		rejoinBackoff = []time.Duration{time.Hour}
		srv, kept := &rejoinServer{}, &crumbs{}
		v := rejoinVoice(t, "good", srv, kept)
		srv.setConnected(false)
		v.Resume(&statefile.VoiceCall{Instance: testInstance, Account: "42", Channel: "1001", Alive: wallNow(),
			Processing: ipc.VoiceAllOn(), Input: ipc.DefaultVoiceInput(), LocalMutes: []string{}})
		waitState(t, v, func(st ipc.VoiceStatus) bool { return st.State == ipc.VoiceJoining }, "no rejoin waits")
		srv.setConnected(true)
		if joins {
			st, err := v.Join(context.Background(), "2002", ipc.VoiceAllOn())
			require.NoError(t, err)
			require.Nil(t, st.Rejoin)
			require.Equal(t, "2002", st.Channel)
		} else {
			st := v.Leave()
			require.Equal(t, ipc.VoiceIdle, st.State)
			require.Equal(t, "left", st.Reason)
			require.Nil(t, st.Rejoin)
			require.False(t, v.Busy())
			c, _ := kept.last()
			require.Nil(t, c)
		}
		v.Online()
		time.Sleep(100 * time.Millisecond)
		for _, j := range srv.seen() {
			require.Equal(t, "2002", j.channel, "the given-up rejoin joined")
		}
		v.Close()
	}
}

// The breadcrumb follows the call while it is up: written once it is, again when the person changes
// something, its stamp refreshed, and kept when the daemon stops, for the start that follows. It holds none
// of what the call was given to connect with (rule 8).
func TestTheBreadcrumbFollowsTheCall(t *testing.T) {
	rejoinFast(t)
	aliveEvery = 20 * time.Millisecond
	srv, kept := &rejoinServer{}, &crumbs{}
	v := rejoinVoice(t, "good", srv, kept)
	_, err := v.Join(context.Background(), "1001", ipc.VoiceAllOn())
	require.NoError(t, err)
	var first *statefile.VoiceCall
	require.Eventually(t, func() bool { first, _ = kept.last(); return first != nil }, 5*time.Second, 5*time.Millisecond)
	require.Equal(t, "1001", first.Channel)
	require.Equal(t, testInstance, first.Instance)
	require.Equal(t, "42", first.Account)
	require.NoError(t, first.Check(), "the daemon writes what it reads back")

	_, err = v.SetSelf(SelfMute)
	require.NoError(t, err)
	require.Eventually(t, func() bool { c, _ := kept.last(); return c != nil && c.Self.Mute }, 5*time.Second,
		5*time.Millisecond, "the mute is not in the breadcrumb")
	require.Eventually(t, func() bool { c, _ := kept.last(); return c != nil && c.Alive.After(first.Alive) },
		5*time.Second, 5*time.Millisecond, "the stamp is not refreshed")

	v.Close()
	last, _ := kept.last()
	require.NotNil(t, last, "a daemon stopping removed the call to join again")
	data, err := json.Marshal(last)
	require.NoError(t, err)
	for _, secret := range []string{"the-turn-credential", "1764000000:42", "turn.example", "answer"} {
		require.NotContains(t, string(data), secret)
	}
}

// A refusal's finality is the instance's code: what passes is tried again, what does not is not.
func TestOnlyALastingRefusalEndsARejoin(t *testing.T) {
	for code, final := range map[string]bool{
		gatewayproto.VoiceNotFound: true, gatewayproto.VoiceNotVoice: true, gatewayproto.VoiceForbidden: true,
		gatewayproto.VoiceFull: true, gatewayproto.VoiceBadOffer: true, gatewayproto.VoiceDisabled: true,
		gatewayproto.VoiceBusy: false, gatewayproto.VoiceRateLimited: false, gatewayproto.VoiceUnavailable: false,
		"a_code_from_later": false,
	} {
		require.Equal(t, final, (&InstanceRefusal{Code: code}).Final(), code)
	}
}

// A rejoined call that stays up has its attempts counted from nothing again, so three losses spread over a
// long call do not use up its rejoins.
func TestAttemptsAreCountedAgainOnceARejoinedCallStaysUp(t *testing.T) {
	rejoinFast(t)
	aliveEvery = 20 * time.Millisecond
	var mu sync.Mutex
	now := time.Now()
	defer func(f func() time.Time) { wallNow = f }(wallNow)
	wallNow = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	srv, kept := &rejoinServer{}, &crumbs{}
	v := rejoinVoice(t, "good", srv, kept)
	v.Resume(&statefile.VoiceCall{Instance: testInstance, Account: "42", Channel: "1001", Alive: now,
		Attempts: 2, Processing: ipc.VoiceAllOn(), Input: ipc.DefaultVoiceInput(), LocalMutes: []string{}})
	waitState(t, v, rejoinedAs(ipc.VoiceRejoinDaemonRestarted, 3), "the call was not joined again")
	require.Eventually(t, func() bool { c, _ := kept.last(); return c != nil && c.Attempts == 3 }, 5*time.Second,
		5*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	c, _ := kept.last()
	require.Equal(t, 3, c.Attempts, "counted again before the call stayed up")

	mu.Lock()
	now = now.Add(rejoinWithin)
	mu.Unlock()
	require.Eventually(t, func() bool { c, _ := kept.last(); return c != nil && c.Attempts == 0 }, 5*time.Second,
		5*time.Millisecond, "the attempts were not counted from nothing again")
	v.Close()
}
