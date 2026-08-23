package main

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/auth"
)

// The browser half of M9: the pages somebody opens on a second device. Driven through the assembled
// router, so the CSP, the rate-limit bucket and the templates are the real ones.

// The done-when for the password branch, end to end: a code issued to a headless client becomes a token
// pair for that client, and the only thing that crossed between them was eight characters a person typed.
func TestADeviceCodeSignInWithAPasswordEndToEnd(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)
	acct := api.newAccount("ada", "ada@example.com", "laptop")
	issued := api.issueDeviceCode(t)

	entry := api.enterUserCode(t, issued.UserCode)
	approval := api.signInOnDevicePage(t, entry, "ada@example.com", testPassword)
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

// The step the whole page exists for. Approving is a separate request from signing in, so a person who
// was talked into entering somebody else's code has one more chance — on a page that names the device and
// shows the code back — to notice.
func TestSigningInDoesNotApproveOnItsOwn(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")
	issued := api.issueDeviceCode(t)

	entry := api.enterUserCode(t, issued.UserCode)
	api.signInOnDevicePage(t, entry, "ada@example.com", testPassword)

	resp := api.call(http.MethodPost, "/api/v1/auth/device/token",
		map[string]string{"device_code": issued.DeviceCode})
	require.Equal(t, http.StatusBadRequest, resp.Code, resp)
	assert.Equal(t, "authorization_pending", resp.errorBody().Code,
		"a completed sign-in must not authorize anything by itself")
}

// The approval page has to carry what somebody needs in order to refuse: which device is asking, and the
// code to compare against the screen that showed it.
func TestTheApprovalPageNamesTheDeviceAndShowsTheCode(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")
	issued := api.issueDeviceCode(t)

	entry := api.enterUserCode(t, issued.UserCode)
	page := api.call(http.MethodPost, "/device/signin", formBody(url.Values{
		"device_token": {entry}, "email": {"ada@example.com"}, "password": {testPassword},
	}), asForm)
	require.Equal(t, http.StatusOK, page.Code, page)

	assert.Contains(t, page.String(), "archlinux", "the page must name the device asking")
	assert.Contains(t, page.String(), issued.UserCode, "and show the code back for comparison")
	assert.Contains(t, page.String(), "press Deny", "and say what to do about a code somebody sent you")
}

// The security property of the two-token split: knowing a code is not the same as being somebody. An
// entry token presented at the approval step must be refused, or the sign-in step would be decorative.
func TestApprovalRequiresTheTokenFromAfterSigningIn(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")
	issued := api.issueDeviceCode(t)

	entry := api.enterUserCode(t, issued.UserCode)

	resp := api.call(http.MethodPost, "/device/approve", formBody(url.Values{
		"device_token": {entry}, "decision": {"approve"},
	}), asForm)
	require.Equal(t, http.StatusBadRequest, resp.Code, resp)

	poll := api.call(http.MethodPost, "/api/v1/auth/device/token",
		map[string]string{"device_code": issued.DeviceCode})
	assert.Equal(t, "authorization_pending", poll.errorBody().Code,
		"nothing may have been authorized by an entry token")
}

// Deny is final and it is prompt, which is the whole reason it exists rather than letting a code expire.
func TestDenyingStopsTheWaitingClientAtOnce(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")
	issued := api.issueDeviceCode(t)

	entry := api.enterUserCode(t, issued.UserCode)
	approval := api.signInOnDevicePage(t, entry, "ada@example.com", testPassword)
	api.decideDevice(t, approval, "deny")

	resp := api.call(http.MethodPost, "/api/v1/auth/device/token",
		map[string]string{"device_code": issued.DeviceCode})
	require.Equal(t, http.StatusBadRequest, resp.Code, resp)
	assert.Equal(t, "access_denied", resp.errorBody().Code)
}

// A submission that is neither approve nor deny denies. Failing closed matters more here than anywhere
// else on the page: the failure open is authorizing a device nobody chose to authorize.
func TestAnUnrecognizedDecisionDenies(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")
	issued := api.issueDeviceCode(t)

	entry := api.enterUserCode(t, issued.UserCode)
	approval := api.signInOnDevicePage(t, entry, "ada@example.com", testPassword)

	resp := api.call(http.MethodPost, "/device/approve", formBody(url.Values{
		"device_token": {approval}, "decision": {"Approve please"},
	}), asForm)
	require.Equal(t, http.StatusOK, resp.Code, resp)

	poll := api.call(http.MethodPost, "/api/v1/auth/device/token",
		map[string]string{"device_code": issued.DeviceCode})
	assert.Equal(t, "access_denied", poll.errorBody().Code)
}

