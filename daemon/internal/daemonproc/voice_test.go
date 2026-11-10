// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonproc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/credentials"
	"github.com/Alexnex31/Norite/daemon/internal/session"
	"github.com/Alexnex31/Norite/daemon/internal/voiceworker"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/voiceipc"
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
		Account: func() (voiceworker.SignIn, error) {
			if !signedIn {
				return voiceworker.SignIn{}, voiceworker.ErrSignedOut
			}
			return voiceworker.SignIn{Account: "42"}, nil
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

// countingSignaler refuses every call and counts the routes asked of it: a join that got that far.
type countingSignaler struct{ routes atomic.Int32 }

func (c *countingSignaler) Route(context.Context, string) (voiceworker.Route, error) {
	c.routes.Add(1)
	return voiceworker.Route{}, errRefusing
}

func (*countingSignaler) Join(context.Context, string, string, string) (string, error) {
	return "", errRefusing
}

// The join's body is the client's settings (M30): no body is every switch on, and anything else names all
// three and nothing more, or the join is refused before it begins. A switch left out would read as off.
func TestAJoinsSettingsAreDecodedStrictly(t *testing.T) {
	for _, body := range []string{"", "null", `{"echo_cancellation":true,"noise_suppression":false,"auto_gain":true}`} {
		if body != "" && body != "null" {
			matchesDefinition(t, "VoiceJoin", []byte(body))
		}
		srv := &countingSignaler{}
		l := voiceFixture(srv, true)
		resp := l.Do(context.Background(), ipc.Request{Method: http.MethodPost, Path: ipc.PathVoiceJoin("1"),
			Body: json.RawMessage(body)})
		require.NotNil(t, resp.Error, body)
		require.Equal(t, ipc.RelayFailed, resp.Error.Code, "%s: the join went ahead, to the voice server", body)
		require.Equal(t, int32(1), srv.routes.Load(), body)
		// A call that failed takes its settings with it.
		var st ipc.VoiceStatus
		require.NoError(t, json.Unmarshal(l.Do(context.Background(),
			ipc.Request{Method: http.MethodGet, Path: ipc.PathVoice}).Body, &st))
		require.Equal(t, ipc.VoiceIdle, st.State)
		require.Nil(t, st.Processing, body)
	}
	for _, body := range []string{
		`{"echo_cancellation":true,"noise_suppression":false}`,
		`{"echo_cancellation":true,"noise_suppression":false,"auto_gain":true,"mute":true}`,
		`{"echo_cancellation":"on","noise_suppression":false,"auto_gain":true}`,
		`{"echo_cancellation":true,"noise_suppression":false,"auto_gain":true} {}`,
		`[]`, `true`,
	} {
		srv := &countingSignaler{}
		l := voiceFixture(srv, true)
		resp := l.Do(context.Background(), ipc.Request{Method: http.MethodPost, Path: ipc.PathVoiceJoin("1"),
			Body: json.RawMessage(body)})
		require.NotNil(t, resp.Error, body)
		require.Equal(t, ipc.RelayBadRequest, resp.Error.Code, body)
		require.Zero(t, srv.routes.Load(), "%s: refused after the join began", body)
		require.False(t, l.voice.Busy())
	}
}

// The settings the body named are the call's, as its state reports them while the join is under way, and
// the call's alone: they are gone once it has ended.
func TestAJoinsSettingsAreTheCalls(t *testing.T) {
	l := voiceFixture(holdingSignaler{}, true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = l.Do(ctx, ipc.Request{Method: http.MethodPost, Path: ipc.PathVoiceJoin("1"),
			Body: json.RawMessage(`{"echo_cancellation":false,"noise_suppression":true,"auto_gain":false}`)})
	}()
	var st ipc.VoiceStatus
	require.Eventually(t, func() bool {
		resp := l.Do(context.Background(), ipc.Request{Method: http.MethodGet, Path: ipc.PathVoice})
		matchesDefinition(t, "VoiceStatus", resp.Body)
		return json.Unmarshal(resp.Body, &st) == nil && st.State == ipc.VoiceJoining
	}, 5*time.Second, 5*time.Millisecond)
	require.Equal(t, &ipc.VoiceJoin{NoiseSuppression: true}, st.Processing)
	l.voice.Leave()
	cancel()
	<-done
	resp := l.Do(context.Background(), ipc.Request{Method: http.MethodGet, Path: ipc.PathVoice})
	matchesDefinition(t, "VoiceStatus", resp.Body)
	require.NoError(t, json.Unmarshal(resp.Body, &st))
	require.Nil(t, st.Processing)
}

// What each body decodes to: no body is every switch on, and the rest is what it says.
func TestAJoinsSettingsAreWhatTheBodySays(t *testing.T) {
	for body, want := range map[string]ipc.VoiceJoin{
		"":     {EchoCancellation: true, NoiseSuppression: true, AutoGain: true},
		"null": {EchoCancellation: true, NoiseSuppression: true, AutoGain: true},
		` {"echo_cancellation":false,"noise_suppression":true,"auto_gain":false} `: {NoiseSuppression: true},
		`{"echo_cancellation":false,"noise_suppression":false,"auto_gain":false}`:  {},
	} {
		got, err := ipc.DecodeVoiceJoin(json.RawMessage(body))
		require.NoError(t, err, body)
		require.Equal(t, want, got, body)
	}
}

// A sign-in not read yet answers "unavailable", never "log in", which would replace a good sign-in.
func TestAJoinBeforeTheSignInIsReadIsUnavailable(t *testing.T) {
	l := &localRequests{log: zerolog.Nop(), voice: voiceworker.NewVoice(voiceworker.VoiceOptions{
		Signaler: refusingSignaler{}, Log: zerolog.Nop(),
		Account: func() (voiceworker.SignIn, error) { return voiceworker.SignIn{}, voiceworker.ErrSignInPending },
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

// A sign-in that ends takes its call with it (M19's rule), from the session itself: here no gateway
// connection exists at all, as when the instance is unreachable, and a logout still ends the call, which the
// gateway's own end of a sign-in did not until the next one.
func TestASignInThatEndsEndsTheCallWithNoGateway(t *testing.T) {
	instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/refresh" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "eyJ.access", "refresh_token": "nrt_rotated", "token_type": "Bearer",
			"expires_at": time.Now().Add(15 * time.Minute).Format(time.RFC3339Nano),
		})
	}))
	t.Cleanup(instance.Close)
	store, err := credentials.OpenLocalForTest(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.Save(credentials.Record{InstanceURL: instance.URL, UserID: "1", Username: "ada",
		DeviceID: "dev_test", DeviceName: "laptop"}, "nrt_from_login"))
	src := session.New(session.Options{Store: store, Log: zerolog.Nop()})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go src.Run(ctx)
	require.Eventually(t, func() bool { st, _ := src.Status(); return st == session.Live }, 5*time.Second,
		5*time.Millisecond)

	calls := voiceworker.NewVoice(voiceworker.VoiceOptions{Signaler: holdingSignaler{}, Log: zerolog.Nop(),
		Account: signedInAccount(src)})
	go func() { _, _ = calls.Join(context.Background(), "1", voiceipc.AllProcessing()) }()
	require.Eventually(t, calls.Busy, 5*time.Second, 5*time.Millisecond)

	require.NoError(t, store.Clear()) // `norite logout`
	src.Reload()
	require.Eventually(t, func() bool { return !calls.Busy() }, 5*time.Second, 5*time.Millisecond)
	require.Equal(t, ipc.VoiceStatus{State: ipc.VoiceIdle, Reason: "the sign-in ended", Available: true},
		calls.Status())
}

// A session that has not read its store yet is pending, not signed out.
func TestASignInNotReadYetIsPending(t *testing.T) {
	store, err := credentials.OpenIn(t.TempDir())
	require.NoError(t, err)
	src := session.New(session.Options{Store: store, Log: zerolog.Nop()})
	_, err = signedInAccount(src)()
	require.ErrorIs(t, err, voiceworker.ErrSignInPending)
}
