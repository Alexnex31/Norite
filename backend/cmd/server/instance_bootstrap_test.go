package main

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/auth"
)

// Bootstrap over the real router, which is where the properties that matter actually live: the middleware
// that decides who may call it, and the guard that decides whether it still applies.

// operatorToken mints the credential the `norite` CLI will mint, from the same key the test router signs
// with. Built through the exported issuer rather than by hand, so this cannot drift from what the CLI
// produces.
func operatorToken(t *testing.T) string {
	t.Helper()
	issuer, err := auth.NewTokenIssuer([]byte(testJWTSecret))
	require.NoError(t, err)
	token, err := issuer.IssueOperatorToken()
	require.NoError(t, err)
	return token
}

// The milestone's first done-when: a freshly-migrated instance can be given exactly one administrator,
// and the account works like any other afterwards.
func TestBootstrapCreatesAnAdministratorWhoCanLogIn(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)

	resp := a.call(http.MethodPost, "/api/v1/instance/bootstrap", map[string]string{
		"username": "ada",
		"email":    "ada@example.com",
		"password": testPassword,
	}, withToken(operatorToken(t)))
	require.Equal(t, http.StatusCreated, resp.Code, resp)

	var user struct {
		ID              string  `json:"id"`
		Username        string  `json:"username"`
		EmailVerifiedAt *string `json:"email_verified_at"`
	}
	resp.decode(&user)
	assert.Equal(t, "ada", user.Username)

	// Verified on creation, and this is the assertion that keeps it that way: the operator read the
	// instance's signing key off its own disk, which is stronger evidence than an emailed link, and
	// requiring mail would make an instance with no relay impossible to set up.
	require.NotNil(t, user.EmailVerifiedAt, "the bootstrap account must be created already verified")

	// An ordinary account, not a special kind of one.
	a.login("ada@example.com", "device-1")
}

// The guard, and the reason bootstrap needs no flag recording that it already ran. A second call is
// refused whether it comes from a confused operator or from a replayed token somebody found.
func TestBootstrapRefusesASecondAdministrator(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)

	first := a.call(http.MethodPost, "/api/v1/instance/bootstrap", map[string]string{
		"username": "ada", "email": "ada@example.com", "password": testPassword,
	}, withToken(operatorToken(t)))
	require.Equal(t, http.StatusCreated, first.Code, first)

	second := a.call(http.MethodPost, "/api/v1/instance/bootstrap", map[string]string{
		"username": "grace", "email": "grace@example.com", "password": testPassword,
	}, withToken(operatorToken(t)))
	assert.Equal(t, http.StatusConflict, second.Code, second)
	assert.Equal(t, "already_bootstrapped", second.errorBody().Code)

	// And the refused account was not created — a 409 that still left a user behind would be worse than
	// either outcome, since the operator would have no reason to look.
	denied := a.call(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "grace@example.com", "password": testPassword, "device_id": "device-1",
	})
	assert.Equal(t, http.StatusUnauthorized, denied.Code, denied)
}

// The confusion the whole /instance mount arrangement exists to prevent, in the direction that matters.
// Any signed-in user holds an access token signed with the instance key; if one reached this endpoint,
// the first person to register on a fresh instance could make themselves its administrator.
func TestAnAccessTokenCannotBootstrap(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)

	mallory := a.newAccount("mallory", "mallory@example.com", "device-1")

	resp := a.call(http.MethodPost, "/api/v1/instance/bootstrap", map[string]string{
		"username": "mallory2", "email": "mallory2@example.com", "password": testPassword,
	}, withToken(mallory.Tokens.AccessToken))
	assert.Equal(t, http.StatusForbidden, resp.Code, resp)
}

// The other direction, and the one the router rather than a `typ` check is responsible for. An operator
// token names no account, so anything that resolved it to an actor would be authenticating a request as
// user zero.
func TestAnOperatorTokenCannotReadAnAccount(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)

	resp := a.call(http.MethodGet, "/api/v1/users/@me", nil, withToken(operatorToken(t)))
	assert.Equal(t, http.StatusUnauthorized, resp.Code, resp)
}

// Possession of *this* instance's signing key is the entire claim. A token minted against another
// instance's config file must buy nothing, which is what makes the operator tier a real boundary rather
// than a format anybody can reproduce.
func TestABootstrapTokenFromAnotherInstanceIsRefused(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)

	elsewhere, err := auth.NewTokenIssuer([]byte("some-other-instances-signing-key-entirely"))
	require.NoError(t, err)
	token, err := elsewhere.IssueOperatorToken()
	require.NoError(t, err)

	resp := a.call(http.MethodPost, "/api/v1/instance/bootstrap", map[string]string{
		"username": "ada", "email": "ada@example.com", "password": testPassword,
	}, withToken(token))
	assert.Equal(t, http.StatusUnauthorized, resp.Code, resp)
}

// No credential at all. Unlike every other group, /instance rejects here rather than deferring to a
// per-route RequireAuth — so this is the test that the group is mounted with its middleware at all.
func TestBootstrapRefusesAnUnauthenticatedCaller(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)

	resp := a.call(http.MethodPost, "/api/v1/instance/bootstrap", map[string]string{
		"username": "ada", "email": "ada@example.com", "password": testPassword,
	})
	assert.Equal(t, http.StatusUnauthorized, resp.Code, resp)
	assert.NotEmpty(t, resp.Header.Get("WWW-Authenticate"), "RFC 9110 requires it on a 401")
}

// An instance admin's *API token* is not the admin. Instance administration is not delegable, for the
// reason token management is not: a credential that can administer the instance can mint itself more.
func TestAnAPITokenCannotBootstrap(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)

	owner := a.newAccount("ada", "ada@example.com", "device-1")
	minted := a.call(http.MethodPost, "/api/v1/auth/tokens", map[string]any{
		"name": "bot", "scopes": []string{"identify"},
	}, withToken(owner.Tokens.AccessToken))
	require.Equal(t, http.StatusCreated, minted.Code, minted)

	var token struct {
		Value string `json:"value"`
	}
	minted.decode(&token)
	require.NotEmpty(t, token.Value)

	resp := a.call(http.MethodPost, "/api/v1/instance/bootstrap", map[string]string{
		"username": "ada2", "email": "ada2@example.com", "password": testPassword,
	}, withToken(token.Value))
	assert.Equal(t, http.StatusUnauthorized, resp.Code, resp)
}
