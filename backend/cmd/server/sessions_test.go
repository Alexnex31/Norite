package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/auth"
)

// M11's user-facing half: seeing what is signed in, and signing it out.

type sessionView struct {
	ID        string    `json:"id"`
	Name      string    `json:"device_name"`
	Address   *string   `json:"ip_address"`
	FirstSeen time.Time `json:"first_seen"`
	LastUsed  time.Time `json:"last_used_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Current   bool      `json:"current"`
}

func listSessions(t *testing.T, a *api, accessToken string) []sessionView {
	t.Helper()
	res := a.call(http.MethodGet, "/api/v1/users/@me/sessions", nil, withToken(accessToken))
	require.Equal(t, http.StatusOK, res.Code, res)

	var out []sessionView
	require.NoError(t, json.Unmarshal(res.Body, &out))
	return out
}

// ---------- listing ----------

// A listed session is a device, not a rotation.
//
// The whole reason this endpoint groups by device: a session row is one generation of a refresh-token
// family, replaced on every refresh. Listing rows would show a new entry every fifteen minutes on an
// active client, and hand out ids that were stale before anybody could act on them.
func TestListedSessionsAreDevicesNotRotations(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	acct := a.newAccount("ada", "ada@example.com", "laptop")

	pair := acct.Tokens
	for range 3 {
		res := a.call(http.MethodPost, "/api/v1/auth/refresh",
			map[string]string{"refresh_token": pair.RefreshToken})
		require.Equal(t, http.StatusOK, res.Code, res)
		res.decode(&pair)
	}

	sessions := listSessions(t, a, pair.AccessToken)
	assert.Len(t, sessions, 1, "three refreshes are one device, not four sessions")
	assert.True(t, sessions[0].Current)
}

// …and the entry says when the device first signed in, not when it last rotated.
func TestASessionListingShowsWhenTheDeviceFirstSignedIn(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	acct := a.newAccount("ada", "ada@example.com", "laptop")

	before := listSessions(t, a, acct.Tokens.AccessToken)
	require.Len(t, before, 1)

	var pair tokenPair
	res := a.call(http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": acct.Tokens.RefreshToken})
	require.Equal(t, http.StatusOK, res.Code, res)
	res.decode(&pair)

	after := listSessions(t, a, pair.AccessToken)
	require.Len(t, after, 1)

	assert.Equal(t, before[0].FirstSeen, after[0].FirstSeen,
		"first_seen belongs to the device; a rotation must not move it")
	assert.NotEqual(t, before[0].ID, after[0].ID,
		"the id names the newest record, which a rotation does replace")
}

// Every device shows up, and exactly one is current.
func TestEveryDeviceIsListedAndOnlyOneIsCurrent(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	acct := a.newAccount("ada", "ada@example.com", "laptop")
	a.login("ada@example.com", "phone")
	a.login("ada@example.com", "tablet")

	sessions := listSessions(t, a, acct.Tokens.AccessToken)
	assert.Len(t, sessions, 3)

	current := 0
	for _, s := range sessions {
		if s.Current {
			current++
		}
	}
	assert.Equal(t, 1, current, "exactly one device is the one that asked")
}

// An API token may not see the account's devices.
func TestAnAPITokenCannotListSessions(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	acct := a.newAccount("ada", "ada@example.com", "laptop")

	res := a.call(http.MethodPost, "/api/v1/auth/tokens",
		map[string]any{"name": "bot", "scopes": []string{"identify"}},
		withToken(acct.Tokens.AccessToken))
	require.Equal(t, http.StatusCreated, res.Code, res)
	var minted struct {
		Value string `json:"value"`
	}
	res.decode(&minted)

	got := a.call(http.MethodGet, "/api/v1/users/@me/sessions", nil, withToken(minted.Value))
	assert.Equal(t, http.StatusForbidden, got.Code,
		"a delegated credential must not enumerate the machines its owner is signed in on")
}

// ---------- revoking one device ----------

func TestRevokingOneDeviceLeavesTheOthers(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	acct := a.newAccount("ada", "ada@example.com", "laptop")
	phone := a.login("ada@example.com", "phone")
	tablet := a.login("ada@example.com", "tablet")

	// Identify the phone by listing from the phone itself: its own entry is the current one, which is the
	// only thing distinguishing three devices that carry the same name.
	var phoneID string
	for _, s := range listSessions(t, a, phone.AccessToken) {
		if s.Current {
			phoneID = s.ID
		}
	}
	require.NotEmpty(t, phoneID)

	res := a.call(http.MethodDelete, "/api/v1/users/@me/sessions/"+phoneID, nil,
		withToken(acct.Tokens.AccessToken))
	require.Equal(t, http.StatusNoContent, res.Code, res)

	refused := a.call(http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": phone.RefreshToken})
	assert.Equal(t, http.StatusUnauthorized, refused.Code, "the phone is signed out")

	for name, tok := range map[string]string{"laptop": acct.Tokens.RefreshToken, "tablet": tablet.RefreshToken} {
		ok := a.call(http.MethodPost, "/api/v1/auth/refresh", map[string]string{"refresh_token": tok})
		assert.Equal(t, http.StatusOK, ok.Code, "%s must be untouched", name)
	}
}

// Another account's session is not found, not forbidden.
//
// The two answers together would let anybody holding a list of snowflakes learn which of them name real
// sessions — and a snowflake carries its own creation time.
func TestRevokingAnotherAccountsSessionIsNotFound(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	ada := a.newAccount("ada", "ada@example.com", "laptop")
	grace := a.newAccount("grace", "grace@example.com", "laptop")

	var graceID string
	for _, s := range listSessions(t, a, grace.Tokens.AccessToken) {
		if s.Current {
			graceID = s.ID
		}
	}
	require.NotEmpty(t, graceID)

	res := a.call(http.MethodDelete, "/api/v1/users/@me/sessions/"+graceID, nil,
		withToken(ada.Tokens.AccessToken))
	assert.Equal(t, http.StatusNotFound, res.Code, res)

	// And it really did nothing.
	ok := a.call(http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": grace.Tokens.RefreshToken})
	assert.Equal(t, http.StatusOK, ok.Code, "grace's session must survive ada's attempt")
}

func TestAMalformedSessionIDIsNotFound(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	acct := a.newAccount("ada", "ada@example.com", "laptop")

	res := a.call(http.MethodDelete, "/api/v1/users/@me/sessions/not-a-snowflake", nil,
		withToken(acct.Tokens.AccessToken))
	assert.Equal(t, http.StatusNotFound, res.Code,
		"malformed, absent and unowned must be one answer, or the endpoint probes which ids are real")
}

// ---------- signing out everywhere else ----------

func TestLoggingOutEverywhereElseKeepsThisDevice(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	acct := a.newAccount("ada", "ada@example.com", "laptop")
	phone := a.login("ada@example.com", "phone")
	tablet := a.login("ada@example.com", "tablet")

	minted := a.call(http.MethodPost, "/api/v1/auth/tokens",
		map[string]any{"name": "bot", "scopes": []string{"identify"}},
		withToken(acct.Tokens.AccessToken))
	require.Equal(t, http.StatusCreated, minted.Code, minted)
	var bot struct {
		Value string `json:"value"`
	}
	minted.decode(&bot)

	res := a.call(http.MethodPost, "/api/v1/auth/logout/all", nil, withToken(acct.Tokens.AccessToken))
	require.Equal(t, http.StatusOK, res.Code, res)

	var summary struct {
		Sessions  int64 `json:"sessions_revoked"`
		APITokens int64 `json:"api_tokens_revoked"`
	}
	res.decode(&summary)
	assert.EqualValues(t, 2, summary.Sessions, "the phone and the tablet")
	assert.EqualValues(t, 1, summary.APITokens, "and the bot, which is the part worth reporting")

	for name, tok := range map[string]string{"phone": phone.RefreshToken, "tablet": tablet.RefreshToken} {
		gone := a.call(http.MethodPost, "/api/v1/auth/refresh", map[string]string{"refresh_token": tok})
		assert.Equal(t, http.StatusUnauthorized, gone.Code, "%s should be signed out", name)
	}

	kept := a.call(http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": acct.Tokens.RefreshToken})
	assert.Equal(t, http.StatusOK, kept.Code, "the calling device keeps working")

	dead := a.call(http.MethodGet, "/api/v1/users/@me", nil, withToken(bot.Value))
	assert.Equal(t, http.StatusUnauthorized, dead.Code, "API tokens go with the sessions")
}

// The milestone's sharp edge.
//
// An access token names the session it was minted from and lives fifteen minutes. Any refresh inside that
// window revokes the named row while leaving the token valid — so "which device am I" has to be resolvable
// from a *revoked* record. Read through a query that hid revoked rows, this endpoint would find no current
// device, spare nothing, and sign the caller out of itself: the one thing its name promises it will not do.
func TestLoggingOutEverywhereElseWorksAfterARotation(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	acct := a.newAccount("ada", "ada@example.com", "laptop")
	phone := a.login("ada@example.com", "phone")

	// Rotate the laptop. The access token from before the rotation is still valid and still names the row
	// the rotation just revoked, which is exactly the state a real client is in for up to fifteen minutes.
	var rotated tokenPair
	res := a.call(http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": acct.Tokens.RefreshToken})
	require.Equal(t, http.StatusOK, res.Code, res)
	res.decode(&rotated)

	out := a.call(http.MethodPost, "/api/v1/auth/logout/all", nil, withToken(acct.Tokens.AccessToken))
	require.Equal(t, http.StatusOK, out.Code, out)

	kept := a.call(http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": rotated.RefreshToken})
	assert.Equal(t, http.StatusOK, kept.Code,
		"the caller signed itself out: the current session was resolved through a revoked row")

	gone := a.call(http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": phone.RefreshToken})
	assert.Equal(t, http.StatusUnauthorized, gone.Code, "and the other device still went")
}

func TestAnAPITokenCannotRevokeItsOwnersSessions(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	acct := a.newAccount("ada", "ada@example.com", "laptop")

	minted := a.call(http.MethodPost, "/api/v1/auth/tokens",
		map[string]any{"name": "bot", "scopes": []string{"identify"}},
		withToken(acct.Tokens.AccessToken))
	require.Equal(t, http.StatusCreated, minted.Code, minted)
	var bot struct {
		Value string `json:"value"`
	}
	minted.decode(&bot)

	res := a.call(http.MethodPost, "/api/v1/auth/logout/all", nil, withToken(bot.Value))
	assert.Equal(t, http.StatusForbidden, res.Code,
		"a credential that can revoke its owner's sessions and tokens can lock its owner out")

	ok := a.call(http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": acct.Tokens.RefreshToken})
	assert.Equal(t, http.StatusOK, ok.Code, "and it changed nothing")
}

// Signing out everywhere else on an account with nowhere else is a no-op, not an error.
func TestLoggingOutEverywhereElseWithNoOtherDevices(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	acct := a.newAccount("ada", "ada@example.com", "laptop")

	res := a.call(http.MethodPost, "/api/v1/auth/logout/all", nil, withToken(acct.Tokens.AccessToken))
	require.Equal(t, http.StatusOK, res.Code, res)

	var summary struct {
		Sessions int64 `json:"sessions_revoked"`
	}
	res.decode(&summary)
	assert.Zero(t, summary.Sessions)

	ok := a.call(http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": acct.Tokens.RefreshToken})
	assert.Equal(t, http.StatusOK, ok.Code)
}