// The page's answer to a code it cannot act on is one answer, whatever the reason. Anything else is an
// oracle for somebody working through codes.
func TestEveryUnusableCodeGetsTheSameAnswer(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")

	denied := api.issueDeviceCode(t)
	entry := api.enterUserCode(t, denied.UserCode)
	approval := api.signInOnDevicePage(t, entry, "ada@example.com", testPassword)
	api.decideDevice(t, approval, "deny")

	var bodies []string
	for _, code := range []string{
		"BCDF-GHJK",     // never issued
		denied.UserCode, // decided already
		"nonsense",      // not a code at all
	} {
		resp := api.call(http.MethodPost, "/device", formBody(url.Values{"user_code": {code}}), asForm)
		require.Equal(t, http.StatusBadRequest, resp.Code, resp)
		bodies = append(bodies, resp.String())
	}
	// Byte-identical once the two things that legitimately differ are taken out: the per-request CSP
	// nonce, and the echo of whatever was typed. Neither says anything about the code that was entered.
	assert.Equal(t, scrubPage(bodies[0]), scrubPage(bodies[1]),
		"a live-but-decided code must look exactly like one that never existed")
	assert.Equal(t, scrubPage(bodies[0]), scrubPage(bodies[2]),
		"and so must a value that is not a code at all")
}

// Rule 19's sibling on a page rather than a terminal. The device name is the one value here somebody else
// chose, and it lands inside a paragraph on a page that asks for a decision.
func TestAHostileDeviceNameCannotEscapeThePage(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")

	resp := api.call(http.MethodPost, "/api/v1/auth/device/code", map[string]string{
		"device_id":   "waiting-device",
		"device_name": `<script>alert(1)</script>`,
	})
	require.Equal(t, http.StatusOK, resp.Code, resp)
	var issued issuedCode
	resp.decode(&issued)

	page := api.call(http.MethodPost, "/device", formBody(url.Values{"user_code": {issued.UserCode}}), asForm)
	require.Equal(t, http.StatusOK, page.Code, page)
	assert.NotContains(t, page.String(), "<script>alert(1)</script>")
	assert.Contains(t, page.String(), "&lt;script&gt;")
}

// The entry form prefills from a query parameter and does nothing else with it, which is the distance
// between a convenience and verification_uri_complete. A value that is not a code is dropped rather than
// echoed, so the page never shows somebody their own arbitrary string back.
func TestThePrefillParameterOnlyPrefills(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)
	issued := api.issueDeviceCode(t)

	page := api.call(http.MethodGet, "/device?code="+url.QueryEscape(strings.ToLower(issued.UserCode)), nil)
	require.Equal(t, http.StatusOK, page.Code, page)
	assert.Contains(t, page.String(), issued.UserCode, "a real code is normalized and prefilled")
	assert.Contains(t, page.String(), "Continue", "and the form still has to be submitted")

	page = api.call(http.MethodGet, "/device?code=Please+sign+in+here", nil)
	require.Equal(t, http.StatusOK, page.Code, page)
	assert.NotContains(t, page.String(), "Please sign in here")

	// And prefilling has authorized nothing, which is the property the parameter is refused for elsewhere.
	poll := api.call(http.MethodPost, "/api/v1/auth/device/token",
		map[string]string{"device_code": issued.DeviceCode})
	assert.Equal(t, "authorization_pending", poll.errorBody().Code)
}

// These pages get the CSP override every server-rendered page here gets, and they need it: without
// form-action 'self' the API's global policy would render the form and then forbid it from submitting.
func TestTheDevicePagesCarryThePageCSP(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)

	page := api.call(http.MethodGet, "/device", nil)
	require.Equal(t, http.StatusOK, page.Code)

	csp := page.Header.Get("Content-Security-Policy")
	assert.Contains(t, csp, "form-action 'self'")
	assert.Contains(t, csp, "default-src 'none'")
	assert.NotContains(t, csp, "script-src", "these pages run no JavaScript and must not be allowed any")
	assert.Equal(t, "no-referrer", page.Header.Get("Referrer-Policy"))
	assert.Contains(t, page.Header.Get("Cache-Control"), "no-store")
}

