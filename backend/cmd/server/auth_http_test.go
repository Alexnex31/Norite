package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/database"
	"github.com/Alexnex31/Norite/backend/internal/platform/dbtest"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/migrations"
)

// These tests drive the assembled router — the real middleware chain, the real auth service, a real
// Postgres — with httptest, rather than calling handlers directly.
//
// # Why at this level
//
// internal/auth's own tests cover the service: rotation, reuse detection, device scoping, argon2id. What
// they cannot see is everything the *router* decides, and that is where the security-relevant mistakes
// live. A handler tested in isolation passes identically whether it is mounted behind RequireAuth or not.
// So does one mounted behind RequireScope instead of RequireUserActor. Both are one line in router
// assembly, neither is visible from the handler, and both are the difference between a protected endpoint
// and a public one.
//
// The guards in internal/auth/middleware.go had no test of any kind before this file; the three of them
// decide who reaches every protected endpoint in the API, now and for every milestone after this one.
//
// Everything here needs a container and is skipped by `go test -short`.

// testJWTSecret signs access tokens in these tests. Its length is what matters — the issuer enforces a
// floor — not its value.
const testJWTSecret = "test-signing-key-of-at-least-32-bytes"

const testPassword = "correct horse battery staple"

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// api is the assembled server under test.
type api struct {
	t       *testing.T
	handler http.Handler
	pool    *pgxpool.Pool
}

// newAPI builds the real router over a freshly-migrated database.
//
// The wiring mirrors run() in main.go deliberately: if the composition root starts mounting things
// differently, these tests should stop reflecting production and that should be a visible edit here.
func newAPI(t *testing.T, mode auth.RegistrationMode) *api {
	t.Helper()
	dbtest.RequireContainer(t)

	ctx := t.Context()
	dsn := dbtest.FreshDatabase(t)

	require.NoError(t, database.Migrate(ctx, database.MigrateOptions{
		DatabaseURL: dsn,
		Source:      migrations.FS,
		SourceDir:   ".",
		LockTimeout: 30 * time.Second,
	}))

	pool, err := database.New(ctx, database.PoolOptions{
		DatabaseURL:    dsn,
		MaxConns:       4,
		MinConns:       1,
		ConnectTimeout: 10 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	ids, err := snowflake.NewGenerator(0)
	require.NoError(t, err)

	issuer, err := auth.NewTokenIssuer([]byte(testJWTSecret))
	require.NoError(t, err)

	svc, err := auth.NewService(auth.ServiceOptions{
		Pool:             pool,
		IDs:              ids,
		Issuer:           issuer,
		RegistrationMode: mode,
	})
	require.NoError(t, err)

	health := newHealth(db.New(pool))
	health.MarkReady()

	handler, err := newRouter(routerOptions{
		Config:  testConfig(),
		Logger:  zerolog.New(io.Discard),
		Health:  health,
		Auth:    auth.NewHandler(svc),
		AuthSvc: svc,
	})
	require.NoError(t, err)

	return &api{t: t, handler: handler, pool: pool}
}

// reqOpt adjusts a request before it is served.
type reqOpt func(*http.Request)

func withToken(token string) reqOpt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func withHeader(name, value string) reqOpt {
	return func(r *http.Request) { r.Header.Set(name, value) }
}

// fromIP fixes the client address, which is the rate limiter's grouping key.
func fromIP(addr string) reqOpt {
	return func(r *http.Request) { r.RemoteAddr = addr + ":40000" }
}

// call sends a request through the whole chain and records the response.
//
// A string body is sent verbatim so malformed-JSON cases can be expressed; anything else is marshaled.
func (a *api) call(method, path string, body any, opts ...reqOpt) *response {
	a.t.Helper()

	var reader io.Reader
	if body != nil {
		if raw, ok := body.(string); ok {
			reader = strings.NewReader(raw)
		} else {
			encoded, err := json.Marshal(body)
			require.NoError(a.t, err)
			reader = bytes.NewReader(encoded)
		}
	}

	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, opt := range opts {
		opt(req)
	}

	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)

	return &response{t: a.t, Code: rec.Code, Header: rec.Header(), Body: rec.Body.Bytes()}
}

type response struct {
	t      *testing.T
	Code   int
	Header http.Header
	Body   []byte
}

func (r *response) String() string { return string(r.Body) }

func (r *response) decode(dst any) {
	r.t.Helper()
	require.NoError(r.t, json.Unmarshal(r.Body, dst), "decoding response body: %s", r.Body)
}

// errorBody returns the error envelope. Clients key off Code, never off Message.
func (r *response) errorBody() httpx.ErrorBody {
	r.t.Helper()
	var envelope struct {
		Error httpx.ErrorBody `json:"error"`
	}
	r.decode(&envelope)
	return envelope.Error
}

type tokenPair struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// account is a registered, logged-in user.
type account struct {
	ID     string
	Email  string
	Tokens tokenPair
}

// newAccount registers and logs in, which is the two-request preamble most of these tests need.
func (a *api) newAccount(username, email, deviceID string) account {
	a.t.Helper()

	created := a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": username,
		"email":    email,
		"password": testPassword,
	})
	require.Equal(a.t, http.StatusCreated, created.Code, "register: %s", created)

	var user struct {
		ID string `json:"id"`
	}
	created.decode(&user)

	return account{ID: user.ID, Email: email, Tokens: a.login(email, deviceID)}
}

