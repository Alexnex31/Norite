package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/auth"
)

// The join between M9's device flow and M6's provider flow, which is the only path by which an account
// that has no password can sign in on a headless machine — the hole the whole milestone exists to close.

// The done-when for the provider branch: a code issued to a headless client, a provider sign-in completed
// in a browser somewhere else, and a token pair collected by the client that never saw a browser.
func TestADeviceCodeSignInWithAProviderEndToEnd(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	acct := api.newAccount("ada", "ada@example.com", "laptop")
	stub.as("google-1", "ada@example.com", true)

	issued := api.issueDeviceCode(t)
	entry := api.enterUserCode(t, issued.UserCode)

	approval := api.signInOnDevicePageWithGoogle(t, entry)
	api.decideDevice(t, approval, "approve")

	pair := api.pollUntilSignedIn(t, issued.DeviceCode)
	me := api.call(http.MethodGet, "/api/v1/users/@me", nil, withToken(pair.AccessToken))
	require.Equal(t, http.StatusOK, me.Code, me)
	var self struct {
		ID string `json:"id"`
	}
	me.decode(&self)
	assert.Equal(t, acct.ID, self.ID)
}

// The sign-up hop. A brand-new provider identity gets the username form first, and the device it is
// finishing has to survive that — otherwise the flow works for anyone who already has an account and
// hangs forever for everyone else, which is most of the point of having it.
func TestABrandNewAccountCanSignUpFromTheDevicePage(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	stub.as("google-new", "newcomer@example.com", true)

	issued := api.issueDeviceCode(t)
	entry := api.enterUserCode(t, issued.UserCode)

	form := api.followProviderLink(t, entry)
	require.Contains(t, form.String(), "signup_token", "a new identity must get the username form")

	page := api.call(http.MethodPost, "/oauth/signup", formBody(url.Values{
		"signup_token": {signupTokenFrom(t, form.String())}, "username": {"newcomer"},
	}), asForm)
	require.Equal(t, http.StatusOK, page.Code, page)
	require.Contains(t, page.String(), "Approve this device?",
		"a sign-up from the verification page must land on the approval step")

	api.decideDevice(t, deviceTokenFrom(t, page.String()), "approve")

	pair := api.pollUntilSignedIn(t, issued.DeviceCode)
	me := api.call(http.MethodGet, "/api/v1/users/@me", nil, withToken(pair.AccessToken))
	require.Equal(t, http.StatusOK, me.Code)
	assert.Contains(t, me.String(), "newcomer")
}

// The security property of the sign-up hop, and the sibling of M8's
// TestTheSignupFormCannotChooseWhereTheCodeIsDelivered. That form is submitted by whoever is looking at
// the page, so which machine gets authorized cannot come out of its body.
func TestTheSignupFormCannotChooseWhichDeviceItApproves(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	stub.as("google-new", "newcomer@example.com", true)

	// Two authorizations in flight: the one being finished, and an attacker's.
	mine := api.issueDeviceCode(t)
	theirs := api.issueDeviceCode(t)

	entry := api.enterUserCode(t, mine.UserCode)
	form := api.followProviderLink(t, entry)

	theirEntry := api.enterUserCode(t, theirs.UserCode)
	page := api.call(http.MethodPost, "/oauth/signup", formBody(url.Values{
		"signup_token": {signupTokenFrom(t, form.String())},
		"username":     {"newcomer"},
		// Every plausible spelling of "authorize that one instead".
		"dvc":            {"999"},
		"device_token":   {theirEntry},
		"device_code_id": {"999"},
	}), asForm)
	require.Equal(t, http.StatusOK, page.Code, page)

	api.decideDevice(t, deviceTokenFrom(t, page.String()), "approve")

	api.pollUntilSignedIn(t, mine.DeviceCode)

	other := api.call(http.MethodPost, "/api/v1/auth/device/token",
		map[string]string{"device_code": theirs.DeviceCode})
	assert.Equal(t, "authorization_pending", other.errorBody().Code,
		"the form must not have been able to redirect the approval at another device")
}

// The same property one leg earlier: the callback is presented by whoever holds the link, so it may not
// name the device either. Which one is waiting comes out of the state row the callback consumes.
func TestACallbackCannotSupplyItsOwnDevice(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")
	stub.as("google-1", "ada@example.com", true)

	victim := api.issueDeviceCode(t)
	victimEntry := api.enterUserCode(t, victim.UserCode)

	// An ordinary loopback flow, started with no device attached at all.
	verifier, challenge, err := auth.GenerateOAuthFlowVerifier()
	require.NoError(t, err)
	require.NotEmpty(t, verifier)

	start := api.call(http.MethodGet, "/api/v1/auth/oauth/google/authorize?flow_challenge="+
		url.QueryEscape(auth.OAuthFlowChallengeFor(challenge)), nil)
	require.Equal(t, http.StatusFound, start.Code, start)
	location, err := url.Parse(start.Header.Get("Location"))
	require.NoError(t, err)

	// ...and a callback that tries to attach one.
	back := api.call(http.MethodGet, "/api/v1/auth/oauth/google/callback?state="+
		url.QueryEscape(location.Query().Get("state"))+"&code=stub-code&device_token="+
		url.QueryEscape(victimEntry), nil)
	require.Equal(t, http.StatusOK, back.Code, back)
	assert.NotContains(t, back.String(), "Approve this device?",
		"the callback must ignore a device named on its own URL")

	poll := api.call(http.MethodPost, "/api/v1/auth/device/token",
		map[string]string{"device_code": victim.DeviceCode})
	assert.Equal(t, "authorization_pending", poll.errorBody().Code)
}

