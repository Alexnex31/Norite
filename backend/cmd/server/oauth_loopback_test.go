package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/auth"
)

// M8's backend half: a client that cannot read a rendered page names a listener on its own machine, and
// the callback returns the exchange code there instead of displaying it.
//
// What travels is worth being precise about, because ADR 0024 refused to put credentials in a URL and this
// looks at a glance like reopening that. It is a single-use code with a two-minute life, worthless without
// the flow verifier that never left the client, on a hop that never leaves the machine. Not a token pair,
// and not over the network.

// loopbackRedirect is a return URI shaped like the one the CLI will bind. Nothing listens on it — these
// tests assert on the Location header rather than following it.
const loopbackRedirect = "http://127.0.0.1:51763/callback"

// authorizeAndCallbackReturning drives the two browser legs with a client redirect, and returns the
// callback's response together with the verifier the flow was bound to.
func (a *api) authorizeAndCallbackReturning(t *testing.T, redirect string) (*response, string) {
	t.Helper()

	verifier, challenge, err := auth.GenerateOAuthFlowVerifier()
	require.NoError(t, err)

	start := a.call(http.MethodGet, "/api/v1/auth/oauth/google/authorize?flow_challenge="+
		url.QueryEscape(auth.OAuthFlowChallengeFor(challenge))+
		"&client_redirect_uri="+url.QueryEscape(redirect), nil)
	require.Equal(t, http.StatusFound, start.Code, "authorize must redirect to the provider: %s", start)

	location, err := url.Parse(start.Header.Get("Location"))
	require.NoError(t, err)
	state := location.Query().Get("state")
	require.NotEmpty(t, state)

	return a.call(http.MethodGet,
		"/api/v1/auth/oauth/google/callback?state="+url.QueryEscape(state)+"&code=stub-code", nil), verifier
}

// returnedTo parses a callback's Location header, asserting it is a redirect at all.
func returnedTo(t *testing.T, res *response) *url.URL {
	t.Helper()
	require.Equal(t, http.StatusFound, res.Code, "the callback must redirect, not render: %s", res)
	u, err := url.Parse(res.Header.Get("Location"))
	require.NoError(t, err)
	return u
}

// ---------- M8 done-when, backend half ----------

func TestOAuthCallbackRedirectsToTheClientsLoopbackListener(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	acct := api.newAccount("ada", "ada@example.com", "laptop")
	stub.as("google-1", "ada@example.com", true)

	res, verifier := api.authorizeAndCallbackReturning(t, loopbackRedirect)
	got := returnedTo(t, res)

	assert.Equal(t, "http", got.Scheme)
	assert.Equal(t, "127.0.0.1:51763", got.Host)
	assert.Equal(t, "/callback", got.Path)

	code := got.Query().Get("code")
	require.NotEmpty(t, code, "the redirect must carry the exchange code")
	assert.True(t, strings.HasPrefix(code, "noc_"), "and it must be an exchange code: %q", code)

	// The delivered code is a real one: it redeems, for the account that already existed.
	exchanged := api.call(http.MethodPost, "/api/v1/auth/oauth/exchange", map[string]string{
		"code": code, "flow_verifier": verifier, "device_id": "laptop",
	})
	require.Equal(t, http.StatusOK, exchanged.Code, exchanged)

	var pair tokenPair
	exchanged.decode(&pair)
	me := api.call(http.MethodGet, "/api/v1/users/@me", nil, withToken(pair.AccessToken))
	require.Equal(t, http.StatusOK, me.Code, me)
	var self struct {
		ID string `json:"id"`
	}
	me.decode(&self)
	assert.Equal(t, acct.ID, self.ID)
}

// The path every existing client is on. A flow started without a redirect must behave exactly as it did
// before this existed — it is what a browser gets, and what the device-code flow will get at M9.
func TestOAuthCallbackStillRendersThePageWithoutARedirect(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")
	stub.as("google-1", "ada@example.com", true)

	page := api.authorizeAndCallback(t)
	require.Equal(t, http.StatusOK, page.Code, page)
	assert.Contains(t, page.String(), "You're signed in")
	assert.Empty(t, page.Header.Get("Location"), "no redirect was asked for, so none may happen")
}

