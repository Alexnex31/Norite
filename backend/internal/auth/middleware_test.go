package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The guards are covered end-to-end through the real router in cmd/server's HTTP tests, which is where they
// belong: what matters about a guard is that it is actually mounted on the route it protects.
//
// Two things cannot be reached from there, and are covered here instead:
//
//   - RequireScope's refusal path. `identify` is the only scope this build defines, so every token the API
//     will mint holds the only scope any route currently checks. The refusal becomes reachable over HTTP
//     the moment a second scope exists — which is exactly when a regression in it would start mattering,
//     and too late to start testing it.
//   - Scope semantics for a user actor, which is a decision (ADR 0022) rather than an implementation
//     detail: a person is never restricted by a scope, because scopes bound *delegated* credentials.
//
// These need no database and run under `-short`.

// probe is a handler that records whether the guard let a request through.
func probe(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	})
}

// serve runs a request carrying the given actor through mw.
func serve(t *testing.T, mw func(http.Handler) http.Handler, actor *Actor) (*httptest.ResponseRecorder, bool) {
	t.Helper()

	var reached bool
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if actor != nil {
		req = req.WithContext(WithActor(req.Context(), *actor))
	}

	rec := httptest.NewRecorder()
	mw(probe(&reached)).ServeHTTP(rec, req)
	return rec, reached
}

// errorCode reads the code out of the error envelope.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope), "body: %s", rec.Body)
	return envelope.Error.Code
}

func TestRequireScopeRefusesATokenWithoutIt(t *testing.T) {
	actor := Actor{Kind: ActorAPIToken, UserID: 1, TokenID: 2, Scopes: []Scope{ScopeIdentify}}

	rec, reached := serve(t, RequireScope("guilds:manage"), &actor)

	assert.False(t, reached, "the handler must not run")
	// 403, not 401: the credential is genuine and the caller is authenticated. Re-authenticating would not
	// help, and a 401 would send a client into a pointless refresh loop.
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "forbidden", errorCode(t, rec))
	assert.Empty(t, rec.Header().Get("WWW-Authenticate"), "there is nothing to re-authenticate with")
}

func TestRequireScopeAdmitsATokenHoldingIt(t *testing.T) {
	actor := Actor{Kind: ActorAPIToken, UserID: 1, TokenID: 2, Scopes: []Scope{ScopeIdentify}}

	rec, reached := serve(t, RequireScope(ScopeIdentify), &actor)

	assert.True(t, reached)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// A person is never restricted by a scope. The alternative — granting user tokens every scope explicitly —
// would mean each new scope silently locking human users out of a feature until someone remembered to
// widen the implicit set, which is a failure that shows up in production rather than in review.
func TestRequireScopeNeverRestrictsAUserActor(t *testing.T) {
	actor := Actor{Kind: ActorUser, UserID: 1, SessionID: 2}

	rec, reached := serve(t, RequireScope("a scope that does not exist"), &actor)

	assert.True(t, reached, "a user actor passes every scope check by design")
	assert.Equal(t, http.StatusOK, rec.Code)
}

// Every guard has to refuse an unauthenticated request on its own, not assume RequireAuth ran first. They
// are composed by hand at each route, and "someone forgot the outer guard" must fail closed.
func TestEveryGuardRefusesAnUnauthenticatedRequest(t *testing.T) {
	guards := map[string]func(http.Handler) http.Handler{
		"RequireAuth":      RequireAuth,
		"RequireScope":     RequireScope(ScopeIdentify),
		"RequireUserActor": RequireUserActor,
	}

	for name, guard := range guards {
		t.Run(name, func(t *testing.T) {
			rec, reached := serve(t, guard, nil)

			assert.False(t, reached)
			require.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Equal(t, "unauthorized", errorCode(t, rec))
			assert.Equal(t, `Bearer realm="norite"`, rec.Header().Get("WWW-Authenticate"),
				"RFC 9110 requires it on a 401")
		})
	}
}

// The zero actor is what a context carries when nothing authenticated. It must never read as a valid
// identity, or a bug that stores an empty actor would authenticate every request as user 0.
func TestTheZeroActorIsNotAnIdentity(t *testing.T) {
	rec, reached := serve(t, RequireAuth, &Actor{})

	assert.False(t, reached)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestRequireUserActorRefusesADelegatedCredential(t *testing.T) {
	actor := Actor{Kind: ActorAPIToken, UserID: 1, TokenID: 2, Scopes: []Scope{ScopeIdentify}}

	rec, reached := serve(t, RequireUserActor, &actor)

	assert.False(t, reached)
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "forbidden", errorCode(t, rec))
}

// bearerCredential is what decides whether a request presented a credential at all. RFC 9110 makes scheme
// names case-insensitive, and a client sending "bearer" is following the spec.
func TestBearerCredentialParsing(t *testing.T) {
	cases := []struct {
		header string
		want   string
		ok     bool
	}{
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true},
		{"BEARER abc", "abc", true},
		{"Bearer   abc  ", "abc", true},
		{"", "", false},
		{"abc", "", false},
		{"Basic abc", "", false},
		{"Bearer", "", false},
		{"Bearer ", "", false},
		{"Bearer\t", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}

			got, ok := bearerCredential(req)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}