// A provider sign-in authorizes nothing on its own, exactly as a password sign-in does not. This is the
// step the flow's one real attack has to get past, so it holds on both branches or on neither.
func TestAProviderSignInStillNeedsApproval(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")
	stub.as("google-1", "ada@example.com", true)

	issued := api.issueDeviceCode(t)
	entry := api.enterUserCode(t, issued.UserCode)
	api.signInOnDevicePageWithGoogle(t, entry)

	poll := api.call(http.MethodPost, "/api/v1/auth/device/token",
		map[string]string{"device_code": issued.DeviceCode})
	assert.Equal(t, "authorization_pending", poll.errorBody().Code)
}

// A device flow mints no exchange code. There would be nobody to redeem it — the waiting client already
// holds the credential it will use — so one would sit in the table until it expired, redeemable by
// whoever had seen it.
func TestADeviceFlowMintsNoExchangeCode(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")
	stub.as("google-1", "ada@example.com", true)

	issued := api.issueDeviceCode(t)
	entry := api.enterUserCode(t, issued.UserCode)
	approval := api.signInOnDevicePageWithGoogle(t, entry)
	api.decideDevice(t, approval, "approve")

	var codes int
	require.NoError(t, api.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM oauth_exchange_codes`).Scan(&codes))
	assert.Zero(t, codes, "a device flow has no client waiting on a code")

	api.pollUntilSignedIn(t, issued.DeviceCode)
}

// Exactly one binding, never both and never neither. An optional binding is not a binding — the whole
// reason GenerateOAuthFlowVerifier's rule exists — and a flow claiming both would have two exits and a
// question about which one wins.
func TestAuthorizeRequiresExactlyOneBinding(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	stub.as("google-1", "ada@example.com", true)

	issued := api.issueDeviceCode(t)
	entry := api.enterUserCode(t, issued.UserCode)
	_, challenge, err := auth.GenerateOAuthFlowVerifier()
	require.NoError(t, err)
	flow := auth.OAuthFlowChallengeFor(challenge)

	for _, tc := range []struct{ why, query string }{
		{"neither", ""},
		{"both", "?flow_challenge=" + url.QueryEscape(flow) + "&device_token=" + url.QueryEscape(entry)},
		{"a device token that is not one", "?device_token=not-a-token"},
		{"a device flow that also names a loopback listener",
			"?device_token=" + url.QueryEscape(entry) +
				"&client_redirect_uri=" + url.QueryEscape("http://127.0.0.1:51763/callback")},
	} {
		resp := api.call(http.MethodGet, "/api/v1/auth/oauth/google/authorize"+tc.query, nil)
		assert.Equal(t, http.StatusBadRequest, resp.Code, "%s: %s", tc.why, resp)
	}

	// And no state row was minted for any of them: /authorize is unauthenticated, so a refusal costs a
	// parse and nothing the sweeper has to clean up.
	var states int
	require.NoError(t, api.pool.QueryRow(t.Context(), `SELECT count(*) FROM oauth_states`).Scan(&states))
	assert.Zero(t, states)
}

// The verification page offers what the instance actually has configured, which is how somebody with no
// password reaches this flow at all.
func TestTheVerificationPageOffersTheConfiguredProviders(t *testing.T) {
	api, _ := newOAuthAPI(t, auth.RegistrationOpen)
	issued := api.issueDeviceCode(t)

	page := api.call(http.MethodPost, "/device", formBody(url.Values{"user_code": {issued.UserCode}}), asForm)
	require.Equal(t, http.StatusOK, page.Code, page)
	assert.Contains(t, page.String(), ">Google<")
	assert.NotContains(t, page.String(), ">GitHub<", "only what this instance configured")
}

// And an instance with no provider configured at all shows the password form and nothing broken — which
// is the shape a self-hosted instance runs in until somebody registers an OAuth app.
func TestTheVerificationPageWithoutProvidersIsStillUsable(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)
	issued := api.issueDeviceCode(t)

	page := api.call(http.MethodPost, "/device", formBody(url.Values{"user_code": {issued.UserCode}}), asForm)
	require.Equal(t, http.StatusOK, page.Code, page)
	assert.NotContains(t, page.String(), "Sign in with a provider")
	assert.Contains(t, page.String(), `name="password"`)
}

// ---------- helpers ----------

// followProviderLink walks the provider button on the sign-in step, through the stub, and back.
func (a *api) followProviderLink(t *testing.T, entryToken string) *response {
	t.Helper()

	start := a.call(http.MethodGet,
		"/api/v1/auth/oauth/google/authorize?device_token="+url.QueryEscape(entryToken), nil)
	require.Equal(t, http.StatusFound, start.Code, "authorize must redirect to the provider: %s", start)

	location, err := url.Parse(start.Header.Get("Location"))
	require.NoError(t, err)
	state := location.Query().Get("state")
	require.NotEmpty(t, state)

	return a.call(http.MethodGet,
		"/api/v1/auth/oauth/google/callback?state="+url.QueryEscape(state)+"&code=stub-code", nil)
}

// signInOnDevicePageWithGoogle does the provider branch of the second step and returns the approval
// continuation.
func (a *api) signInOnDevicePageWithGoogle(t *testing.T, entryToken string) string {
	t.Helper()
	page := a.followProviderLink(t, entryToken)
	require.Equal(t, http.StatusOK, page.Code, page)
	require.Contains(t, page.String(), "Approve this device?", "expected the approval step: %s", page)
	return deviceTokenFrom(t, page.String())
}

func signupTokenFrom(t *testing.T, body string) string {
	t.Helper()
	_, after, found := strings.Cut(body, `name="signup_token" value="`)
	require.True(t, found, "the page must carry a signup token:\n%s", body)
	token, _, _ := strings.Cut(after, `"`)
	require.NotEmpty(t, token)
	return token
}
