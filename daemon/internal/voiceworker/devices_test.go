// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceworker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/voiceipc"
)

// answeringServer gives a route and answers every provision, so a stand-in's call comes up.
type answeringServer struct{}

func (answeringServer) Prepare(context.Context, string) (Route, error) { return Route{}, nil }
func (answeringServer) Join(context.Context, string, string, string, ipc.VoiceSelf) (Answer, error) {
	return Answer{SDP: "v=0\r\nanswer\r\n", Rights: voiceipc.Rights{Send: true, Hear: true}}, nil
}

// The devices a join answers with are the worker's (M32): it waits for the microphone to be past opening,
// keeps what the worker says after, and cleans the worker's words.
func TestAJoinWaitsForTheMicrophone(t *testing.T) {
	fast(t)
	v := standinVoice(t, "devopen", answeringServer{})
	start := time.Now()
	st, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	require.NoError(t, err)
	require.GreaterOrEqual(t, time.Since(start), 250*time.Millisecond, "the join did not wait for the first frame")
	require.Equal(t, ipc.VoiceInCall, st.State)
	require.Equal(t, &ipc.VoiceDevice{State: ipc.VoiceDeviceOpen}, st.Microphone)
	require.Equal(t, &ipc.VoiceDevice{State: ipc.VoiceDeviceOpen}, st.Speakers)
	require.Eventually(t, func() bool {
		m := v.Status().Microphone
		return m != nil && m.State == ipc.VoiceDeviceFailed
	}, 5*time.Second, 10*time.Millisecond, "a device lost mid-call was not kept")
	require.Equal(t, "it stopped delivering audio �[31m", v.Status().Microphone.Reason)
	require.Equal(t, ipc.VoiceInCall, v.Status().State, "one device failing ended the call")
	st = v.Leave()
	require.Nil(t, st.Microphone, "a leave kept the devices")
	require.Nil(t, st.Speakers)
}

// macOS's question is past opening: the join answers while it is open, never waiting on the person. It is
// asked as the call starts, so what the worker says while connecting is kept too.
func TestAJoinAnswersWhileTheQuestionIsOpen(t *testing.T) {
	fast(t)
	v := standinVoice(t, "devasking", answeringServer{})
	start := time.Now()
	st, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	require.NoError(t, err)
	require.Less(t, time.Since(start), micWithin, "the join waited out its bound")
	require.Equal(t, &ipc.VoiceDevice{State: ipc.VoiceDeviceAsking}, st.Microphone)
}

// A worker that says nothing of its devices is waited for to the bound, and the call is up.
func TestAJoinWaitsForTheMicrophoneOnlySoLong(t *testing.T) {
	fast(t)
	defer func(d time.Duration) { micWithin = d }(micWithin)
	micWithin = 200 * time.Millisecond
	v := standinVoice(t, "devsilent", answeringServer{})
	start := time.Now()
	st, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	require.NoError(t, err)
	require.Less(t, time.Since(start), 3*time.Second)
	require.Equal(t, ipc.VoiceInCall, st.State)
	require.Nil(t, st.Microphone)
}

// A call whose devices both fail as it starts is a failed join, with the worker's reason, and the status
// keeps the devices that ended it, for the client to word.
func TestAJoinWhoseDevicesBothFailFails(t *testing.T) {
	fast(t)
	v := standinVoice(t, "devneither", answeringServer{})
	st, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	var refused *Refusal
	require.True(t, errors.As(err, &refused), "%v", err)
	require.True(t, refused.Failed)
	require.Equal(t, "neither the microphone nor the speakers can be used: nobody is logged in", refused.Msg)
	require.Equal(t, ipc.VoiceIdle, st.State)
	none := &ipc.VoiceDevice{State: ipc.VoiceDeviceNoSession, Reason: "nobody is logged in"}
	require.Equal(t, none, st.Microphone)
	require.Equal(t, none, st.Speakers)

	// A leave with no call clears them, as the contract says, and keeps why the join failed.
	st = v.Leave()
	require.Nil(t, st.Microphone)
	require.Nil(t, st.Speakers)
	require.Equal(t, "neither the microphone nor the speakers can be used: nobody is logged in", st.Reason)
}

// A call that ends otherwise, its devices both failing in the status by then, keeps none: only the devices
// an ended state names are kept.
func TestAnEndNotTheDevicesKeepsNone(t *testing.T) {
	fast(t)
	v := standinVoice(t, "devclaims", answeringServer{})
	_, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	require.NoError(t, err)
	require.Eventually(t, func() bool { return v.Status().State == ipc.VoiceIdle }, 5*time.Second, 10*time.Millisecond)
	st := v.Status()
	require.Equal(t, "their end", st.Reason)
	require.Nil(t, st.Microphone)
	require.Nil(t, st.Speakers)
}