// ---------- helpers ----------

// scrubPage removes the two values that differ between two renderings of the same page for reasons that
// carry no information: the per-request nonce, and the echo of what somebody typed into the form.
func scrubPage(body string) string {
	body = regexp.MustCompile(`nonce="[^"]*"`).ReplaceAllString(body, `nonce=""`)
	return regexp.MustCompile(`name="user_code" type="text" value="[^"]*"`).
		ReplaceAllString(body, `name="user_code" type="text" value=""`)
}

// asForm sends a form-encoded body, which is what these pages take.
func asForm(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }

func formBody(v url.Values) string { return v.Encode() }

// enterUserCode does the first step and returns the continuation the page handed back.
func (a *api) enterUserCode(t *testing.T, userCode string) string {
	t.Helper()
	resp := a.call(http.MethodPost, "/device", formBody(url.Values{"user_code": {userCode}}), asForm)
	require.Equal(t, http.StatusOK, resp.Code, resp)
	return deviceTokenFrom(t, resp.String())
}

// signInOnDevicePage does the second step and returns the approval continuation.
func (a *api) signInOnDevicePage(t *testing.T, entryToken, email, password string) string {
	t.Helper()
	resp := a.call(http.MethodPost, "/device/signin", formBody(url.Values{
		"device_token": {entryToken}, "email": {email}, "password": {password},
	}), asForm)
	require.Equal(t, http.StatusOK, resp.Code, resp)
	return deviceTokenFrom(t, resp.String())
}

func (a *api) decideDevice(t *testing.T, approvalToken, decision string) {
	t.Helper()
	resp := a.call(http.MethodPost, "/device/approve", formBody(url.Values{
		"device_token": {approvalToken}, "decision": {decision},
	}), asForm)
	require.Equal(t, http.StatusOK, resp.Code, resp)
}

// pollUntilSignedIn spends the device code, stepping the clock past the interval the way a real client
// waits rather than sleeping through it.
func (a *api) pollUntilSignedIn(t *testing.T, deviceCode string) tokenPair {
	t.Helper()
	resp := a.call(http.MethodPost, "/api/v1/auth/device/token",
		map[string]string{"device_code": deviceCode})
	require.Equal(t, http.StatusOK, resp.Code, "the approved code must redeem: %s", resp)

	var pair tokenPair
	resp.decode(&pair)
	require.NotEmpty(t, pair.AccessToken)
	return pair
}

// deviceTokenFrom pulls the continuation out of a rendered page, which is the only place it exists.
func deviceTokenFrom(t *testing.T, body string) string {
	t.Helper()
	_, after, found := strings.Cut(body, `name="device_token" value="`)
	require.True(t, found, "the page must carry a continuation:\n%s", body)
	token, _, _ := strings.Cut(after, `"`)
	require.NotEmpty(t, token)
	return token
}

// Deny is the one recovery path this flow promises (ADR 0028, §14.21), and the way somebody most often
// reaches it is by approving and realizing a second later. Until this was fixed that path did nothing at
// all and still rendered "nothing was signed in" — the page telling a person they were safe at exactly the
// moment they were not.
func TestDenyingAfterApprovingStopsTheDeviceAndSaysSo(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")
	issued := api.issueDeviceCode(t)

	entry := api.enterUserCode(t, issued.UserCode)
	approval := api.signInOnDevicePage(t, entry, "ada@example.com", testPassword)
	api.decideDevice(t, approval, "approve")

	// The back button: the approval page is still in history and its token is still good.
	resp := api.call(http.MethodPost, "/device/approve", formBody(url.Values{
		"device_token": {approval}, "decision": {"deny"},
	}), asForm)
	require.Equal(t, http.StatusOK, resp.Code, resp)
	assert.Contains(t, resp.String(), "has been stopped")
	assert.NotContains(t, resp.String(), "Nothing was signed in",
		"the page must not claim nothing happened when something did")

	// And it is true: the waiting client gets nothing.
	poll := api.call(http.MethodPost, "/api/v1/auth/device/token",
		map[string]string{"device_code": issued.DeviceCode})
	require.Equal(t, http.StatusBadRequest, poll.Code, poll)
	assert.Equal(t, "expired_token", poll.errorBody().Code)
}

