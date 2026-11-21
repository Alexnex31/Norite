// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/voiceipc"
)

// selfServer answers every join, and records what it was told the person set on themselves. While down is
// set, an update is refused as a gateway reconnecting refuses it, and not recorded.
type selfServer struct {
	mu      sync.Mutex
	joined  []ipc.VoiceSelf
	updates []ipc.VoiceSelf
	down    bool
}

func (*selfServer) Prepare(context.Context, string) (Route, error) { return Route{}, nil }
func (s *selfServer) Join(_ context.Context, _, _, _ string, self ipc.VoiceSelf) (Answer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.joined = append(s.joined, self)
	return Answer{SDP: "v=0\r\nanswer\r\n", Rights: voiceipc.Rights{Send: true, Hear: true}}, nil
}
func (s *selfServer) Update(_ context.Context, _, _ string, self ipc.VoiceSelf) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return errors.New("not connected")
	}
	s.updates = append(s.updates, self)
	return nil
}

func (s *selfServer) setDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}
func (*selfServer) Leave(context.Context, string) {}

func (s *selfServer) seen() ([]ipc.VoiceSelf, []ipc.VoiceSelf) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ipc.VoiceSelf(nil), s.joined...), append([]ipc.VoiceSelf(nil), s.updates...)
}

// What the person sets on themselves before a call goes with its join, and what they set during it reaches
// the voice server and the worker, the worker's rights being the instance's less the person's (M34).
func TestTheSelfSwitchesReachTheJoinTheVoiceServerAndTheWorker(t *testing.T) {
	fast(t)
	srv := &selfServer{}
	v := standinVoice(t, "good", srv)
	_, err := v.SetSelf(SelfDeafen)
	require.NoError(t, err)
	st, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	require.NoError(t, err)
	require.Equal(t, ipc.VoiceSelf{Mute: true, Deaf: true}, st.Self)
	joined, _ := srv.seen()
	require.Equal(t, []ipc.VoiceSelf{{Mute: true, Deaf: true}}, joined)

	// Undeafened, the mute from before the deafen comes back, which was none.
	_, err = v.SetSelf(SelfUndeafen)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		st := v.Status()
		_, updates := srv.seen()
		return st.Microphone != nil && st.Microphone.Reason == "send=true hear=true" &&
			len(updates) == 1 && updates[0] == ipc.VoiceSelf{}
	}, 5*time.Second, 10*time.Millisecond, "the worker or the voice server was not told: %+v", v.Status())

	_, err = v.SetSelf(SelfMute)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		st := v.Status()
		return st.Microphone != nil && st.Microphone.Reason == "send=false hear=true"
	}, 5*time.Second, 10*time.Millisecond, "a self mute did not stop the sending: %+v", v.Status())
	// The instance's own rights are not the person's: the status keeps the two apart.
	require.Equal(t, &ipc.VoiceRights{Send: true, Hear: true}, v.Status().Rights)
}

// A self update the voice server did not take, as while the gateway connection is down and resuming, is sent
// again once it can be, and not taken for sent: everybody else would see the old flags for the rest of the
// call (M34's /code-review).
func TestASelfUpdateMadeWhileTheGatewayIsDownIsSentOnceItIsBack(t *testing.T) {
	fast(t)
	srv := &selfServer{}
	v := standinVoice(t, "good", srv)
	_, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	require.NoError(t, err)
	srv.setDown(true)
	_, err = v.SetSelf(SelfDeafen)
	require.NoError(t, err)
	time.Sleep(3 * selfRetryEvery / 2)
	_, updates := srv.seen()
	require.Empty(t, updates)
	srv.setDown(false)
	require.Eventually(t, func() bool {
		_, updates := srv.seen()
		return len(updates) == 1 && updates[0] == ipc.VoiceSelf{Mute: true, Deaf: true}
	}, 5*time.Second, 10*time.Millisecond, "the deafen never reached the voice server")
}

// A daemon whose credential store did not open answers what the person sets with why it cannot, and does
// not stop: it has no sign-in to keep them under, and asking it for one panicked the daemon (M34's
// /code-review).
func TestAVoiceWithNoSignInRefusesTheSwitches(t *testing.T) {
	v := NewVoice(VoiceOptions{Unavailable: "the daemon could not open its credential store", Log: zerolog.Nop()})
	for name, change := range map[string]func() (ipc.VoiceStatus, error){
		"mute":       func() (ipc.VoiceStatus, error) { return v.SetSelf(SelfMute) },
		"local mute": func() (ipc.VoiceStatus, error) { return v.LocalMute("7", true) },
		"streams":    func() (ipc.VoiceStatus, error) { return v.Streams(2) },
	} {
		_, err := change()
		var refused *Refusal
		require.ErrorAs(t, err, &refused, name)
		require.Equal(t, &Refusal{Msg: "the daemon could not open its credential store", Unavailable: true}, refused, name)
	}
}

