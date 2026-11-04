// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonproc

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/credentials"
	"github.com/Alexnex31/Norite/daemon/internal/session"
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
		Account: func() (string, error) {
			if !signedIn {
				return "", voiceworker.ErrSignedOut
			}
			return "42", nil
		},
		Log: zerolog.Nop(),
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

// A sign-in not read yet answers "unavailable", never "log in", which would replace a good sign-in.
func TestAJoinBeforeTheSignInIsReadIsUnavailable(t *testing.T) {
	l := &localRequests{log: zerolog.Nop(), voice: voiceworker.NewVoice(voiceworker.VoiceOptions{
		Signaler: refusingSignaler{}, Log: zerolog.Nop(),
		Account: func() (string, error) { return "", voiceworker.ErrSignInPending },
	})}
	resp := l.Do(context.Background(), ipc.Request{Method: http.MethodPost, Path: ipc.PathVoiceJoin("1")})
	require.NotNil(t, resp.Error)
	require.Equal(t, ipc.RelayUnreachable, resp.Error.Code)
	require.NotContains(t, resp.Error.Message, "norite login")
}

// A daemon whose credential store could not be opened answers about voice with the reason, rather than
// refusing the request as one it does not know, which the CLI reads as an older daemon.
func TestNoCredentialStoreStillAnswersAboutVoice(t *testing.T) {
	l := &localRequests{log: zerolog.Nop(), voice: noVoice("0.4.0")}
	resp := l.Do(context.Background(), ipc.Request{Method: http.MethodGet, Path: ipc.PathVoice})
	require.Nil(t, resp.Error)
	matchesDefinition(t, "VoiceStatus", resp.Body)
	var st ipc.VoiceStatus
	require.NoError(t, json.Unmarshal(resp.Body, &st))
	require.False(t, st.Available)
	require.Contains(t, st.Unavailable, "credential store")
}

// holdingSignaler holds the route until its context ends, so a join stays under way.
type holdingSignaler struct{}

func (holdingSignaler) Route(ctx context.Context, _ string) (voiceworker.Route, error) {
	<-ctx.Done()
	return voiceworker.Route{}, ctx.Err()
}

func (holdingSignaler) Join(context.Context, string, string, string) (string, error) {
	return "", errRefusing
}

type countingSink struct{ ends atomic.Int32 }

func (*countingSink) Begin(uint64)                     {}
func (*countingSink) Dispatch(string, json.RawMessage) {}
func (s *countingSink) End()                           { s.ends.Add(1) }

// A sign-in that ends takes its call with it (M19's rule), as it takes the state.
func TestASignInThatEndsEndsTheCall(t *testing.T) {
	calls := voiceworker.NewVoice(voiceworker.VoiceOptions{Signaler: holdingSignaler{}, Log: zerolog.Nop(),
		Account: func() (string, error) { return "42", nil }})
	go func() { _, _ = calls.Join(context.Background(), "1") }()
	require.Eventually(t, calls.Busy, 5*time.Second, 5*time.Millisecond)

	inner := &countingSink{}
	endingCalls{Sink: inner, calls: calls}.End()
	require.EqualValues(t, 1, inner.ends.Load(), "the state is told first, as before")
	require.Eventually(t, func() bool { return !calls.Busy() }, 5*time.Second, 5*time.Millisecond)
	require.Equal(t, ipc.VoiceIdle, calls.Status().State)
}

// A session that has not read its store yet is pending, not signed out.
func TestASignInNotReadYetIsPending(t *testing.T) {
	store, err := credentials.OpenIn(t.TempDir())
	require.NoError(t, err)
	src := session.New(session.Options{Store: store, Log: zerolog.Nop()})
	_, err = signedInAccount(src)()
	require.ErrorIs(t, err, voiceworker.ErrSignInPending)
}
