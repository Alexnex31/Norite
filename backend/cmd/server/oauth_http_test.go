package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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
//
// The flow verifier it mints is discarded; tests that go on to redeem the code use authorizeAndCallbackBound.
func (a *api) authorizeAndCallback(t *testing.T) *response {
	t.Helper()
	page, _ := a.authorizeAndCallbackBound(t)
	return page
}

// authorizeAndCallbackBound is authorizeAndCallback, keeping the verifier the flow was bound to.
func (a *api) authorizeAndCallbackBound(t *testing.T) (*response, string) {
	t.Helper()

	verifier, challenge, err := auth.GenerateOAuthFlowVerifier()
	require.NoError(t, err)

	start := a.call(http.MethodGet,
		"/api/v1/auth/oauth/google/authorize?flow_challenge="+
			url.QueryEscape(auth.OAuthFlowChallengeFor(challenge)), nil)
	require.Equal(t, http.StatusFound, start.Code, "authorize must redirect to the provider: %s", start)

	location, err := url.Parse(start.Header.Get("Location"))
	require.NoError(t, err)

	state := location.Query().Get("state")
	require.NotEmpty(t, state, "the redirect must carry a state parameter")
	require.NotEmpty(t, location.Query().Get("code_challenge"), "and a PKCE challenge")

	return a.call(http.MethodGet,
		"/api/v1/auth/oauth/google/callback?state="+url.QueryEscape(state)+"&code=stub-code", nil), verifier
}

