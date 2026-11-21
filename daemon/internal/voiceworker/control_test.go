// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceworker

import (
	"context"
	"encoding/json"
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

// selfServer answers every join, and records what it was told the person set on themselves.
type selfServer struct {
	mu      sync.Mutex
	joined  []ipc.VoiceSelf
	updates []ipc.VoiceSelf
}

func (*selfServer) Prepare(context.Context, string) (Route, error) { return Route{}, nil }
func (s *selfServer) Join(_ context.Context, _, _, _ string, self ipc.VoiceSelf) (Answer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.joined = append(s.joined, self)
	return Answer{SDP: "v=0\r\nanswer\r\n", Rights: voiceipc.Rights{Send: true, Hear: true}}, nil
}
func (s *selfServer) Update(_ context.Context, _, _ string, self ipc.VoiceSelf) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, self)
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

	only := ipc.VoiceJoin{NoiseSuppression: true}
	st := v.Processing(only)
	require.Equal(t, &only, st.Processing, "what was asked")
	require.Eventually(t, func() bool {
		d := v.Status().DSP
		return d != nil && *d == ipc.VoiceDSP{State: ipc.VoiceDSPRunning, NoiseSuppression: true}
	}, 5*time.Second, 10*time.Millisecond, "%+v", v.Status().DSP)

	st = v.Leave()
	require.Empty(t, st.Speaking)
	require.Nil(t, st.DSP)
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