// A join that fails for the daemon's own reason keeps no devices, even when the worker it left was ending
// the call on its devices as it did: the status's reason is the daemon's, which they would contradict.
func TestAJoinThatTimesOutKeepsNoDevices(t *testing.T) {
	fast(t)
	r := readyWithin
	readyWithin = 300 * time.Millisecond
	t.Cleanup(func() { readyWithin = r })
	v := standinVoice(t, "endsasleft", answeringServer{})
	st, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	require.ErrorContains(t, err, "did not connect within")
	require.Equal(t, ipc.VoiceIdle, st.State)
	require.Nil(t, st.Microphone)
	require.Nil(t, st.Speakers)
}

// Both devices failing answer the join once the worker says the call's start is judged and it goes on,
// rather than at micWithin: the worker ends a call on both failing only as it starts.
func TestAJoinWhoseDevicesFailWithNoEndAnswers(t *testing.T) {
	fast(t)
	v := standinVoice(t, "devfailedlate", answeringServer{})
	start := time.Now()
	st, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	require.NoError(t, err)
	// The stand-in says the start is judged 300 ms after its first report, which already has both failing.
	require.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond, "the join answered before it was judged")
	require.Less(t, time.Since(start), micWithin, "the join waited out its bound")
	require.Equal(t, ipc.VoiceInCall, st.State)
	require.Equal(t, ipc.VoiceDeviceNoDevice, st.Microphone.State)
}

// A worker ending the call as its microphone is waited for fails the join, whatever reason it gives,
// none included.
func TestAJoinWhoseWorkerEndsSilentlyFails(t *testing.T) {
	fast(t)
	v := standinVoice(t, "endsilently", answeringServer{})
	st, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	var refused *Refusal
	require.True(t, errors.As(err, &refused), "%v", err)
	require.Equal(t, "the voice worker ended the call without saying why", refused.Msg)
	require.Equal(t, ipc.VoiceIdle, st.State)
}

