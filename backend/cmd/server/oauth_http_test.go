package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/Alexnex31/Norite/backend/internal/auth"
)

// M6's "done when" is that completing a provider flow against the backend issues a valid token pair.
// These drive that through the assembled router — real middleware chain, real database, real templates —
// against a stub standing in for Google.
//
// A stub rather than the real thing for the usual reason: the cases worth testing are an unverified email
// and a refused code, and neither Google nor GitHub will produce those on request.

// oauthStub is a minimal provider: a token endpoint and a userinfo endpoint.
type oauthStub struct {
	server *httptest.Server
	info   map[string]any
}

func newOAuthStub(t *testing.T) *oauthStub {
	t.Helper()
	stub := &oauthStub{}

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "stub-token", "token_type": "Bearer",
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stub.info)
	})

	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)
	return stub
}

// as points the stub at one identity.
func (s *oauthStub) as(sub, email string, verified bool) {
	s.info = map[string]any{"sub": sub, "email": email, "email_verified": verified, "name": "Ada Lovelace"}
}

func (s *oauthStub) providers() auth.OAuthProviders {
	endpoint := oauth2.Endpoint{AuthURL: s.server.URL + "/authorize", TokenURL: s.server.URL + "/token"}
	return auth.NewOAuthProviders(auth.OAuthOptions{
		PublicBaseURL:      "https://chat.example.com",
		GoogleClientID:     "stub-id",
		GoogleClientSecret: "stub-secret",
		HTTPClient:         s.server.Client(),
		GoogleUserInfoURL:  s.server.URL + "/userinfo",
		GoogleEndpoint:     &endpoint,
	})
}

// newOAuthAPI builds the router with the stub provider configured.
func newOAuthAPI(t *testing.T, mode auth.RegistrationMode) (*api, *oauthStub) {
	t.Helper()
	stub := newOAuthStub(t)
	return newAPIWith(t, mode, &captureMailer{}, stub.providers()), stub
}

// authorizeAndCallback drives the two browser legs and returns the callback's rendered page.
func (a *api) authorizeAndCallback(t *testing.T) *response {
	t.Helper()

	start := a.call(http.MethodGet, "/api/v1/auth/oauth/google/authorize", nil)
	require.Equal(t, http.StatusFound, start.Code, "authorize must redirect to the provider: %s", start)

	location, err := url.Parse(start.Header.Get("Location"))
	require.NoError(t, err)

	state := location.Query().Get("state")
	require.NotEmpty(t, state, "the redirect must carry a state parameter")
	require.NotEmpty(t, location.Query().Get("code_challenge"), "and a PKCE challenge")

	return a.call(http.MethodGet,
		"/api/v1/auth/oauth/google/callback?state="+url.QueryEscape(state)+"&code=stub-code", nil)
}

// exchangeCodeFromPage pulls the one-time code out of the rendered page, which is the only place it
// appears — it is deliberately never in a URL.
func exchangeCodeFromPage(t *testing.T, page *response) string {
	t.Helper()
	_, after, found := strings.Cut(page.String(), `<code class="code">`)
	require.True(t, found, "the page must carry an exchange code:\n%s", page)
	code, _, _ := strings.Cut(after, "<")
	require.NotEmpty(t, code)
	return code
}

// ---------- M6 done-when: a flow issues a valid token pair ----------

func TestOAuthSignInIssuesATokenPair(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	acct := api.newAccount("ada", "ada@example.com", "laptop")

	// A provider-verified address matching an existing account links and signs in.
	stub.as("google-1", "ada@example.com", true)

	page := api.authorizeAndCallback(t)
	require.Equal(t, http.StatusOK, page.Code, page)
	assert.Contains(t, page.String(), "You're signed in")

	code := exchangeCodeFromPage(t, page)
	assert.NotContains(t, page.Header.Get("Location"), code, "the code must never travel in a URL")

	exchanged := api.call(http.MethodPost, "/api/v1/auth/oauth/exchange", map[string]string{
		"code": code, "device_id": "laptop",
	})
	require.Equal(t, http.StatusOK, exchanged.Code, exchanged)

	var pair tokenPair
	exchanged.decode(&pair)
	require.NotEmpty(t, pair.AccessToken)
	assert.Equal(t, "Bearer", pair.TokenType)

	// The pair is a real one: it authenticates, and as the account that already existed.
	me := api.call(http.MethodGet, "/api/v1/users/@me", nil, withToken(pair.AccessToken))
	require.Equal(t, http.StatusOK, me.Code, me)
	var self struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	me.decode(&self)
	assert.Equal(t, acct.ID, self.ID, "OAuth must sign in to the existing account, not a new one")
	assert.Equal(t, "ada@example.com", self.Email)
}

// The other half of the same criterion: a brand-new account, created through the browser form.
func TestOAuthSignupCompletesThroughTheForm(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	stub.as("google-99", "newcomer@example.com", true)

	page := api.authorizeAndCallback(t)
	require.Equal(t, http.StatusOK, page.Code, page)
	assert.Contains(t, page.String(), "Choose your username")
	assert.Contains(t, page.String(), "newcomer@example.com", "the page must show which account is being made")

	token := hiddenField(t, page, "signup_token")

	// Nothing exists yet — that is the whole point of the continuation token.
	var users int
	require.NoError(t, api.pool.QueryRow(t.Context(), "SELECT count(*) FROM users").Scan(&users))
	require.Zero(t, users, "no account may exist before a username is chosen")

	form := url.Values{"signup_token": {token}, "username": {"newcomer"}}
	done := api.call(http.MethodPost, "/oauth/signup", form.Encode(),
		withHeader("Content-Type", "application/x-www-form-urlencoded"))
	require.Equal(t, http.StatusOK, done.Code, done)

	exchanged := api.call(http.MethodPost, "/api/v1/auth/oauth/exchange", map[string]string{
		"code": exchangeCodeFromPage(t, done), "device_id": "laptop",
	})
	require.Equal(t, http.StatusOK, exchanged.Code, exchanged)

	var pair tokenPair
	exchanged.decode(&pair)

	me := api.call(http.MethodGet, "/api/v1/users/@me", nil, withToken(pair.AccessToken))
	require.Equal(t, http.StatusOK, me.Code)
	var self struct {
		Username string `json:"username"`
		Email    string `json:"email"`
	}
	me.decode(&self)
	assert.Equal(t, "newcomer", self.Username)
	assert.Equal(t, "newcomer@example.com", self.Email)
}

