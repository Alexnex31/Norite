// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceworker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	require.Equal(t, "neither the microphone nor the speakers can be used", refused.Msg)
	require.Equal(t, ipc.VoiceIdle, st.State)
	none := &ipc.VoiceDevice{State: ipc.VoiceDeviceNoSession, Reason: "nobody is logged in"}
	require.Equal(t, none, st.Microphone)
	require.Equal(t, none, st.Speakers)
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
	type device struct {
		OneOf []struct {
			Properties struct {
				State struct {
					Enum []string `json:"enum"`
				} `json:"state"`
			} `json:"properties"`
		} `json:"oneOf"`
	}
	var doc struct {
		Defs struct {
			VoiceStatus struct {
				Properties struct {
					Microphone device `json:"microphone"`
					Speakers   device `json:"speakers"`
				} `json:"properties"`
			} `json:"VoiceStatus"`
		} `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	want := []string{ipc.VoiceDeviceOpening, ipc.VoiceDeviceOpen, ipc.VoiceDeviceAsking,
		ipc.VoiceDeviceRefused, ipc.VoiceDeviceNoDevice, ipc.VoiceDeviceNoSession, ipc.VoiceDeviceFailed}
	for _, d := range []device{doc.Defs.VoiceStatus.Properties.Microphone, doc.Defs.VoiceStatus.Properties.Speakers} {
		require.Len(t, d.OneOf, 2)
		require.Equal(t, want, d.OneOf[1].Properties.State.Enum)
	}
}