// The daemon's words for a device's state are the worker's, which daemon/ipc cannot import, and the
// contract's.
func TestTheDeviceStatesAgreeAcrossPackages(t *testing.T) {
	require.Equal(t,
		[]string{voiceipc.DeviceOpening, voiceipc.DeviceOpen, voiceipc.DeviceAsking, voiceipc.DeviceRefused,
			voiceipc.DeviceNoDevice, voiceipc.DeviceNoSession, voiceipc.DeviceFailed},
		[]string{ipc.VoiceDeviceOpening, ipc.VoiceDeviceOpen, ipc.VoiceDeviceAsking, ipc.VoiceDeviceRefused,
			ipc.VoiceDeviceNoDevice, ipc.VoiceDeviceNoSession, ipc.VoiceDeviceFailed})
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "daemon-ipc.schema.json"))
	require.NoError(t, err)
	var doc struct {
		Defs struct {
			VoiceDevice struct {
				Properties struct {
					State struct {
						Enum []string `json:"enum"`
					} `json:"state"`
				} `json:"properties"`
			} `json:"VoiceDevice"`
		} `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Equal(t, []string{ipc.VoiceDeviceOpening, ipc.VoiceDeviceOpen, ipc.VoiceDeviceAsking,
		ipc.VoiceDeviceRefused, ipc.VoiceDeviceNoDevice, ipc.VoiceDeviceNoSession, ipc.VoiceDeviceFailed},
		doc.Defs.VoiceDevice.Properties.State.Enum)
}

// A call that ends for any reason but its devices does not keep them: they would describe devices nothing
// has open any more, and the client prints them. Both are failing as this one ends, the session having
// ended mid-call, and the end is still not theirs.
func TestACallEndedOtherwiseKeepsNoDevices(t *testing.T) {
	fast(t)
	v := standinVoice(t, "devdropped", answeringServer{})
	st, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	require.NoError(t, err)
	require.Equal(t, &ipc.VoiceDevice{State: ipc.VoiceDeviceOpen}, st.Microphone)
	require.Eventually(t, func() bool { return v.Status().State == ipc.VoiceIdle }, 5*time.Second, 10*time.Millisecond)
	st = v.Status()
	require.Equal(t, "you may no longer join this voice channel", st.Reason)
	require.True(t, st.EndedInCall, "a call that was up ended, and the status says it was a call")
	require.Nil(t, st.Microphone)
	require.Nil(t, st.Speakers)
}

// A join after one its devices failed publishes the new call joining first: leaving the call there is not
// is no status of anybody's, the kept devices included.
func TestAJoinAfterADevicesFailurePublishesNoLeave(t *testing.T) {
	fast(t)
	o := standinOptions(t, "devneither", nil)
	var mu sync.Mutex
	var seen []ipc.VoiceStatus
	v := NewVoice(VoiceOptions{Program: o.Program, Version: o.Version, Signaler: answeringServer{}, Env: o.Env,
		Account: func() (SignIn, error) { return SignIn{Account: "42", Instance: testInstance}, nil }, Log: zerolog.Nop(),
		Changed: func(st ipc.VoiceStatus) {
			mu.Lock()
			seen = append(seen, st)
			mu.Unlock()
		}})
	t.Cleanup(v.Close)
	_, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	require.Error(t, err)
	require.NotNil(t, v.Status().Microphone)
	mu.Lock()
	seen = nil
	mu.Unlock()
	_, _ = v.Join(context.Background(), "1", ipc.VoiceAllOn())
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, seen)
	require.Equal(t, ipc.VoiceJoining, seen[0].State, "%+v", seen[0])
}

func (answeringServer) Update(context.Context, string, string, ipc.VoiceSelf) error { return nil }
func (answeringServer) Leave(context.Context, string)                               {}

// changingServer answers with rights the instance later changes, and counts the leaves it is told of.
type changingServer struct {
	changes chan voiceipc.Rights
	leaves  atomic.Int32
}

func (*changingServer) Prepare(context.Context, string) (Route, error) { return Route{}, nil }
func (s *changingServer) Join(context.Context, string, string, string, ipc.VoiceSelf) (Answer, error) {
	return Answer{SDP: "v=0\r\nanswer\r\n", Rights: voiceipc.Rights{Send: false, Hear: true}, Changes: s.changes}, nil
}
func (s *changingServer) Update(context.Context, string, string, ipc.VoiceSelf) error { return nil }
func (s *changingServer) Leave(context.Context, string)                               { s.leaves.Add(1) }

// What the account may do reaches the status with the answer, and each change the instance sends reaches
// the status and the worker (M33); leaving tells the voice server.
func TestTheRightsReachTheStatusAndTheWorker(t *testing.T) {
	fast(t)
	srv := &changingServer{changes: make(chan voiceipc.Rights, 1)}
	v := standinVoice(t, "good", srv)
	st, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	require.NoError(t, err)
	require.Equal(t, &ipc.VoiceRights{Send: false, Hear: true}, st.Rights, "the answer's rights")

	srv.changes <- voiceipc.Rights{Send: true, Hear: false}
	require.Eventually(t, func() bool {
		st := v.Status()
		return st.Rights != nil && *st.Rights == ipc.VoiceRights{Send: true, Hear: false} &&
			st.Microphone != nil && st.Microphone.Reason == "send=true hear=false"
	}, 5*time.Second, 10*time.Millisecond, "the change reached the status, or the worker, or neither")

	st = v.Leave()
	require.Nil(t, st.Rights, "a call left has no rights")
	require.Equal(t, int32(1), srv.leaves.Load(), "the voice server was not told the call was left")
}

// losingServer answers with a Lost the test closes, as the gateway's signaler does when the instance no
// longer holds the session.
type losingServer struct {
	lost   chan struct{}
	leaves atomic.Int32
}

// Prepare refuses once the session is lost, as the gateway's does until the new one is live.
func (s *losingServer) Prepare(context.Context, string) (Route, error) {
	select {
	case <-s.lost:
		return Route{}, errors.New("the daemon is not connected to its instance")
	default:
		return Route{}, nil
	}
}
func (s *losingServer) Join(context.Context, string, string, string, ipc.VoiceSelf) (Answer, error) {
	return Answer{SDP: "v=0\r\nanswer\r\n", Rights: voiceipc.Rights{Send: true, Hear: true}, Lost: s.lost}, nil
}
func (s *losingServer) Update(context.Context, string, string, ipc.VoiceSelf) error { return nil }
func (s *losingServer) Leave(context.Context, string)                               { s.leaves.Add(1) }

// A call whose session the instance no longer holds is let go at once, saying so (M33's manual pass): its
// worker ends and the voice server is told. Since M36 it is joined again once the instance is reachable,
// and until then the status says why it was lost.
func TestACallLostWithItsSessionIsLetGoAtOnce(t *testing.T) {
	fast(t)
	srv := &losingServer{lost: make(chan struct{})}
	v := standinVoice(t, "good", srv)
	_, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	require.NoError(t, err)
	v.mu.Lock()
	w := v.current.worker
	v.mu.Unlock()
	close(srv.lost)
	require.Eventually(t, func() bool {
		st := v.Status()
		return st.State == ipc.VoiceJoining && st.Reason == SessionLostReason && st.Rejoin != nil
	}, 5*time.Second, 10*time.Millisecond, "%+v", v.Status())
	require.NoError(t, within(5*time.Second, w.Done()), "the lost call's worker was not let go")
	require.Eventually(t, func() bool { return srv.leaves.Load() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the voice server was not told")
	require.Equal(t, ipc.VoiceRejoin{Cause: ipc.VoiceRejoinSessionLost}, *v.Status().Rejoin)
	require.True(t, v.Busy())
}