// And when it genuinely is too late, that is what it says. Nothing on this page can reach a session that
// already exists, so promising otherwise would be the same lie in the other direction.
func TestDenyingAfterTheDeviceCollectedSaysItIsTooLate(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")
	issued := api.issueDeviceCode(t)

	entry := api.enterUserCode(t, issued.UserCode)
	approval := api.signInOnDevicePage(t, entry, "ada@example.com", testPassword)
	api.decideDevice(t, approval, "approve")
	api.pollUntilSignedIn(t, issued.DeviceCode)

	resp := api.call(http.MethodPost, "/device/approve", formBody(url.Values{
		"device_token": {approval}, "decision": {"deny"},
	}), asForm)
	require.Equal(t, http.StatusOK, resp.Code, resp)
	assert.Contains(t, resp.String(), "already signed in")
	assert.Contains(t, resp.String(), "device list", "and say what can still be done about it")
}

// Fail-closed in the handler is undone if the markup fails open. A form submitted without a button being
// chosen sends the first submit button in DOM order, so Deny has to be first.
func TestTheApprovalFormsDefaultButtonDenies(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")
	issued := api.issueDeviceCode(t)

	entry := api.enterUserCode(t, issued.UserCode)
	page := api.call(http.MethodPost, "/device/signin", formBody(url.Values{
		"device_token": {entry}, "email": {"ada@example.com"}, "password": {testPassword},
	}), asForm)
	require.Equal(t, http.StatusOK, page.Code, page)

	deny := strings.Index(page.String(), `value="deny"`)
	approve := strings.Index(page.String(), `value="approve"`)
	require.Positive(t, deny)
	require.Positive(t, approve)
	assert.Less(t, deny, approve,
		"Deny must be the first submit button, or an implicit submission approves")
}

// The device name is the one value on these pages somebody else chose, and it lands in a sentence above a
// warning this flow depends on being read in the right order. html/template stops markup and nothing else:
// it passes C0 controls and the bidi overrides straight through, and those reorder what is rendered.
func TestAHostileDeviceNameCannotReorderTheApprovalPage(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")

	resp := api.call(http.MethodPost, "/api/v1/auth/device/code", map[string]string{
		"device_id": "waiting-device",
		// A right-to-left override and its pop, escaped rather than written literally so this file
		// cannot itself be misread — and a carriage return for the other half of the same class.
		"device_name": "laptop\u202egnihtemos\u202c\r\nelse",
	})
	require.Equal(t, http.StatusOK, resp.Code, resp)
	var issued issuedCode
	resp.decode(&issued)

	entry := api.enterUserCode(t, issued.UserCode)
	page := api.call(http.MethodPost, "/device/signin", formBody(url.Values{
		"device_token": {entry}, "email": {"ada@example.com"}, "password": {testPassword},
	}), asForm)
	require.Equal(t, http.StatusOK, page.Code, page)

	body := page.String()
	name := body[strings.Index(body, "<strong>"):strings.Index(body, "</strong>")]
	for _, bad := range []string{"\u202e", "\u202c", "\r", "\x00"} {
		assert.NotContains(t, name, bad,
			"nothing that reorders or controls rendered text may reach this page")
	}
	assert.Contains(t, name, "laptop", "and the legible part of the name survives")
	assert.Contains(t, name, "�", "with the removal marked rather than silent")
}