// exchangeCodeFromPage pulls the one-time code out of the rendered page.
//
// The page is where it appears for a flow that named no loopback listener, which is every flow in this
// file. A flow that named one gets the code on the redirect instead (oauth_loopback_test.go).
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

	page, verifier := api.authorizeAndCallbackBound(t)
	require.Equal(t, http.StatusOK, page.Code, page)
	assert.Contains(t, page.String(), "You're signed in")

	code := exchangeCodeFromPage(t, page)
	// This flow named no listener, so there is no redirect at all — the code is on the page and nowhere
	// else. A flow that does name one gets it delivered instead; see oauth_loopback_test.go for what may
	// travel there and why that is not the token-in-a-URL shape ADR 0024 refused.
	assert.Empty(t, page.Header.Get("Location"), "a flow with no listener must not redirect anywhere")

	exchanged := api.call(http.MethodPost, "/api/v1/auth/oauth/exchange", map[string]string{
		"code": code, "flow_verifier": verifier, "device_id": "laptop",
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

	page, verifier := api.authorizeAndCallbackBound(t)
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
		"code": exchangeCodeFromPage(t, done), "flow_verifier": verifier, "device_id": "laptop",
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

// The decision the milestone turns on, at the level a user actually meets it — and at M10 what the browser
// meets is the same page whichever case it is.
//
// M6 refused here, and had to: it could not verify an address itself, so an address the provider would not
// vouch for was a dead end. M10 turns that into a detour by mail. What must not change is that the page
// says nothing about whether the address has an account, because anyone can present any address at a
// provider that does not verify it.
func TestAnUnverifiedProviderAddressEndsAtTheSamePageEitherWay(t *testing.T) {
	// Both registration modes. The first version of this test hard-coded RegistrationOpen, which is the
	// only mode where it passed: a security review found that on a gated instance the registration-mode
	// gate ran before the detour, so an address with an account got 200 "check your email" and one without
	// got 400. Pinning one mode is how a property that holds in half the configurations looks like one
	// that holds.
	for _, mode := range []auth.RegistrationMode{auth.RegistrationOpen, auth.RegistrationInvite} {
		t.Run(string(mode), func(t *testing.T) { unverifiedProviderAddressIsUniform(t, mode) })
	}
}

func unverifiedProviderAddressIsUniform(t *testing.T, mode auth.RegistrationMode) {
	// One instance, two addresses: one that has an account and one that does not. Both presented as
	// unverified by the provider, which is the case an attacker can arrange for any address.
	api, stub := newOAuthAPI(t, mode)

	// Seeded through an invite on a gated instance, since ordinary registration is refused there. The
	// account has to exist for this test to mean anything: the whole question is whether the callback
	// answers differently for an address that has one.
	if mode == auth.RegistrationInvite {
		invite := mintInvite(t, api, map[string]any{"max_uses": 5})
		require.Equal(t, http.StatusAccepted, api.call(http.MethodPost, "/api/v1/auth/register",
			map[string]string{
				"username": "ada", "email": "ada@example.com", "password": testPassword,
				"invite_code": invite.Code,
			}).Code)
		api.confirmAddress("ada@example.com")
	} else {
		api.newAccount("ada", "ada@example.com", "laptop")
	}

	stub.as("google-1", "ada@example.com", false)
	registered := api.authorizeAndCallback(t)

	stub.as("google-2", "newcomer@example.com", false)
	unknown := api.authorizeAndCallback(t)

	assert.Equal(t, registered.Code, unknown.Code, "the status must not differ")
	// Compared with the CSP nonce masked: it is random per request by design, and is the only thing that
	// legitimately differs between two renders of one page.
	assert.Equal(t, withoutNonce(registered.String()), withoutNonce(unknown.String()),
		"the page must not differ")

	// Nothing was linked to the existing account, and no account was created for the unknown address —
	// the two properties the identical page must not have cost.
	var links, users int
	require.NoError(t, api.pool.QueryRow(t.Context(), "SELECT count(*) FROM oauth_identities").Scan(&links))
	assert.Zero(t, links, "an unverified address must never be linked to an existing account")
	require.NoError(t, api.pool.QueryRow(t.Context(), "SELECT count(*) FROM users").Scan(&users))
	assert.Equal(t, 1, users, "no account may be created until the mailed link is followed")
}

// The login-CSRF defense, through the assembled router: the attacker's page renders and the code on it is
// real, and the victim's client still cannot spend it.
func TestOAuthExchangeRefusesACodeFromSomebodyElsesFlow(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	api.newAccount("attacker", "attacker@example.com", "laptop")
	stub.as("google-attacker", "attacker@example.com", true)

	page, _ := api.authorizeAndCallbackBound(t)
	require.Equal(t, http.StatusOK, page.Code, page)
	code := exchangeCodeFromPage(t, page)

	exchanged := api.call(http.MethodPost, "/api/v1/auth/oauth/exchange", map[string]string{
		"code": code, "flow_verifier": mustFlowVerifier(t), "device_id": "victim-laptop",
	})
	require.Equal(t, http.StatusUnauthorized, exchanged.Code, exchanged)

	// Answered exactly as an unknown or expired code is. A distinct status would tell whoever crafted the
	// link that the code was genuine and only the binding refused it.
	assert.Contains(t, exchanged.String(), "invalid or expired sign-in code")
}

// The construction contracts/openapi.yaml documents, followed by hand.
//
// Every other test builds the pair with auth.GenerateOAuthFlowVerifier, and that is exactly how a wrong
// recipe in the contract survives a green suite: the clients that will actually implement this — the CLI
// at M8, the SPA at Phase O — read the document, not the Go helper. So this one mints the pair the way the
// document says to, byte for byte, and never calls the helper at all.
func TestTheDocumentedFlowVerifierConstructionWorks(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	acct := api.newAccount("ada", "ada@example.com", "laptop")
	stub.as("google-1", "ada@example.com", true)

	// Step 1: "nof_" followed by 32 random bytes, base64url without padding — 47 characters.
	buf := make([]byte, 32)
	_, err := rand.Read(buf)
	require.NoError(t, err)
	verifier := "nof_" + base64.RawURLEncoding.EncodeToString(buf)
	require.Len(t, verifier, 47, "the contract promises 47 characters")

	// Step 2: SHA-256 of that whole string, prefix included, base64url without padding — 43 characters.
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	require.Len(t, challenge, 43, "the contract promises 43 characters")

	start := api.call(http.MethodGet,
		"/api/v1/auth/oauth/google/authorize?flow_challenge="+url.QueryEscape(challenge), nil)
	require.Equal(t, http.StatusFound, start.Code, start)

	location, err := url.Parse(start.Header.Get("Location"))
	require.NoError(t, err)
	page := api.call(http.MethodGet, "/api/v1/auth/oauth/google/callback?state="+
		url.QueryEscape(location.Query().Get("state"))+"&code=stub-code", nil)
	require.Equal(t, http.StatusOK, page.Code, page)

	// Step 3: present the verifier itself.
	exchanged := api.call(http.MethodPost, "/api/v1/auth/oauth/exchange", map[string]string{
		"code": exchangeCodeFromPage(t, page), "flow_verifier": verifier, "device_id": "laptop",
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

// A deleted account keeps its address, and every lookup that would have refused the flow earlier skips its
// row — so the person reaches the username form legitimately and cannot get past it. They are told what
// happened rather than "something went wrong on our end", which was untrue and left them nothing to do.
func TestOAuthSignupSaysSoWhenTheAddressIsAlreadyClaimed(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")
	_, err := api.pool.Exec(t.Context(),
		"UPDATE users SET deleted_at = now() WHERE email = $1", "ada@example.com")
	require.NoError(t, err)

	stub.as("google-1", "ada@example.com", true)
	page := api.authorizeAndCallback(t)
	require.Equal(t, http.StatusOK, page.Code, page)
	require.Contains(t, page.String(), "Choose your username", "the dead row is invisible until the insert")

	form := url.Values{"signup_token": {hiddenField(t, page, "signup_token")}, "username": {"reborn"}}
	done := api.call(http.MethodPost, "/oauth/signup", form.Encode(),
		withHeader("Content-Type", "application/x-www-form-urlencoded"))

	require.Equal(t, http.StatusBadRequest, done.Code, done)
	assert.Contains(t, done.String(), "already uses that email address")
	assert.NotContains(t, done.String(), "went wrong on our end",
		"a claimed address is not a server fault, and reporting it as one leaves the person stuck")
}

// A flow cannot even be started without a binding, so there is no unbound path left to aim at.
func TestOAuthAuthorizeRequiresAFlowChallenge(t *testing.T) {
	api, _ := newOAuthAPI(t, auth.RegistrationOpen)

	for name, query := range map[string]string{
		"absent":  "",
		"garbage": "?flow_challenge=!!!!",
		"short":   "?flow_challenge=c2hvcnQ",
	} {
		t.Run(name, func(t *testing.T) {
			resp := api.call(http.MethodGet, "/api/v1/auth/oauth/google/authorize"+query, nil)
			assert.Equal(t, http.StatusBadRequest, resp.Code, resp)
		})
	}
}

// ---------- state and replay, through the router ----------

func TestOAuthCallbackStateIsSingleUseThroughTheRouter(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")
	stub.as("google-1", "ada@example.com", true)

	_, challenge, err := auth.GenerateOAuthFlowVerifier()
	require.NoError(t, err)
	start := api.call(http.MethodGet, "/api/v1/auth/oauth/google/authorize?flow_challenge="+
		url.QueryEscape(auth.OAuthFlowChallengeFor(challenge)), nil)
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
//
// Driven through a real flow rather than a made-up state, because that is what a provider actually sends:
// the state it was given, alongside the error. The state is consumed before the error is considered, which
// is what lets a waiting client be told (oauth_loopback_test.go); this asserts the page half.
func TestOAuthCallbackHandlesAProviderError(t *testing.T) {
	api, _ := newOAuthAPI(t, auth.RegistrationOpen)

	_, challenge, err := auth.GenerateOAuthFlowVerifier()
	require.NoError(t, err)
	start := api.call(http.MethodGet, "/api/v1/auth/oauth/google/authorize?flow_challenge="+
		url.QueryEscape(auth.OAuthFlowChallengeFor(challenge)), nil)
	require.Equal(t, http.StatusFound, start.Code, start)
	location, err := url.Parse(start.Header.Get("Location"))
	require.NoError(t, err)

	page := api.call(http.MethodGet, "/api/v1/auth/oauth/google/callback?error=access_denied&state="+
		url.QueryEscape(location.Query().Get("state")), nil)

	require.Equal(t, http.StatusBadRequest, page.Code)
	assert.Contains(t, page.String(), "not completed")
	assert.NotContains(t, page.String(), "wrong on our end", "a declined consent is not a server fault")
}

// The precedence when both are wrong, stated so it is a decision rather than an accident of ordering.
//
// The state is examined first, so a declined consent whose state has expired or been replayed reads as
// "this link is no longer valid" rather than "you canceled". That is the right way round: the state has
// to be consumed before anything can be reported to a client's listener at all, and "start again" is true
// and actionable for someone whose link is fifteen minutes old however they left the provider.
func TestAnExpiredStateOutranksADeclinedConsent(t *testing.T) {
	api, _ := newOAuthAPI(t, auth.RegistrationOpen)

	page := api.call(http.MethodGet,
		"/api/v1/auth/oauth/google/callback?error=access_denied&state=nos_nonsense", nil)

	require.Equal(t, http.StatusBadRequest, page.Code)
	assert.Contains(t, page.String(), "no longer valid")
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

// withoutNonce masks the per-request CSP nonce so two renders of one page can be compared.
var nonceAttr = regexp.MustCompile(`nonce="[^"]*"`)

func withoutNonce(page string) string {
	return nonceAttr.ReplaceAllString(page, `nonce="…"`)
}