func (a *api) login(email, deviceID string) tokenPair {
	a.t.Helper()

	resp := a.call(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email":     email,
		"password":  testPassword,
		"device_id": deviceID,
	})
	require.Equal(a.t, http.StatusOK, resp.Code, "login: %s", resp)

	var pair tokenPair
	resp.decode(&pair)
	return pair
}

// protectedRoutes is every M4 endpoint that must refuse an unauthenticated caller.
//
// Table-driven on purpose: a route added to the router without being added here still passes, but a route
// added here without a guard fails immediately — and the failure names the route.
var protectedRoutes = []struct {
	name   string
	method string
	path   string
	body   any
}{
	{"current user", http.MethodGet, "/api/v1/users/@me", nil},
	{"mint token", http.MethodPost, "/api/v1/auth/tokens", map[string]any{"name": "bot", "scopes": []string{"identify"}}},
	{"list tokens", http.MethodGet, "/api/v1/auth/tokens", nil},
	{"revoke token", http.MethodDelete, "/api/v1/auth/tokens/1234567890", nil},
}

// ---------------------------------------------------------------------------
// the happy path, end to end over HTTP
// ---------------------------------------------------------------------------

func TestAuthLifecycleOverHTTP(t *testing.T) {
	api := newAPI(t, auth.RegistrationOpen)

	created := api.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username":     "ada",
		"email":        "ada@example.com",
		"password":     testPassword,
		"display_name": "Ada L.",
	})
	require.Equal(t, http.StatusCreated, created.Code, created)

	var fields map[string]any
	created.decode(&fields)
	assert.Equal(t, "ada", fields["username"])
	assert.Equal(t, "Ada L.", fields["display_name"])
	// Snowflakes cross the wire as quoted strings. A JSON number would exceed 2^53 and lose precision in
	// any JavaScript client, so this is a contract property, not a formatting preference.
	assert.IsType(t, "", fields["id"], "id must be a JSON string, not a number: %s", created)

	pair := api.login("ada@example.com", "laptop")
	assert.Equal(t, "Bearer", pair.TokenType)
	assert.WithinDuration(t, time.Now().Add(auth.AccessTokenTTL), pair.ExpiresAt, time.Minute)

	// The access token authenticates, and resolves to the account that logged in.
	me := api.call(http.MethodGet, "/api/v1/users/@me", nil, withToken(pair.AccessToken))
	require.Equal(t, http.StatusOK, me.Code, me)
	var self map[string]any
	me.decode(&self)
	assert.Equal(t, "ada@example.com", self["email"])
	assert.Equal(t, fields["id"], self["id"])

	// Refresh takes the token in the body and needs no Authorization header — a client whose access token
	// has already expired must still be able to refresh.
	refreshed := api.call(http.MethodPost, "/api/v1/auth/refresh", map[string]string{
		"refresh_token": pair.RefreshToken,
	})
	require.Equal(t, http.StatusOK, refreshed.Code, refreshed)

	var next tokenPair
	refreshed.decode(&next)
	assert.NotEqual(t, pair.RefreshToken, next.RefreshToken, "refresh must rotate the token, not reissue it")
	assert.NotEmpty(t, next.AccessToken)

	// The rotated-to token is the live one, and logging out with it ends the session.
	loggedOut := api.call(http.MethodPost, "/api/v1/auth/logout", map[string]string{
		"refresh_token": next.RefreshToken,
	})
	require.Equal(t, http.StatusNoContent, loggedOut.Code, loggedOut)
	assert.Empty(t, loggedOut.Body, "204 carries no body")

	after := api.call(http.MethodPost, "/api/v1/auth/refresh", map[string]string{
		"refresh_token": next.RefreshToken,
	})
	assert.Equal(t, http.StatusUnauthorized, after.Code, "a logged-out session must not refresh")
}