// A change of processing while the join is under way is read by the join under the lock the change takes
// (M34's /code-review). Staged: once the worker is started, a change is set off on a goroutine of its own
// with nothing ordering it against the join's building, and -race reports a read outside the lock.
func TestAProcessingChangeDuringAJoinIsNoRace(t *testing.T) {
	fast(t)
	srv := &orderingServer{hold: true, joined: make(chan struct{}, 1)}
	v := standinVoice(t, "good", srv)
	changed := make(chan struct{})
	v.started = func() {
		go func() {
			defer close(changed)
			v.Processing(ipc.VoiceJoin{EchoCancellation: true})
		}()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = v.Join(ctx, "1", ipc.VoiceAllOn())
	}()
	require.NoError(t, within(5*time.Second, srv.joined))
	require.NoError(t, within(5*time.Second, changed))
	require.Equal(t, &ipc.VoiceJoin{EchoCancellation: true}, v.Status().Processing)
	cancel()
	v.Leave()
	require.NoError(t, within(5*time.Second, done))
}

// The switches compose as people expect: a deafen mutes, an undeafen puts back the mute from before, and
// an unmute while deafened undeafens too.
func TestTheSelfSwitchesCompose(t *testing.T) {
	v := NewVoice(VoiceOptions{Account: func() (SignIn, error) { return SignIn{Account: "42"}, nil },
		Log: zerolog.Nop()})
	steps := []struct {
		what string
		want ipc.VoiceSelf
	}{
		{SelfMute, ipc.VoiceSelf{Mute: true}},
		{SelfDeafen, ipc.VoiceSelf{Mute: true, Deaf: true}},
		{SelfUndeafen, ipc.VoiceSelf{Mute: true}},
		{SelfUnmute, ipc.VoiceSelf{}},
		{SelfDeafen, ipc.VoiceSelf{Mute: true, Deaf: true}},
		{SelfMute, ipc.VoiceSelf{Mute: true, Deaf: true}},
		{SelfUndeafen, ipc.VoiceSelf{Mute: true}},
		{SelfDeafen, ipc.VoiceSelf{Mute: true, Deaf: true}},
		{SelfUnmute, ipc.VoiceSelf{}},
		{SelfUndeafen, ipc.VoiceSelf{}},
	}
	for i, s := range steps {
		st, err := v.SetSelf(s.what)
		require.NoError(t, err)
		require.Equal(t, s.want, st.Self, "step %d, %s", i, s.what)
	}
}

// A local mute reaches the worker as a subscribe, and so does how many people the person hears at once;
// both hold for the next call too (M34).
func TestALocalMuteAndTheStreamsReachTheWorkerAndTheNextCall(t *testing.T) {
	fast(t)
	v := standinVoice(t, "good", &selfServer{})
	_, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	require.NoError(t, err)
	_, err = v.LocalMute("7", true)
	require.NoError(t, err)
	_, err = v.Streams(2)
	require.NoError(t, err)
	reached := func(want string) {
		t.Helper()
		require.Eventually(t, func() bool {
			st := v.Status()
			return st.DSP != nil && st.DSP.Reason == want
		}, 5*time.Second, 10*time.Millisecond, "the worker was not told %s: %+v", want, v.Status().DSP)
	}
	reached("drop=7 cap=2")

	v.Leave()
	st, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	require.NoError(t, err)
	require.Equal(t, []string{"7"}, st.LocalMutes)
	reached("drop=7 cap=2")
	_, err = v.LocalMute("7", false)
	require.NoError(t, err)
	_, err = v.Streams(0)
	require.NoError(t, err)
	reached("drop= cap=none")
}