// A device code that expired and was swept while somebody was still on the page must answer the same way
// one that expired a moment ago does. It used to be a 500: the continuation outlives the row it names, so
// the click on a provider button reached a foreign key pointing at nothing.
func TestAProviderClickAfterTheCodeWasSweptSaysToStartAgain(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	stub.as("google-1", "ada@example.com", true)

	issued := api.issueDeviceCode(t)
	entry := api.enterUserCode(t, issued.UserCode)

	// The sweeper, exactly: expired rows deleted regardless of state.
	_, err := api.pool.Exec(t.Context(), `DELETE FROM device_codes`)
	require.NoError(t, err)

	resp := api.call(http.MethodGet,
		"/api/v1/auth/oauth/google/authorize?device_token="+url.QueryEscape(entry), nil)
	require.Equal(t, http.StatusBadRequest, resp.Code, "a swept code must not be a 500: %s", resp)

	var states int
	require.NoError(t, api.pool.QueryRow(t.Context(), `SELECT count(*) FROM oauth_states`).Scan(&states))
	assert.Zero(t, states, "and no state row is left behind for the sweeper")
}

// The JSON completion endpoint cannot finish a device flow — that one ends on an approval page in a
// browser. It used to create the account anyway and answer 200 with an empty code, which the contract
// marks required and which a client would then try to redeem.
func TestTheJSONCompletionEndpointRefusesADeviceSignup(t *testing.T) {
	api, stub := newOAuthAPI(t, auth.RegistrationOpen)
	stub.as("google-new", "newcomer@example.com", true)

	issued := api.issueDeviceCode(t)
	entry := api.enterUserCode(t, issued.UserCode)
	form := api.followProviderLink(t, entry)
	token := signupTokenFrom(t, form.String())

	resp := api.call(http.MethodPost, "/api/v1/auth/oauth/complete",
		map[string]string{"signup_token": token, "username": "newcomer"})
	require.Equal(t, http.StatusBadRequest, resp.Code, resp)
	assert.Contains(t, resp.errorBody().Message, "device verification page")

	// And nothing was created on the way to refusing.
	var users int
	require.NoError(t, api.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM users WHERE username = 'newcomer'`).Scan(&users))
	assert.Zero(t, users)
}

// The approval page says who is about to be authorized, not just what. It read "your account" for its
// whole first draft, which is a certainty the page does not have: somebody can reach this screen signed in
// as an account they did not mean to use — a second provider identity, or one an attacker authenticated as
// after obtaining their code. This line is the only place that mismatch can surface before the decision.
func TestTheApprovalPageNamesTheAccountBeingAuthorized(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")
	issued := api.issueDeviceCode(t)

	entry := api.enterUserCode(t, issued.UserCode)
	page := api.call(http.MethodPost, "/device/signin", formBody(url.Values{
		"device_token": {entry}, "email": {"ada@example.com"}, "password": {testPassword},
	}), asForm)
	require.Equal(t, http.StatusOK, page.Code, page)

	assert.Contains(t, page.String(), "ada (ada@example.com)")
	assert.NotContains(t, page.String(), "to your account",
		"the page must name the account rather than assert it is theirs")
}

// The recovery path ADR 0028 promises has to be reachable, not merely implemented. It was neither
// linked nor described from the page somebody actually ends on — and a phone will not re-render a POST
// response, so the back button does not get there either. Approving and then realizing is the way people
// arrive at Deny, so the way out belongs on the page that says it worked.
func TestTheApprovedPageCanStillStopTheDevice(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)
	api.newAccount("ada", "ada@example.com", "laptop")
	issued := api.issueDeviceCode(t)

	entry := api.enterUserCode(t, issued.UserCode)
	approval := api.signInOnDevicePage(t, entry, "ada@example.com", testPassword)

	approved := api.call(http.MethodPost, "/device/approve", formBody(url.Values{
		"device_token": {approval}, "decision": {"approve"},
	}), asForm)
	require.Equal(t, http.StatusOK, approved.Code, approved)
	assert.Contains(t, approved.String(), "stop it", "the way out must be on the page, not only in the API")

	// And following it from that page — the token it carries — actually stops the device.
	stopped := api.call(http.MethodPost, "/device/approve", formBody(url.Values{
		"device_token": {deviceTokenFrom(t, approved.String())}, "decision": {"deny"},
	}), asForm)
	require.Equal(t, http.StatusOK, stopped.Code, stopped)
	assert.Contains(t, stopped.String(), "has been stopped")

	poll := api.call(http.MethodPost, "/api/v1/auth/device/token",
		map[string]string{"device_code": issued.DeviceCode})
	require.Equal(t, http.StatusBadRequest, poll.Code, poll)
	assert.Equal(t, "expired_token", poll.errorBody().Code)
}