// ---------------------------------------------------------------------------
// the guards
// ---------------------------------------------------------------------------

func TestProtectedRoutesRejectAnUnauthenticatedRequest(t *testing.T) {
	api := newAPI(t, auth.RegistrationOpen)

	for _, route := range protectedRoutes {
		t.Run(route.name, func(t *testing.T) {
			resp := api.call(route.method, route.path, route.body)

			require.Equal(t, http.StatusUnauthorized, resp.Code, resp)
			body := resp.errorBody()
			assert.Equal(t, "unauthorized", body.Code)
			assert.NotEmpty(t, body.RequestID, "every error body carries the correlation ID")
			// RFC 9110 requires it on a 401, and it is what tells a client to retry with a credential
			// rather than treat the failure as terminal.
			assert.Equal(t, `Bearer realm="norite"`, resp.Header.Get("WWW-Authenticate"))
		})
	}
}

// An invalid credential is treated as no credential: Authenticate does not reject, it simply records
// nobody, and the route's own guard produces the 401. That is what lets public and protected routes share
// one middleware without an exemption list — so it has to hold for every shape of bad credential.
func TestProtectedRoutesRejectABadCredential(t *testing.T) {
	api := newAPI(t, auth.RegistrationOpen)
	acct := api.newAccount("ada", "ada@example.com", "laptop")

	// A token signed by a different key, otherwise perfectly well-formed. This is the forgery the signing
	// key exists to stop, and the only test that exercises verification through the real chain.
	forger, err := auth.NewTokenIssuer([]byte("a-different-key-of-at-least-32-bytes"))
	require.NoError(t, err)
	forged, _, err := forger.Issue(snowflake.ID(1), snowflake.ID(2))
	require.NoError(t, err)

	credentials := []struct {
		name       string
		credential string
	}{
		{"garbage", "not-a-token"},
		{"empty bearer", ""},
		{"a jwt signed with a foreign key", forged},
		{"an api token that was never issued", "nat_" + strings.Repeat("A", 43)},
		// A refresh token is a genuine credential — for exchanging at /auth/refresh, and nowhere else.
		// Accepting one as a bearer token would hand a 30-day credential the reach of a 15-minute one.
		{"this account's own refresh token", acct.Tokens.RefreshToken},
	}

	for _, tc := range credentials {
		t.Run(tc.name, func(t *testing.T) {
			resp := api.call(http.MethodGet, "/api/v1/users/@me", nil, withToken(tc.credential))

			require.Equal(t, http.StatusUnauthorized, resp.Code, resp)
			assert.Equal(t, "unauthorized", resp.errorBody().Code)
		})
	}
}