// Who speaks and what the processing is doing reach the status, and a change of processing during the call
// reaches the worker (M34).
func TestWhoSpeaksAndTheProcessingReachTheStatus(t *testing.T) {
	fast(t)
	v := standinVoice(t, "speaks", &selfServer{})
	_, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		st := v.Status()
		return len(st.Speaking) == 1 && st.Speaking[0] == "7" && st.DSP != nil && st.DSP.State == ipc.VoiceDSPRunning
	}, 5*time.Second, 10*time.Millisecond, "%+v", v.Status())

	// The answer says what runs once the worker has rebuilt its processing, which the stand-in takes a
	// moment to, as the worker does: answered at once, it said the processing from before (M34's manual
	// pass).
	// Each step changes one stage, so that each is what the answer waits on, and the answer comes when the
	// worker says so, well before processingWithin.
	for _, asked := range []ipc.VoiceJoin{
		{NoiseSuppression: true, AutoGain: true},
		{AutoGain: true},
		{},
	} {
		start := time.Now()
		st := v.Processing(asked)
		require.Less(t, time.Since(start), processingWithin, "%+v", asked)
		require.Equal(t, &asked, st.Processing, "what was asked")
		require.Equal(t, &ipc.VoiceDSP{State: ipc.VoiceDSPRunning, EchoCancellation: asked.EchoCancellation,
			NoiseSuppression: asked.NoiseSuppression, AutoGain: asked.AutoGain}, st.DSP, "what runs")
	}

	st := v.Leave()
	require.Empty(t, st.Speaking)
	require.Nil(t, st.DSP)
}

// A change of processing the worker never reports is answered after processingWithin, saying what still
// runs; one made without a call is answered at once (M34).
func TestAChangeOfProcessingIsAnsweredWithinItsBound(t *testing.T) {
	fast(t)
	defer func(d time.Duration) { processingWithin = d }(processingWithin)
	processingWithin = 300 * time.Millisecond
	v := standinVoice(t, "dspsilent", &selfServer{})
	start := time.Now()
	require.Equal(t, ipc.VoiceStatus{State: ipc.VoiceIdle, Available: true, LocalMutes: []string{},
		Speaking: []string{}}, v.Processing(ipc.VoiceJoin{}), "without a call")
	require.Less(t, time.Since(start), processingWithin)

	_, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		d := v.Status().DSP
		return d != nil && d.State == ipc.VoiceDSPRunning
	}, 5*time.Second, 10*time.Millisecond, "%+v", v.Status())
	start = time.Now()
	answered := make(chan ipc.VoiceStatus, 1)
	go func() { answered <- v.Processing(ipc.VoiceJoin{AutoGain: true}) }()
	var st ipc.VoiceStatus
	select {
	case st = <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("a change the worker never reports was never answered")
	}
	require.GreaterOrEqual(t, time.Since(start), processingWithin)
	require.Equal(t, &ipc.VoiceDSP{State: ipc.VoiceDSPRunning, EchoCancellation: true, NoiseSuppression: true,
		AutoGain: true}, st.DSP, "what still runs")
}

// A processing that failed answers a change at once: no change brings it back (M34).
func TestAFailedProcessingAnswersAChangeAtOnce(t *testing.T) {
	fast(t)
	v := standinVoice(t, "dspfails", &selfServer{})
	_, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		d := v.Status().DSP
		return d != nil && d.State == ipc.VoiceDSPFailed
	}, 5*time.Second, 10*time.Millisecond, "%+v", v.Status())
	start := time.Now()
	st := v.Processing(ipc.VoiceJoin{})
	require.Less(t, time.Since(start), processingWithin/2)
	require.Equal(t, ipc.VoiceDSPFailed, st.DSP.State)
}

// What the person set goes with the sign-in it was set under: another sign-in starts from nothing (M34's
// D1 and D3).
func TestThePersonsSwitchesEndWithTheSignIn(t *testing.T) {
	ended := make(chan struct{})
	signIn := SignIn{Account: "42", Ended: ended}
	var mu sync.Mutex
	v := NewVoice(VoiceOptions{Log: zerolog.Nop(), Account: func() (SignIn, error) {
		mu.Lock()
		defer mu.Unlock()
		return signIn, nil
	}})
	_, err := v.SetSelf(SelfDeafen)
	require.NoError(t, err)
	_, err = v.LocalMute("7", true)
	require.NoError(t, err)
	require.Equal(t, []string{"7"}, v.Status().LocalMutes)

	close(ended)
	st := v.Status()
	require.Equal(t, ipc.VoiceSelf{}, st.Self)
	require.Empty(t, st.LocalMutes)

	mu.Lock()
	signIn = SignIn{Account: "42", Ended: make(chan struct{})}
	mu.Unlock()
	st, err = v.LocalMute("8", true)
	require.NoError(t, err)
	require.Equal(t, []string{"8"}, st.LocalMutes, "the ended sign-in's mutes came back")

	// A sign-in replaced before the end of the one it replaced is seen: the prefs are still another's.
	mu.Lock()
	signIn = SignIn{Account: "43", Ended: make(chan struct{})}
	mu.Unlock()
	st, err = v.LocalMute("9", true)
	require.NoError(t, err)
	require.Equal(t, []string{"9"}, st.LocalMutes, "another sign-in's mutes carried over")
}

