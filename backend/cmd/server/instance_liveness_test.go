package main

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/auth"
)

// M11 drew the liveness rule around the *account's* security state and mounted RequireLiveSession on the
// endpoints that change it. An instance invite is not the account's security state, so /instance fell
// outside the wording — while POST /instance/invites mints a code that can carry unlimited uses and no
// expiry, which is the same durable persistence the rule exists for, one level up.
//
// These tests pin both halves: the guard now covers /instance, and it did not get there by the obvious
// route, which would have broken the operator.

// signedOutAdmin bootstraps an administrator, signs it in on two devices, and signs the first one out from
// the second. The returned token is the first device's access token: cryptographically valid, naming a
// session that no longer is.
func signedOutAdmin(t *testing.T, a *api) string {
	t.Helper()

	created := a.call(http.MethodPost, "/api/v1/instance/bootstrap", map[string]string{
		"username": "ada", "email": "ada@example.com", "password": testPassword,
	}, withToken(operatorToken(t)))
	require.Equal(t, http.StatusCreated, created.Code, created)

	stale := a.login("ada@example.com", "device-1")
	keeper := a.login("ada@example.com", "device-2")

	out := a.call(http.MethodPost, "/api/v1/auth/logout/all", nil, withToken(keeper.AccessToken))
	require.Equal(t, http.StatusOK, out.Code, out)

	return stale.AccessToken
}

// The finding, stated as the scenario it actually matters in.
//
// An administrator who believes their account is compromised reaches for "sign out everywhere else". The
// intruder's access token stays valid for up to fifteen minutes (§17.10), and before this it could spend
// them minting an unlimited-use invite — a credential that outlives the sign-out permanently and lets its
// holder create accounts on a gated instance. The same window opens under M72's ban.
func TestASignedOutAdministratorCannotMintAnInvite(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	stale := signedOutAdmin(t, a)

	resp := a.call(http.MethodPost, "/api/v1/instance/invites", map[string]any{"max_uses": 0},
		withToken(stale))
	assert.Equal(t, http.StatusUnauthorized, resp.Code,
		"a signed-out administrator must not mint an invite: %s", resp)
}

// The whole surface, walked rather than listed.
//
// This is the property M11 got wrong twice — first as three per-handler checks that missed POST
// /auth/tokens, then as a middleware whose coverage stopped at the group it was mounted on. A list of
// routes to check is a list somebody has to remember to extend. Walking the router instead means a new
// /instance route is covered the moment it is mounted, and fails here if it is not.
func TestEveryInstanceRouteRefusesASignedOutSession(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	stale := signedOutAdmin(t, a)

	var instanceRoutes []string
	for op := range routedOperations(t, a.handler) {
		if strings.Contains(op, " /api/v1/instance") {
			instanceRoutes = append(instanceRoutes, op)
		}
	}
	sort.Strings(instanceRoutes)
	require.NotEmpty(t, instanceRoutes, "the walk must find the /instance routes, or it is asserting nothing")

	for _, op := range instanceRoutes {
		method, path, _ := strings.Cut(op, " ")
		resp := a.call(method, path, nil, withToken(stale))
		assert.Equal(t, http.StatusUnauthorized, resp.Code,
			"%s must refuse a signed-out session: %s", op, resp)
	}
}

// And the regression guard for the fix that was not taken.
//
// The obvious reading is to mount RequireLiveSession on the /instance group. That middleware's first act
// is ActorFrom, and the operator branch of AuthenticateInstanceAdmin deliberately carries no Actor at all —
// an operator names no account. So mounting it there answers 401 to every operator request and takes
// `norite instance bootstrap` with it, on the one path that has to work when the instance has no accounts
// and therefore no sessions to be live. The check lives inside the account branch for exactly this reason.
func TestTheOperatorIsUnaffectedByTheLivenessCheck(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)

	resp := a.call(http.MethodPost, "/api/v1/instance/invites", map[string]any{"max_uses": 2},
		withToken(operatorToken(t)))
	assert.Equal(t, http.StatusCreated, resp.Code,
		"an operator holds no session, and must not need one: %s", resp)
}