// Token management requires the person, not a delegated credential (ADR 0022). This is the guard that is
// invisible from inside the handler: mintToken behaves identically whichever actor reaches it, so only the
// router decides, and only a router test can tell.
func TestAnAPITokenCannotManageTokens(t *testing.T) {
	api := newAPI(t, auth.RegistrationOpen)
	acct := api.newAccount("ada", "ada@example.com", "laptop")

	minted := api.call(http.MethodPost, "/api/v1/auth/tokens", map[string]any{
		"name":   "deploy bot",
		"scopes": []string{"identify"},
	}, withToken(acct.Tokens.AccessToken))
	require.Equal(t, http.StatusCreated, minted.Code, minted)

	var token struct {
		ID    string `json:"id"`
		Value string `json:"value"`
	}
	minted.decode(&token)
	require.NotEmpty(t, token.Value)

	// The token is a working credential: it authenticates, and it carries the identify scope.
	me := api.call(http.MethodGet, "/api/v1/users/@me", nil, withToken(token.Value))
	require.Equal(t, http.StatusOK, me.Code, "an API token must authenticate ordinary reads: %s", me)

	// But not for anything touching the account's credential inventory.
	forbidden := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"mint", http.MethodPost, "/api/v1/auth/tokens", map[string]any{"name": "second", "scopes": []string{"identify"}}},
		{"list", http.MethodGet, "/api/v1/auth/tokens", nil},
		{"revoke", http.MethodDelete, "/api/v1/auth/tokens/" + token.ID, nil},
	}
	for _, tc := range forbidden {
		t.Run(tc.name, func(t *testing.T) {
			resp := api.call(tc.method, tc.path, tc.body, withToken(token.Value))

			// 403, not 401. The credential is genuine and the caller is authenticated; re-authenticating
			// would not help, and a 401 would send a client into a pointless refresh loop.
			require.Equal(t, http.StatusForbidden, resp.Code, resp)
			assert.Equal(t, "forbidden", resp.errorBody().Code)
		})
	}

	// And the token it tried to revoke is still live.
	still := api.call(http.MethodGet, "/api/v1/users/@me", nil, withToken(token.Value))
	assert.Equal(t, http.StatusOK, still.Code, "a refused revoke must not have revoked anything")
}

// Ownership is enforced in the SQL statement's WHERE clause, which is what makes it impossible to forget.
// This asserts the property that matters from outside: another account's token ID is indistinguishable
// from one that does not exist, so the endpoint cannot be used to probe which IDs are real.
func TestOneAccountCannotRevokeAnothersToken(t *testing.T) {
	api := newAPI(t, auth.RegistrationOpen)

	owner := api.newAccount("ada", "ada@example.com", "laptop")
	minted := api.call(http.MethodPost, "/api/v1/auth/tokens", map[string]any{
		"name":   "ada's bot",
		"scopes": []string{"identify"},
	}, withToken(owner.Tokens.AccessToken))
	require.Equal(t, http.StatusCreated, minted.Code, minted)

	var token struct {
		ID    string `json:"id"`
		Value string `json:"value"`
	}
	minted.decode(&token)

	attacker := api.newAccount("eve", "eve@example.com", "laptop")

	stolen := api.call(http.MethodDelete, "/api/v1/auth/tokens/"+token.ID, nil,
		withToken(attacker.Tokens.AccessToken))
	require.Equal(t, http.StatusNotFound, stolen.Code, stolen)
	assert.Equal(t, "not_found", stolen.errorBody().Code)

	// Refusing has to mean refusing: the token still works.
	me := api.call(http.MethodGet, "/api/v1/users/@me", nil, withToken(token.Value))
	require.Equal(t, http.StatusOK, me.Code, "the owner's token must survive another account's revoke attempt")
	var self struct {
		ID string `json:"id"`
	}
	me.decode(&self)
	assert.Equal(t, owner.ID, self.ID)

	// The attacker's list is empty — they can see their own inventory and nothing else.
	listed := api.call(http.MethodGet, "/api/v1/auth/tokens", nil, withToken(attacker.Tokens.AccessToken))
	require.Equal(t, http.StatusOK, listed.Code, listed)
	var entries []map[string]any
	listed.decode(&entries)
	assert.Empty(t, entries, "listing must be scoped to the actor's own account")
}