// The redirect carries the code and nothing else — no state, no token, no address. A client parses this
// URL, and every extra parameter is one more thing it has to be right about ignoring.
func TestTheRedirectCarriesTheCodeAndNothingElse(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")
	stub.as("google-1", "ada@example.com", true)

	res, _ := api.authorizeAndCallbackReturning(t, loopbackRedirect)
	got := returnedTo(t, res)

	assert.Equal(t, []string{"code"}, keysOf(got.Query()))

	// The headers that keep the code out of anywhere else. Both come from httpx.HTMLPage, which wraps this
	// route, and both apply to the 302 as much as to the page — asserted rather than assumed, because a
	// Referer leaking a credential is exactly the kind of thing a refactor removes silently.
	assert.Equal(t, "no-referrer", res.Header.Get("Referrer-Policy"))
	assert.Contains(t, res.Header.Get("Cache-Control"), "no-store")
}

// The destination is fixed when the flow starts and read back out of the state row. A callback that
// supplies its own must be ignored — the callback URL is written by whoever presents it, and this value
// decides where a credential is delivered. Same discipline as the PKCE verifier living server-side.
func TestACallbackCannotSupplyItsOwnRedirect(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")
	stub.as("google-1", "ada@example.com", true)

	_, challenge, err := auth.GenerateOAuthFlowVerifier()
	require.NoError(t, err)

	// Started with no redirect at all.
	start := api.call(http.MethodGet, "/api/v1/auth/oauth/google/authorize?flow_challenge="+
		url.QueryEscape(auth.OAuthFlowChallengeFor(challenge)), nil)
	require.Equal(t, http.StatusFound, start.Code, start)
	location, err := url.Parse(start.Header.Get("Location"))
	require.NoError(t, err)

	// The callback then asks for one.
	page := api.call(http.MethodGet, "/api/v1/auth/oauth/google/callback?state="+
		url.QueryEscape(location.Query().Get("state"))+"&code=stub-code"+
		"&client_redirect_uri="+url.QueryEscape("http://127.0.0.1:9999/steal"), nil)

	require.Equal(t, http.StatusOK, page.Code, "the page must render: %s", page)
	assert.Empty(t, page.Header.Get("Location"), "a redirect supplied at the callback must be ignored")
	assert.NotContains(t, page.String(), "9999")
}

// Delivering the code to a listener does not weaken the binding that makes the code safe to deliver. This
// is the test that justifies validating the redirect by host alone and not against a port allowlist: a
// squatter on any port receives something it cannot spend.
func TestARedirectDoesNotWeakenTheClientBinding(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")
	stub.as("google-1", "ada@example.com", true)

	res, _ := api.authorizeAndCallbackReturning(t, loopbackRedirect)
	code := returnedTo(t, res).Query().Get("code")
	require.NotEmpty(t, code)

	// A verifier from some other flow — which is everything a process that merely received the redirect
	// would have.
	other, _, err := auth.GenerateOAuthFlowVerifier()
	require.NoError(t, err)

	refused := api.call(http.MethodPost, "/api/v1/auth/oauth/exchange", map[string]string{
		"code": code, "flow_verifier": other, "device_id": "thief",
	})
	assert.Equal(t, http.StatusUnauthorized, refused.Code, refused)
}

// ---------- what the instance will not send a browser to ----------

// The HTTP-level half of ParseOAuthClientRedirect's unit table: the refusal has to reach the client as
// a 400 rather than being swallowed into a rendered page or a 500.
func TestOAuthAuthorizeRefusesANonLoopbackRedirect(t *testing.T) {
	api, _ := newOAuthAPI(t, auth.RegistrationOpen)

	_, challenge, err := auth.GenerateOAuthFlowVerifier()
	require.NoError(t, err)
	flow := url.QueryEscape(auth.OAuthFlowChallengeFor(challenge))

	for _, tc := range []struct{ why, redirect string }{
		{"an ordinary remote host", "http://evil.example/cb"},
		{"a remote host over TLS", "https://evil.example/cb"},
		{"a host that merely begins with the loopback address", "http://127.0.0.1.evil.example:51763/cb"},
		{"the link-local metadata address", "http://169.254.169.254/cb"},
		{"a private-range address", "http://10.0.0.1:51763/cb"},
		{"the name localhost rather than an address", "http://localhost:51763/cb"},
		{"userinfo hiding the real host", "http://127.0.0.1:51763@evil.example/cb"},
		{"a scheme-relative URI", "//evil.example/cb"},
		{"a non-navigation scheme", "javascript:alert(1)"},
		{"a local file", "file:///etc/passwd"},
		{"loopback over TLS", "https://127.0.0.1:51763/cb"},
		{"no explicit port", "http://127.0.0.1/cb"},
		{"port zero", "http://127.0.0.1:0/cb"},
		{"a query that would collide with the code", "http://127.0.0.1:51763/cb?code=x"},
		{"a fragment", "http://127.0.0.1:51763/cb#f"},
		{"a zoned loopback address", "http://[::1%25eth0]:51763/cb"},
		{"a carriage return", "http://127.0.0.1:51763/cb\r\nX: y"},
	} {
		res := api.call(http.MethodGet, "/api/v1/auth/oauth/google/authorize?flow_challenge="+flow+
			"&client_redirect_uri="+url.QueryEscape(tc.redirect), nil)
		assert.Equal(t, http.StatusBadRequest, res.Code, "%s: %q must be refused, got %s",
			tc.why, tc.redirect, res)
		assert.Empty(t, res.Header.Get("Location"), "%s: nothing may be redirected to", tc.why)
	}

	// And the shape a CLI actually sends is accepted, so the table above is refusing for the right reason
	// rather than because the request was malformed in some other way.
	ok := api.call(http.MethodGet, "/api/v1/auth/oauth/google/authorize?flow_challenge="+flow+
		"&client_redirect_uri="+url.QueryEscape(loopbackRedirect), nil)
	assert.Equal(t, http.StatusFound, ok.Code, ok)
}

