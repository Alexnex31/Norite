// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package login

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registerInstance stands in for POST /api/v1/auth/register: it records what it was sent and answers with a
// scripted status and body.
type registerInstance struct {
	server *httptest.Server
	got    []registerRequest
	status int
	body   any
}

func newRegisterInstance(t *testing.T) *registerInstance {
	t.Helper()
	f := &registerInstance{status: http.StatusAccepted,
		body: map[string]string{"message": "Check your email to confirm the address."}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/register" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var req registerRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.got = append(f.got, req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_ = json.NewEncoder(w).Encode(f.body)
	}))
	t.Cleanup(f.server.Close)
	return f
}

// registrar builds one with scripted answers: lines for ReadLine, secrets for ReadSecret.
func registrar(f *registerInstance, opts RegisterOptions, interactive bool, lines, secrets []string,
) (*Registrar, *bytes.Buffer) {
	var out bytes.Buffer
	if opts.Instance == "" {
		opts.Instance = f.server.URL
	}
	next := func(q *[]string) func(string) (string, error) {
		return func(string) (string, error) {
			if len(*q) == 0 {
				return "", errors.New("asked more than the test answered")
			}
			v := (*q)[0]
			*q = (*q)[1:]
			return v, nil
		}
	}
	return &Registrar{Options: opts, Out: &out, ReadLine: next(&lines), ReadSecret: next(&secrets),
		Interactive: interactive}, &out
}

// TestRegisterSendsTheAccountAndSaysHowToSignIn: what was typed reaches the instance, the instance's
// sentence is printed sanitized, and the next command is the login that uses the account.
func TestRegisterSendsTheAccountAndSaysHowToSignIn(t *testing.T) {
	t.Setenv(passwordEnvVar, "")
	f := newRegisterInstance(t)
	f.body = map[string]string{"message": "Ready\x1b[2J; sign in now."}
	r, out := registrar(f, RegisterOptions{DisplayName: "Bob B."}, true,
		[]string{"bob", "bob@example.com"}, []string{"correct horse battery", "correct horse battery"})

	require.NoError(t, r.Run(context.Background()))
	require.Len(t, f.got, 1)
	assert.Equal(t, registerRequest{Username: "bob", Email: "bob@example.com", Password: "correct horse battery",
		DisplayName: "Bob B."}, f.got[0])
	assert.Contains(t, out.String(), "Ready\uFFFD[2J; sign in now.", "the instance's words, made inert")
	assert.NotContains(t, out.String(), "\x1b")
	assert.Contains(t, out.String(), "norite login --instance "+f.server.URL+" --email bob@example.com")
	assert.NotContains(t, out.String(), "correct horse battery", "the password is never printed")
}

// TestTwoDifferentPasswordsSendNothing: the second entry is what catches a typo nobody could recover from.
func TestTwoDifferentPasswordsSendNothing(t *testing.T) {
	t.Setenv(passwordEnvVar, "")
	f := newRegisterInstance(t)
	r, _ := registrar(f, RegisterOptions{Username: "bob", Email: "bob@example.com"}, true, nil,
		[]string{"correct horse battery", "correct horse batery"})

	err := r.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "differ")
	assert.Empty(t, f.got)
}

// TestAScriptedRegistrationTakesThePasswordFromTheEnvironment, and asks for nothing.
func TestAScriptedRegistrationTakesThePasswordFromTheEnvironment(t *testing.T) {
	t.Setenv(passwordEnvVar, "a passphrase from the environment")
	f := newRegisterInstance(t)
	r, _ := registrar(f, RegisterOptions{Username: "bob", Email: "bob@example.com"}, false, nil, nil)

	require.NoError(t, r.Run(context.Background()))
	require.Len(t, f.got, 1)
	assert.Equal(t, "a passphrase from the environment", f.got[0].Password)
}

// TestWithoutATerminalEachMissingAnswerNamesItsFlag: no blocking on input nobody can type.
func TestWithoutATerminalEachMissingAnswerNamesItsFlag(t *testing.T) {
	t.Setenv(passwordEnvVar, "")
	f := newRegisterInstance(t)
	for _, tc := range []struct {
		opts RegisterOptions
		want string
	}{
		{RegisterOptions{}, "--username"},
		{RegisterOptions{Username: "bob"}, "--email"},
		{RegisterOptions{Username: "bob", Email: "bob@example.com"}, passwordEnvVar},
	} {
		r, _ := registrar(f, tc.opts, false, nil, nil)
		err := r.Run(context.Background())
		require.ErrorIs(t, err, ErrNoTerminal)
		assert.Contains(t, err.Error(), tc.want)
	}
	assert.Empty(t, f.got)
}

// TestAnInviteOnlyInstanceSaysWhereACodeComesFrom: the instance cannot know this client's flag or who mints
// codes, so its refusal is the one reworded; a code given is sent.
func TestAnInviteOnlyInstanceSaysWhereACodeComesFrom(t *testing.T) {
	t.Setenv(passwordEnvVar, "a passphrase from the environment")
	f := newRegisterInstance(t)
	f.status = http.StatusForbidden
	f.body = map[string]any{"error": map[string]string{"code": "invite_required",
		"message": "this instance requires an invite code to register", "request_id": "r1"}}
	r, _ := registrar(f, RegisterOptions{Username: "bob", Email: "bob@example.com"}, false, nil, nil)

	err := r.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--invite-code")
	assert.Contains(t, err.Error(), "norite instance invite create")

	f.status, f.body = http.StatusAccepted, map[string]string{"message": "ok"}
	r, _ = registrar(f, RegisterOptions{Username: "bob", Email: "bob@example.com", InviteCode: " ABCD-EFGH "},
		false, nil, nil)
	require.NoError(t, r.Run(context.Background()))
	assert.Equal(t, "ABCD-EFGH", f.got[len(f.got)-1].InviteCode)
}

// TestATakenUsernameIsTheInstancesAnswer: every other refusal is the instance's own words.
func TestATakenUsernameIsTheInstancesAnswer(t *testing.T) {
	t.Setenv(passwordEnvVar, "a passphrase from the environment")
	f := newRegisterInstance(t)
	f.status = http.StatusConflict
	f.body = map[string]any{"error": map[string]string{"code": "username_taken",
		"message": "that username is taken", "request_id": "r1"}}
	r, _ := registrar(f, RegisterOptions{Username: "alice", Email: "bob@example.com"}, false, nil, nil)

	err := r.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "that username is taken")
}