// A malformed ID must be answered exactly like an unowned one, or the difference between 400 and 404
// becomes an oracle for which IDs exist.
func TestRevokingAMalformedTokenIDIsIndistinguishableFromAMissingOne(t *testing.T) {
	api := newAPI(t, auth.RegistrationOpen)
	acct := api.newAccount("ada", "ada@example.com", "laptop")

	malformed := api.call(http.MethodDelete, "/api/v1/auth/tokens/not-an-id", nil,
		withToken(acct.Tokens.AccessToken))
	missing := api.call(http.MethodDelete, "/api/v1/auth/tokens/1234567890", nil,
		withToken(acct.Tokens.AccessToken))

	require.Equal(t, http.StatusNotFound, malformed.Code, malformed)
	require.Equal(t, http.StatusNotFound, missing.Code, missing)

	a, b := malformed.errorBody(), missing.errorBody()
	a.RequestID, b.RequestID = "", ""
	assert.Equal(t, b, a, "the whole envelope must match, message included")
}

// ---------------------------------------------------------------------------
// credential material at the HTTP boundary (CLAUDE.md rule 8)
// ---------------------------------------------------------------------------

// The response types are built field-by-field from database rows precisely so a hash cannot reach the wire.
// This asserts the outcome rather than the technique, so it keeps holding if the technique changes.
func TestNoResponseLeaksStoredCredentialMaterial(t *testing.T) {
	api := newAPI(t, auth.RegistrationOpen)

	created := api.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "ada",
		"email":    "ada@example.com",
		"password": testPassword,
	})
	require.Equal(t, http.StatusCreated, created.Code, created)

	pair := api.login("ada@example.com", "laptop")
	me := api.call(http.MethodGet, "/api/v1/users/@me", nil, withToken(pair.AccessToken))
	require.Equal(t, http.StatusOK, me.Code, me)

	for _, resp := range []*response{created, me} {
		body := resp.String()
		assert.NotContains(t, body, testPassword, "the submitted password must never be echoed")
		assert.NotContains(t, body, "$argon2id$", "a password hash must never reach the wire")
		assert.NotContains(t, body, "password_hash")
	}

	// The refresh token is stored only as a SHA-256, so the row cannot be turned back into the credential.
	var stored string
	require.NoError(t, api.pool.QueryRow(t.Context(),
		"SELECT encode(refresh_token_hash, 'hex') FROM sessions LIMIT 1").Scan(&stored))
	assert.NotContains(t, pair.RefreshToken, stored)
	assert.NotEmpty(t, stored)
}

// A minted token's raw value exists exactly once, in the response that issued it. If listing ever grew a
// value field — by someone reusing the minted response type for the list — every token an account holds
// would be readable by anything that could read one of them.
func TestAMintedTokenValueIsReturnedOnlyOnce(t *testing.T) {
	api := newAPI(t, auth.RegistrationOpen)
	acct := api.newAccount("ada", "ada@example.com", "laptop")

	minted := api.call(http.MethodPost, "/api/v1/auth/tokens", map[string]any{
		"name":   "deploy bot",
		"scopes": []string{"identify"},
	}, withToken(acct.Tokens.AccessToken))
	require.Equal(t, http.StatusCreated, minted.Code, minted)

	var issued struct {
		Value string `json:"value"`
	}
	minted.decode(&issued)
	require.NotEmpty(t, issued.Value)
	assert.True(t, auth.LooksLikeOpaqueToken(issued.Value), "a minted token must carry its prefix")

	listed := api.call(http.MethodGet, "/api/v1/auth/tokens", nil, withToken(acct.Tokens.AccessToken))
	require.Equal(t, http.StatusOK, listed.Code, listed)

	assert.NotContains(t, listed.String(), issued.Value, "the raw value must not be recoverable from a listing")

	var entries []map[string]any
	listed.decode(&entries)
	require.Len(t, entries, 1)
	assert.NotContains(t, entries[0], "value", "the listing shape must not carry credential material at all")
	assert.Equal(t, "deploy bot", entries[0]["name"])
}

// ---------------------------------------------------------------------------
// indistinguishable failures
// ---------------------------------------------------------------------------