// hiddenField reads a hidden input's value out of a rendered form.
func hiddenField(t *testing.T, page *response, name string) string {
	t.Helper()
	_, after, found := strings.Cut(page.String(), `name="`+name+`" value="`)
	require.True(t, found, "no %s field on the page:\n%s", name, page)
	value, _, _ := strings.Cut(after, `"`)
	require.NotEmpty(t, value)
	return value
}

// ---------- the linking rule, through the router ----------

// The decision the milestone turns on, at the level a user actually meets it: an unverified address
// matching an existing account is refused, and the page says what to do instead.
func TestOAuthRefusesAnUnverifiedAddressThatMatchesAnAccount(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")
	stub.as("google-1", "ada@example.com", false)

	page := api.authorizeAndCallback(t)

	require.Equal(t, http.StatusBadRequest, page.Code)
	assert.Contains(t, page.String(), "sign in with your password",
		"the refusal must tell the person what to do, or they press the same button forever")

	// Nothing was linked and no account was created.
	var links int
	require.NoError(t, api.pool.QueryRow(t.Context(), "SELECT count(*) FROM oauth_identities").Scan(&links))
	assert.Zero(t, links)
}

// ---------- state and replay, through the router ----------

func TestOAuthCallbackStateIsSingleUseThroughTheRouter(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")
	stub.as("google-1", "ada@example.com", true)

	start := api.call(http.MethodGet, "/api/v1/auth/oauth/google/authorize", nil)
	location, err := url.Parse(start.Header.Get("Location"))
	require.NoError(t, err)
	state := location.Query().Get("state")

	callback := "/api/v1/auth/oauth/google/callback?state=" + url.QueryEscape(state) + "&code=stub-code"

	first := api.call(http.MethodGet, callback, nil)
	require.Equal(t, http.StatusOK, first.Code)

	// A refresh of the callback page is the ordinary way this happens.
	second := api.call(http.MethodGet, callback, nil)
	require.Equal(t, http.StatusBadRequest, second.Code)
	assert.Contains(t, second.String(), "no longer valid")
}

// A provider reporting its own failure — most often someone pressing "cancel" — is an abandonment, not a
// fault, and must not read like a broken instance.
func TestOAuthCallbackHandlesAProviderError(t *testing.T) {
	api, _ := newOAuthAPI(t, auth.RegistrationOpen)

	page := api.call(http.MethodGet,
		"/api/v1/auth/oauth/google/callback?error=access_denied&state=x", nil)

	require.Equal(t, http.StatusBadRequest, page.Code)
	assert.Contains(t, page.String(), "not completed")
	assert.NotContains(t, page.String(), "wrong on our end", "a declined consent is not a server fault")
}

// The callback echoes nothing a provider supplied as markup. The provider controls the display name and
// the address, and both reach a template.
func TestOAuthPagesEscapeProviderSuppliedText(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	stub.as("google-99", `"><script>alert(1)</script>@example.com`, true)

	page := api.authorizeAndCallback(t)

	assert.NotContains(t, page.String(), "<script>alert(1)</script>",
		"provider-supplied text must never reach the page as markup (CLAUDE.md rule 9)")
}

// An invite-only instance refuses to create an account through a provider, and says so on the page.
func TestOAuthSignupIsRefusedOnAnInviteOnlyInstance(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationInvite)
	stub.as("google-99", "newcomer@example.com", true)

	page := api.authorizeAndCallback(t)

	require.Equal(t, http.StatusBadRequest, page.Code)
	assert.Contains(t, page.String(), "invite code")
}

// A username the rule rejects re-renders the form rather than ending the flow, so it can be fixed without
// starting over — but an unusable token cannot be re-rendered and ends on the error page.
func TestOAuthSignupFormRerendersOnABadUsername(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	stub.as("google-99", "newcomer@example.com", true)

	page := api.authorizeAndCallback(t)
	token := hiddenField(t, page, "signup_token")

	form := url.Values{"signup_token": {token}, "username": {"ada lovelace"}}
	rejected := api.call(http.MethodPost, "/oauth/signup", form.Encode(),
		withHeader("Content-Type", "application/x-www-form-urlencoded"))

	require.Equal(t, http.StatusBadRequest, rejected.Code)
	assert.Contains(t, rejected.String(), "Choose your username", "the form must come back, not vanish")
	assert.Contains(t, rejected.String(), "letters, digits")

	// The address has to survive the re-render. It is the only thing on the page saying *which* account is
	// being created, and losing it at the moment someone is correcting a mistake is when it matters most.
	assert.Contains(t, rejected.String(), "newcomer@example.com",
		"the account being created must stay named on the second attempt")

	// ...and the token it carries still works, so a second attempt succeeds.
	retry := url.Values{"signup_token": {hiddenField(t, rejected, "signup_token")}, "username": {"newcomer"}}
	done := api.call(http.MethodPost, "/oauth/signup", retry.Encode(),
		withHeader("Content-Type", "application/x-www-form-urlencoded"))
	assert.Equal(t, http.StatusOK, done.Code, done)
}
