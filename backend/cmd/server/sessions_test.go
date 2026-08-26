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

// A device that has been signed out cannot sign anybody else out.
//
// Found by hand. Access tokens are stateless and stay valid for up to fifteen minutes after their session
// is revoked (§17.10), which is the accepted cost of not doing a database lookup on every authenticated
// request. Reading a profile inside that window is what the trade buys. *Undoing a revocation* inside it is
// not: a device signed out by "sign out everywhere else" could spend its remaining minutes calling the same
// endpoint and signing out the device that signed it out, taking the account's API tokens with it each
// time. The owner wins eventually — only they can authenticate afresh — but the operation whose entire
// promise is "this took effect" would have quietly not.
//
// The check costs one indexed lookup on an endpoint called approximately never, and nothing at all on the
// path §17.10 is actually about.
func TestASignedOutDeviceCannotSignOutTheDeviceThatSignedItOut(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	laptop := a.newAccount("ada", "ada@example.com", "laptop")
	phone := a.login("ada@example.com", "phone")

	out := a.call(http.MethodPost, "/api/v1/auth/logout/all", nil, withToken(laptop.Tokens.AccessToken))
	require.Equal(t, http.StatusOK, out.Code, out)

	// The phone's access token is still cryptographically valid, and still reads.
	me := a.call(http.MethodGet, "/api/v1/users/@me", nil, withToken(phone.AccessToken))
	require.Equal(t, http.StatusOK, me.Code, "the residual window is real and accepted")

	// But it may not revoke.
	revenge := a.call(http.MethodPost, "/api/v1/auth/logout/all", nil, withToken(phone.AccessToken))
	assert.Equal(t, http.StatusUnauthorized, revenge.Code,
		"a signed-out device must not be able to undo the sign-out")
	assert.Equal(t, "unauthorized", revenge.errorBody().Code)

	kept := a.call(http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": laptop.Tokens.RefreshToken})
	assert.Equal(t, http.StatusOK, kept.Code, "and the device that did the signing out is untouched")
}

// The same rule on the single-device endpoint, which can do the same damage one device at a time.
func TestASignedOutDeviceCannotRevokeASession(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	laptop := a.newAccount("ada", "ada@example.com", "laptop")
	phone := a.login("ada@example.com", "phone")

	var laptopID string
	for _, s := range listSessions(t, a, laptop.Tokens.AccessToken) {
		if s.Current {
			laptopID = s.ID
		}
	}
	require.NotEmpty(t, laptopID)

	out := a.call(http.MethodPost, "/api/v1/auth/logout/all", nil, withToken(laptop.Tokens.AccessToken))
	require.Equal(t, http.StatusOK, out.Code, out)

	res := a.call(http.MethodDelete, "/api/v1/users/@me/sessions/"+laptopID, nil, withToken(phone.AccessToken))
	assert.Equal(t, http.StatusUnauthorized, res.Code)

	kept := a.call(http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": laptop.Tokens.RefreshToken})
	assert.Equal(t, http.StatusOK, kept.Code, "the laptop survives the attempt")
}

// The check must not fire on the ordinary case it sits next to: a caller whose session was *rotated* is
// live, not signed out, and rotation revokes the row the access token names. Conflating the two would break
// sign-out-everywhere-else for every recently-refreshed client — which is TestLoggingOutEverywhereElse-
// WorksAfterARotation's territory, asserted here from the other direction.
func TestARotatedDeviceIsNotASignedOutOne(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	acct := a.newAccount("ada", "ada@example.com", "laptop")

	var rotated tokenPair
	res := a.call(http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": acct.Tokens.RefreshToken})
	require.Equal(t, http.StatusOK, res.Code, res)
	res.decode(&rotated)

	// The pre-rotation access token names a revoked row, and must still be allowed to revoke.
	out := a.call(http.MethodPost, "/api/v1/auth/logout/all", nil, withToken(acct.Tokens.AccessToken))
	assert.Equal(t, http.StatusOK, out.Code, "a rotation is not a sign-out")

	kept := a.call(http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": rotated.RefreshToken})
	assert.Equal(t, http.StatusOK, kept.Code)
}

// The rule is "a signed-out credential may not change the account's security state", and the endpoint it
// was first written without is the one that matters most.
//
// M11 shipped the check as two per-handler calls, on logout/all and the session revoke. POST /auth/tokens
// was missed — and an API token is not session-scoped, so one minted inside the residual window outlives
// the sign-out permanently. That turns the window from the spite ADR 0030 called it into durable
// persistence: the thing "sign out everywhere else" is reached for precisely to prevent.
//
// Found by review. The guard is one middleware now, and this walks every route under it.
func TestASignedOutDeviceCannotChangeSecurityState(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	owner := a.newAccount("ada", "ada@example.com", "laptop")
	stolen := a.login("ada@example.com", "phone")

	// Something for the revoke-token route to aim at, minted while the phone was still live.
	minted := a.call(http.MethodPost, "/api/v1/auth/tokens",
		map[string]any{"name": "bot", "scopes": []string{"identify"}}, withToken(stolen.AccessToken))
	require.Equal(t, http.StatusCreated, minted.Code, minted)
	var existing struct {
		ID string `json:"id"`
	}
	minted.decode(&existing)

	out := a.call(http.MethodPost, "/api/v1/auth/logout/all", nil, withToken(owner.Tokens.AccessToken))
	require.Equal(t, http.StatusOK, out.Code, out)

	// The access token is still cryptographically valid — that is §17.10, and reading still works.
	require.Equal(t, http.StatusOK,
		a.call(http.MethodGet, "/api/v1/users/@me", nil, withToken(stolen.AccessToken)).Code)
	require.Equal(t, http.StatusOK,
		a.call(http.MethodGet, "/api/v1/users/@me/sessions", nil, withToken(stolen.AccessToken)).Code,
		"listing is a read; the window covers it")

	for name, call := range map[string]func() *response{
		"mint an API token": func() *response {
			return a.call(http.MethodPost, "/api/v1/auth/tokens",
				map[string]any{"name": "persistence", "scopes": []string{"identify"}},
				withToken(stolen.AccessToken))
		},
		"list API tokens": func() *response {
			return a.call(http.MethodGet, "/api/v1/auth/tokens", nil, withToken(stolen.AccessToken))
		},
		"revoke an API token": func() *response {
			return a.call(http.MethodDelete, "/api/v1/auth/tokens/"+existing.ID, nil,
				withToken(stolen.AccessToken))
		},
		"sign out everywhere else": func() *response {
			return a.call(http.MethodPost, "/api/v1/auth/logout/all", nil, withToken(stolen.AccessToken))
		},
		"revoke a session": func() *response {
			return a.call(http.MethodDelete, "/api/v1/users/@me/sessions/350000000000000000", nil,
				withToken(stolen.AccessToken))
		},
	} {
		t.Run(name, func(t *testing.T) {
			res := call()
			assert.Equal(t, http.StatusUnauthorized, res.Code,
				"a signed-out device must not be able to %s", name)
		})
	}
}

// And the guard must not fire on an ordinary rotation, on any route it covers — minting included.
func TestARotatedDeviceCanStillChangeSecurityState(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	acct := a.newAccount("ada", "ada@example.com", "laptop")

	res := a.call(http.MethodPost, "/api/v1/auth/refresh",
		map[string]string{"refresh_token": acct.Tokens.RefreshToken})
	require.Equal(t, http.StatusOK, res.Code, res)

	// The pre-rotation access token names a row the rotation revoked.
	minted := a.call(http.MethodPost, "/api/v1/auth/tokens",
		map[string]any{"name": "bot", "scopes": []string{"identify"}}, withToken(acct.Tokens.AccessToken))
	assert.Equal(t, http.StatusCreated, minted.Code, "a rotation is not a sign-out")

	listed := a.call(http.MethodGet, "/api/v1/auth/tokens", nil, withToken(acct.Tokens.AccessToken))
	assert.Equal(t, http.StatusOK, listed.Code)
}

// first_seen must survive the sweep, which is what 000013 exists for.
//
// It used to be min(created_at) across the family. That is correct until something deletes rows — and
// 000012, one migration earlier in this same milestone, started deleting them. Every row expires
// RefreshTokenTTL after it is created, so no row older than that survives a sweep and the aggregate could
// never report further back than thirty days. A laptop signed in for a year reported a rolling month.
//
// Neither TestASessionListingShowsWhenTheDeviceFirstSignedIn nor any sweep test caught it: one rotates
// without aging anything, the others never read the listing.
func TestFirstSeenSurvivesTheSweep(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	acct := a.newAccount("ada", "ada@example.com", "laptop")

	// Age this device's whole history: a year of rotations, every row long past its expiry.
	_, err := a.pool.Exec(t.Context(), `
		UPDATE sessions
		SET first_seen = now() - interval '365 days',
		    created_at = now() - interval '365 days',
		    expires_at = now() - interval '335 days'`)
	require.NoError(t, err)

	// One live row, as an active device always has, carrying the family's start forward.
	pair := acct.Tokens
	res := a.call(http.MethodPost, "/api/v1/auth/login",
		map[string]any{"email": "ada@example.com", "password": testPassword, "device_id": "laptop"})
	require.Equal(t, http.StatusOK, res.Code, res)
	res.decode(&pair)
	_, err = a.pool.Exec(t.Context(),
		`UPDATE sessions SET first_seen = now() - interval '365 days' WHERE revoked_at IS NULL`)
	require.NoError(t, err)

	before := listSessions(t, a, pair.AccessToken)
	require.Len(t, before, 1)

	swept, err := a.pool.Exec(t.Context(), `DELETE FROM sessions WHERE expires_at < now()`)
	require.NoError(t, err)
	require.Positive(t, swept.RowsAffected(), "the sweep must actually have removed the old rows")

	after := listSessions(t, a, pair.AccessToken)
	require.Len(t, after, 1)
	assert.Equal(t, before[0].FirstSeen, after[0].FirstSeen,
		"the sweep must not move first_seen; it is the family's start, not the oldest surviving row's")
	assert.Less(t, after[0].FirstSeen, time.Now().Add(-300*24*time.Hour),
		"a device used for a year must not report having been first seen this month")
}
