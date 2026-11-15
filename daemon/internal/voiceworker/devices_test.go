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
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/voiceipc"
)

// answeringServer gives a route and answers every provision, so a stand-in's call comes up.
type answeringServer struct{}

func (answeringServer) Route(context.Context, string) (Route, error) { return Route{}, nil }
func (answeringServer) Join(context.Context, string, string, string) (string, error) {
	return "v=0\r\nanswer\r\n", nil
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

// Both devices failing answer the join once the worker says the call's start is judged and it goes on,
// rather than at micWithin: the worker ends a call on both failing only as it starts.
func TestAJoinWhoseDevicesFailWithNoEndAnswers(t *testing.T) {
	fast(t)
	v := standinVoice(t, "devfailedlate", answeringServer{})
	start := time.Now()
	st, err := v.Join(context.Background(), "1", ipc.VoiceAllOn())
	require.NoError(t, err)
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
	require.Equal(t, "the voice server closed the call", st.Reason)
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
		Account: func() (SignIn, error) { return SignIn{Account: "42"}, nil }, Log: zerolog.Nop(),
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