// An unknown address and a wrong password must produce byte-identical answers, or the login endpoint
// becomes a way to discover which addresses have accounts. The service test asserts the error value; only
// here can the actual response a caller sees be compared.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	api := newAPI(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")

	wrongPassword := api.call(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "ada@example.com", "password": "not the password", "device_id": "laptop",
	})
	unknownAccount := api.call(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "nobody@example.com", "password": testPassword, "device_id": "laptop",
	})

	require.Equal(t, http.StatusUnauthorized, wrongPassword.Code, wrongPassword)
	require.Equal(t, http.StatusUnauthorized, unknownAccount.Code, unknownAccount)

	// Everything except the correlation ID, which is unique per request by design.
	a, b := wrongPassword.errorBody(), unknownAccount.errorBody()
	a.RequestID, b.RequestID = "", ""
	assert.Equal(t, a, b, "the two failures must be indistinguishable to the client")
}

// Replaying a rotated refresh token is reported exactly like presenting an unknown one. Saying "that token
// was already used" would confirm to a thief that they hold something real, and tell them the legitimate
// client got there first.
func TestRefreshReplayIsReportedAsAnOrdinaryInvalidToken(t *testing.T) {
	api := newAPI(t, auth.RegistrationOpen)
	acct := api.newAccount("ada", "ada@example.com", "laptop")

	rotated := api.call(http.MethodPost, "/api/v1/auth/refresh", map[string]string{
		"refresh_token": acct.Tokens.RefreshToken,
	})
	require.Equal(t, http.StatusOK, rotated.Code, rotated)

	replay := api.call(http.MethodPost, "/api/v1/auth/refresh", map[string]string{
		"refresh_token": acct.Tokens.RefreshToken,
	})
	unknown := api.call(http.MethodPost, "/api/v1/auth/refresh", map[string]string{
		"refresh_token": "nrt_" + strings.Repeat("A", 43),
	})

	require.Equal(t, http.StatusUnauthorized, replay.Code, replay)
	require.Equal(t, http.StatusUnauthorized, unknown.Code, unknown)

	a, b := replay.errorBody(), unknown.errorBody()
	a.RequestID, b.RequestID = "", ""
	assert.Equal(t, a, b)
}

// ---------------------------------------------------------------------------
// request validation and status mapping
// ---------------------------------------------------------------------------

func TestRegisterRejectsBadRequestBodies(t *testing.T) {
	api := newAPI(t, auth.RegistrationOpen)

	valid := map[string]string{"username": "ada", "email": "ada@example.com", "password": testPassword}

	cases := []struct {
		name string
		body any
		opts []reqOpt
	}{
		{name: "empty body", body: ""},
		{name: "malformed JSON", body: `{"username":`},
		{name: "unknown field", body: `{"username":"ada","email":"a@b.co","password":"` + testPassword + `","admin":true}`},
		{name: "missing email", body: map[string]string{"username": "ada", "password": testPassword}},
		{name: "not an email", body: map[string]string{"username": "ada", "email": "not-an-email", "password": testPassword}},
		{name: "username with a space", body: map[string]string{"username": "a da", "email": "a@b.co", "password": testPassword}},
		{name: "two JSON objects", body: `{"username":"ada","email":"a@b.co","password":"x"}{"a":1}`},
		{name: "wrong content type", body: valid, opts: []reqOpt{withHeader("Content-Type", "text/plain")}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := api.call(http.MethodPost, "/api/v1/auth/register", tc.body, tc.opts...)

			require.Equal(t, http.StatusBadRequest, resp.Code, resp)
			body := resp.errorBody()
			assert.Equal(t, "bad_request", body.Code)
			// A validation message names the field and the rule; it must never echo the value, which on
			// this route is a password.
			assert.NotContains(t, body.Message, testPassword)
		})
	}
}

func TestRegisterReportsConflicts(t *testing.T) {
	api := newAPI(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")

	sameEmail := api.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "different", "email": "ada@example.com", "password": testPassword,
	})
	require.Equal(t, http.StatusConflict, sameEmail.Code, sameEmail)
	assert.Equal(t, "conflict", sameEmail.errorBody().Code)

	sameUsername := api.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "ada", "email": "different@example.com", "password": testPassword,
	})
	require.Equal(t, http.StatusConflict, sameUsername.Code, sameUsername)
}

