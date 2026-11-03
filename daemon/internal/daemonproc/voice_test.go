// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonproc

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/internal/voiceworker"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// refusingSignaler is a voice server that refuses every call, so a join fails at the first step without
// starting a worker.
type refusingSignaler struct{}

func (refusingSignaler) Route(context.Context, string) (voiceworker.Route, error) {
	return voiceworker.Route{}, errRefusing
}

func (refusingSignaler) Join(context.Context, string, string, string) (string, error) {
	return "", errRefusing
}

var errRefusing = &voiceworker.Refusal{Msg: "the voice server refused: 409 room full"}

func voiceFixture(signaler voiceworker.Signaler, signedIn bool) *localRequests {
	return &localRequests{log: zerolog.Nop(), voice: voiceworker.NewVoice(voiceworker.VoiceOptions{
		Program: "/nonexistent/norite-voice", Version: "dev", Signaler: signaler,
		Unavailable: "voice calls arrive in a later version of Norite",
		Account:     func() (string, bool) { return "42", signedIn },
		Log:         zerolog.Nop(),
	})}
}

func TestTheVoiceRequestsAnswerAStatus(t *testing.T) {
	l := voiceFixture(nil, true)
	resp := l.Do(context.Background(), ipc.Request{Method: http.MethodGet, Path: ipc.PathVoice})
	require.Nil(t, resp.Error)
	matchesDefinition(t, "VoiceStatus", resp.Body)
	var st ipc.VoiceStatus
	require.NoError(t, json.Unmarshal(resp.Body, &st))
	require.Equal(t, ipc.VoiceStatus{State: ipc.VoiceIdle,
		Unavailable: "voice calls arrive in a later version of Norite"}, st)

	resp = l.Do(context.Background(), ipc.Request{Method: http.MethodPost, Path: ipc.PathVoiceLeave})
	require.Nil(t, resp.Error)
	matchesDefinition(t, "VoiceStatus", resp.Body)
}

func TestTheVoiceRequestsRefuseWhatTheyCannotDo(t *testing.T) {
	cases := []struct {
		name     string
		l        *localRequests
		method   string
		path     string
		code     string
		contains string
	}{
		{"a read that is not GET", voiceFixture(nil, true), http.MethodPost, ipc.PathVoice, ipc.RelayBadRequest, ""},
		{"a join that is not POST", voiceFixture(nil, true), http.MethodGet, ipc.PathVoiceJoin("1"), ipc.RelayBadRequest, ""},
		{"a channel that is not an id", voiceFixture(nil, true), http.MethodPost, ipc.PathVoice + "/join/general", ipc.RelayBadRequest, ""},
		{"no channel", voiceFixture(nil, true), http.MethodPost, ipc.PathVoice + "/join/", ipc.RelayBadRequest, ""},
		{"a path that is none of them", voiceFixture(nil, true), http.MethodPost, ipc.PathVoice + "/mute", ipc.RelayBadRequest, ""},
		{"a release, which cannot join yet", voiceFixture(nil, true), http.MethodPost, ipc.PathVoiceJoin("1"), ipc.RelayConflict, "arrive in a later version"},
		{"nobody signed in", voiceFixture(refusingSignaler{}, false), http.MethodPost, ipc.PathVoiceJoin("1"), ipc.RelayConflict, "norite login"},
		{"a voice server that refuses", voiceFixture(refusingSignaler{}, true), http.MethodPost, ipc.PathVoiceJoin("1"), ipc.RelayFailed, "room full"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := tc.l.Do(context.Background(), ipc.Request{Method: tc.method, Path: tc.path})
			require.NotNil(t, resp.Error)
			require.Equal(t, tc.code, resp.Error.Code)
			require.Contains(t, resp.Error.Message, tc.contains)
			require.False(t, tc.l.voice.Busy())
		})
	}
	// A join that failed leaves its reason for whoever asks next.
	l := voiceFixture(refusingSignaler{}, true)
	_ = l.Do(context.Background(), ipc.Request{Method: http.MethodPost, Path: ipc.PathVoiceJoin("7")})
	var st ipc.VoiceStatus
	require.NoError(t, json.Unmarshal(l.Do(context.Background(), ipc.Request{Method: http.MethodGet, Path: ipc.PathVoice}).Body, &st))
	require.Equal(t, ipc.VoiceIdle, st.State)
	require.Contains(t, st.Reason, "room full")
}