// ---------- the recipe a client reads ----------

// The sibling of TestTheDocumentedFlowVerifierConstructionWorks, and it exists for the same reason: the
// CLI reads contracts/openapi.yaml, not this package's helpers. So this drives the whole loopback flow
// using only what the document says — the verifier recipe by hand, the redirect written out literally —
// and calls no auth helper at all. If the contract and the implementation ever disagree, this fails and
// the other loopback tests do not.
func TestTheDocumentedLoopbackRedirectRecipeWorks(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	acct := api.newAccount("ada", "ada@example.com", "laptop")
	stub.as("google-1", "ada@example.com", true)

	buf := make([]byte, 32)
	_, err := rand.Read(buf)
	require.NoError(t, err)
	verifier := "nof_" + base64.RawURLEncoding.EncodeToString(buf)
	require.Len(t, verifier, 47, "the contract promises 47 characters")

	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	require.Len(t, challenge, 43, "the contract promises 43 characters")

	// "an http URL on a loopback IP literal with an explicit port, no userinfo, no query and no fragment"
	redirect := "http://127.0.0.1:51763/callback"

	start := api.call(http.MethodGet, "/api/v1/auth/oauth/google/authorize?flow_challenge="+
		url.QueryEscape(challenge)+"&client_redirect_uri="+url.QueryEscape(redirect), nil)
	require.Equal(t, http.StatusFound, start.Code, start)

	location, err := url.Parse(start.Header.Get("Location"))
	require.NoError(t, err)

	back := api.call(http.MethodGet, "/api/v1/auth/oauth/google/callback?state="+
		url.QueryEscape(location.Query().Get("state"))+"&code=stub-code", nil)
	require.Equal(t, http.StatusFound, back.Code, "the callback must return to the listener: %s", back)

	returned, err := url.Parse(back.Header.Get("Location"))
	require.NoError(t, err)
	require.Equal(t, redirect, returned.Scheme+"://"+returned.Host+returned.Path)

	exchanged := api.call(http.MethodPost, "/api/v1/auth/oauth/exchange", map[string]string{
		"code": returned.Query().Get("code"), "flow_verifier": verifier, "device_id": "laptop",
	})
	require.Equal(t, http.StatusOK, exchanged.Code, exchanged)

	var pair tokenPair
	exchanged.decode(&pair)
	me := api.call(http.MethodGet, "/api/v1/users/@me", nil, withToken(pair.AccessToken))
	require.Equal(t, http.StatusOK, me.Code)
	var self struct {
		ID string `json:"id"`
	}
	me.decode(&self)
	assert.Equal(t, acct.ID, self.ID)
}