// What the worker says of who speaks or of its processing that no correct worker says breaks the protocol,
// and ends the call, as any such frame does.
func TestAWorkerSpeakingOutOfTurnIsKilled(t *testing.T) {
	fast(t)
	for _, mode := range []string{"speaksbadly", "dspbadly"} {
		v := standinVoice(t, mode, &selfServer{})
		// As the join waits on the microphone, or once it is up: either way the worker is gone, saying why.
		if _, err := v.Join(context.Background(), "1", ipc.VoiceAllOn()); err != nil {
			require.ErrorContains(t, err, "broke the protocol", mode)
		}
		require.Eventually(t, func() bool {
			st := v.Status()
			return st.State == ipc.VoiceIdle && strings.Contains(st.Reason, "broke the protocol")
		}, 5*time.Second, 10*time.Millisecond, "%s: %+v", mode, v.Status())
	}
}

// One account mutes at most a full room for itself.
func TestLocalMutesAreBounded(t *testing.T) {
	v := NewVoice(VoiceOptions{Account: func() (SignIn, error) { return SignIn{Account: "42"}, nil },
		Log: zerolog.Nop()})
	for i := range ipc.MaxLocalMutes {
		_, err := v.LocalMute(fmt.Sprint(1000+i), true)
		require.NoError(t, err)
	}
	_, err := v.LocalMute("5", true)
	require.ErrorContains(t, err, "at most 64")
	_, err = v.LocalMute("1000", true)
	require.NoError(t, err, "muting somebody muted already is no new one")
	require.Len(t, v.Status().LocalMutes, ipc.MaxLocalMutes)
}

// The daemon's words for the processing's state are the worker's, which daemon/ipc cannot import, and the
// contract's (M34).
func TestTheDSPStatesAgreeAcrossPackages(t *testing.T) {
	want := []string{voiceipc.DSPStarting, voiceipc.DSPRunning, voiceipc.DSPFailed}
	require.Equal(t, want, []string{ipc.VoiceDSPStarting, ipc.VoiceDSPRunning, ipc.VoiceDSPFailed})
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "daemon-ipc.schema.json"))
	require.NoError(t, err)
	var doc struct {
		Defs struct {
			VoiceStatus struct {
				Properties struct {
					DSP struct {
						OneOf []struct {
							Properties struct {
								State struct {
									Enum []string `json:"enum"`
								} `json:"state"`
							} `json:"properties"`
						} `json:"oneOf"`
					} `json:"dsp"`
				} `json:"properties"`
			} `json:"VoiceStatus"`
		} `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Len(t, doc.Defs.VoiceStatus.Properties.DSP.OneOf, 2)
	require.Equal(t, want, doc.Defs.VoiceStatus.Properties.DSP.OneOf[1].Properties.State.Enum)
}

// heldServer holds its answer until released.
type heldServer struct {
	selfServer
	asked, release chan struct{}
}

func (s *heldServer) Join(ctx context.Context, a, ch, o string, self ipc.VoiceSelf) (Answer, error) {
	close(s.asked)
	<-s.release
	return s.selfServer.Join(ctx, a, ch, o, self)
}

// What the person changes while the join waits for its answer reaches the worker once it has the answer,
// never before: a worker takes no SUBSCRIBE before its ANSWER. The voice server, joined with the old self,
// is told the new one.
func TestAChangeDuringTheJoinWaitsForTheAnswer(t *testing.T) {
	fast(t)
	srv := &heldServer{asked: make(chan struct{}), release: make(chan struct{})}
	v := standinVoice(t, "good", srv)
	joined := make(chan error, 1)
	go func() {
		_, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
		joined <- err
	}()
	<-srv.asked
	_, err := v.LocalMute("7", true)
	require.NoError(t, err)
	_, err = v.SetSelf(SelfMute)
	require.NoError(t, err)
	close(srv.release)
	require.NoError(t, <-joined)
	require.Eventually(t, func() bool {
		st := v.Status()
		_, updates := srv.seen()
		return st.DSP != nil && st.DSP.Reason == "drop=7 cap=none" && len(updates) == 1 &&
			updates[0] == ipc.VoiceSelf{Mute: true}
	}, 5*time.Second, 10*time.Millisecond, "%+v", v.Status())
}