// An instance in invite mode must refuse self-service registration, with its own code so a client can tell
// this apart from a permissions failure and say something useful.
func TestRegistrationIsRefusedInInviteMode(t *testing.T) {
	api := newAPI(t, auth.RegistrationInvite)

	resp := api.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "ada", "email": "ada@example.com", "password": testPassword,
	})

	require.Equal(t, http.StatusForbidden, resp.Code, resp)
	assert.Equal(t, "registration_closed", resp.errorBody().Code)
}

func TestMintingRejectsAnUnknownScope(t *testing.T) {
	api := newAPI(t, auth.RegistrationOpen)
	acct := api.newAccount("ada", "ada@example.com", "laptop")

	resp := api.call(http.MethodPost, "/api/v1/auth/tokens", map[string]any{
		"name":   "bot",
		"scopes": []string{"identify", "guilds:delete"},
	}, withToken(acct.Tokens.AccessToken))

	// Rejected, never silently dropped: a caller must not walk away believing they hold a scope they do not
	// (ADR 0022).
	require.Equal(t, http.StatusBadRequest, resp.Code, resp)
	assert.Equal(t, "bad_request", resp.errorBody().Code)

	listed := api.call(http.MethodGet, "/api/v1/auth/tokens", nil, withToken(acct.Tokens.AccessToken))
	var entries []map[string]any
	listed.decode(&entries)
	assert.Empty(t, entries, "a rejected mint must not have created anything")
}

// ---------------------------------------------------------------------------
// rate limiting
// ---------------------------------------------------------------------------

// The auth routes carry a stricter bucket *in addition to* the base limiter on the root chain. Two
// properties matter and neither is visible from a handler: that the strict bucket is actually mounted, and
// that it counts independently — so throttling a credential-guessing run cannot also throttle the same
// client's ordinary API traffic.
func TestAuthRoutesCarryTheStricterRateLimit(t *testing.T) {
	api := newAPI(t, auth.RegistrationOpen)

	const client = "203.0.113.42"
	// A body that fails validation is answered before the service is reached, so this measures the limiter
	// rather than paying for twenty argon2id hashes.
	incomplete := map[string]string{"email": "ada@example.com"}

	var throttled *response
	for i := 0; i < 40; i++ {
		resp := api.call(http.MethodPost, "/api/v1/auth/login", incomplete, fromIP(client))
		if resp.Code == http.StatusTooManyRequests {
			throttled = resp
			break
		}
		require.Equal(t, http.StatusBadRequest, resp.Code, "request %d: %s", i+1, resp)
	}

	require.NotNil(t, throttled,
		"the auth bucket (%s) must throttle well before the base limit (%s)", authRateLimit, testConfig().RateLimit)
	assert.Equal(t, "rate_limited", throttled.errorBody().Code)
	// Retry-After is what a well-behaved client — and the CLI's own backoff — actually reads.
	assert.NotEmpty(t, throttled.Header.Get("Retry-After"))
	assert.NotEmpty(t, throttled.Header.Get("X-RateLimit-Reset"))

	// The same client, well past the auth ceiling, is still served on a base-limited path.
	for i := 0; i < 25; i++ {
		resp := api.call(http.MethodGet, "/api/v1/users/@me", nil, fromIP(client))
		require.Equal(t, http.StatusUnauthorized, resp.Code,
			"request %d fell into the auth bucket; the two must count independently", i+1)
	}
}

// A client that gives up mid-request — most often while queued for an argon2id slot, which is the
// concurrency gate working as designed — must not be reported as a server fault. It used to fall through
// writeErr's default branch, logging at ERROR and answering 500: an ordinary login burst produced a stream
// of alarming lines about a server that was fine.
func TestAbandonedRequestIsNotReportedAsAServerFault(t *testing.T) {
	api := newAPI(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // the client is already gone before the handler runs

	body, err := json.Marshal(map[string]string{
		"email": "ada@example.com", "password": testPassword, "device_id": "laptop",
	})
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	api.handler.ServeHTTP(rec, req)

	assert.NotEqual(t, http.StatusInternalServerError, rec.Code,
		"a client that hung up is not a server error")
	if rec.Code == http.StatusServiceUnavailable {
		assert.Equal(t, "service_unavailable", (&response{t: t, Code: rec.Code, Body: rec.Body.Bytes()}).errorBody().Code)
	}
}