// keysOf lists a query's parameter names, sorted, for an exact assertion on what a URL carries.
func keysOf(v url.Values) []string {
	out := make([]string, 0, len(v))
	for k := range v {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// ---------- the signup hop ----------

// A brand-new OAuth account still returns to the listener. Without this a first-time user's sign-in
// completes in a browser and the CLI waits forever, which is the difference between `norite login`
// onboarding somebody and only ever working for accounts that already exist.
func TestTheLoopbackRedirectSurvivesTheUsernameForm(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	stub.as("google-99", "newcomer@example.com", true)

	page, verifier := api.authorizeAndCallbackReturning(t, loopbackRedirect)
	require.Equal(t, http.StatusOK, page.Code, "a new identity must reach the form, not a redirect: %s", page)
	assert.Contains(t, page.String(), "Choose your username")

	form := url.Values{"signup_token": {hiddenField(t, page, "signup_token")}, "username": {"newcomer"}}
	done := api.call(http.MethodPost, "/oauth/signup", form.Encode(),
		withHeader("Content-Type", "application/x-www-form-urlencoded"))

	got := returnedTo(t, done)
	assert.Equal(t, "127.0.0.1:51763", got.Host)
	assert.Equal(t, "/callback", got.Path)
	assert.Equal(t, []string{"code"}, keysOf(got.Query()))

	// And the delivered code redeems, so the account really exists.
	exchanged := api.call(http.MethodPost, "/api/v1/auth/oauth/exchange", map[string]string{
		"code": got.Query().Get("code"), "flow_verifier": verifier, "device_id": "laptop",
	})
	require.Equal(t, http.StatusOK, exchanged.Code, exchanged)

	var pair tokenPair
	exchanged.decode(&pair)
	me := api.call(http.MethodGet, "/api/v1/users/@me", nil, withToken(pair.AccessToken))
	require.Equal(t, http.StatusOK, me.Code, me)
	var self struct {
		Username string `json:"username"`
	}
	me.decode(&self)
	assert.Equal(t, "newcomer", self.Username)
}

// The security property of the signup hop, and the reason the redirect rides inside the signed token
// rather than in a hidden field: this form is submitted by whoever is looking at the page, so a redirect
// they could supply would let them choose where somebody else's exchange code is delivered.
func TestTheSignupFormCannotChooseWhereTheCodeIsDelivered(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	stub.as("google-99", "newcomer@example.com", true)

	page, _ := api.authorizeAndCallbackReturning(t, loopbackRedirect)
	require.Equal(t, http.StatusOK, page.Code, page)

	// Every spelling a well-meaning client — or an attacker — might try.
	form := url.Values{
		"signup_token":        {hiddenField(t, page, "signup_token")},
		"username":            {"newcomer"},
		"client_redirect_uri": {"http://127.0.0.1:9999/steal"},
		"redirect_uri":        {"http://127.0.0.1:9999/steal"},
		"rdr":                 {"http://127.0.0.1:9999/steal"},
	}
	done := api.call(http.MethodPost, "/oauth/signup", form.Encode(),
		withHeader("Content-Type", "application/x-www-form-urlencoded"))

	got := returnedTo(t, done)
	assert.Equal(t, "127.0.0.1:51763", got.Host, "the destination must come from the token, not the form")
	assert.NotContains(t, got.String(), "9999")
	assert.NotContains(t, got.String(), "steal")
}

// A rejected username re-renders the form with the same token, so the listener survives a second attempt.
// Free, given where the redirect lives — asserted because "free" is a claim about the design that a
// future change to the re-render path could quietly break.
func TestTheListenerSurvivesARejectedUsername(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	stub.as("google-99", "newcomer@example.com", true)

	page, _ := api.authorizeAndCallbackReturning(t, loopbackRedirect)
	require.Equal(t, http.StatusOK, page.Code, page)
	token := hiddenField(t, page, "signup_token")

	rejected := api.call(http.MethodPost, "/oauth/signup",
		url.Values{"signup_token": {token}, "username": {"!"}}.Encode(),
		withHeader("Content-Type", "application/x-www-form-urlencoded"))
	require.Equal(t, http.StatusBadRequest, rejected.Code, rejected)

	done := api.call(http.MethodPost, "/oauth/signup",
		url.Values{"signup_token": {hiddenField(t, rejected, "signup_token")}, "username": {"newcomer"}}.Encode(),
		withHeader("Content-Type", "application/x-www-form-urlencoded"))
	assert.Equal(t, "127.0.0.1:51763", returnedTo(t, done).Host)
}

// The JSON completion endpoint has no browser to send anywhere, so it answers with a code and says nothing
// about a redirect — a client driving the flow itself already knows where it lives.
func TestTheJSONSignupEndpointReturnsACodeAndNoRedirect(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	stub.as("google-99", "newcomer@example.com", true)

	page, _ := api.authorizeAndCallbackReturning(t, loopbackRedirect)
	require.Equal(t, http.StatusOK, page.Code, page)

	done := api.call(http.MethodPost, "/api/v1/auth/oauth/complete", map[string]string{
		"signup_token": hiddenField(t, page, "signup_token"), "username": "newcomer",
	})
	require.Equal(t, http.StatusOK, done.Code, done)
	assert.Empty(t, done.Header.Get("Location"))

	var body map[string]any
	done.decode(&body)
	assert.Contains(t, body, "code")
	assert.NotContains(t, body, "client_redirect_uri")
	assert.NotContains(t, done.String(), "127.0.0.1")
}
