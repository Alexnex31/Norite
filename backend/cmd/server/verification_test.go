package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/mail"
)

// M10's headline done-when: registering an address that already has an account is indistinguishable, in
// status, body and timing, from registering a new one.
//
// This was the one endpoint in the API that disclosed account existence — login, reset and both OAuth
// refusals are deliberately uniform — and it did so because there was no way to accept a registration and
// sort it out by mail. That is what email verification provides.
func TestRegisteringATakenAddressIsIndistinguishable(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	a.newAccount("ada", "ada@example.com", "laptop")

	taken := a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "someone", "email": "ada@example.com", "password": testPassword,
	})
	fresh := a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "another", "email": "nobody@example.com", "password": testPassword,
	})

	assert.Equal(t, taken.Code, fresh.Code, "the status must not differ")
	assert.Equal(t, taken.String(), fresh.String(), "the body must not differ")

	// Headers too, minus the ones that differ by construction. X-Request-Id is unique per request by
	// design, and a response whose length matched but whose header set did not would still be an oracle.
	assert.Equal(t, headerNames(taken), headerNames(fresh), "the header set must not differ")
}

// headerNames lists a response's header names, sorted, ignoring the per-request ones.
func headerNames(r *response) []string {
	var names []string
	for name := range r.Header {
		if name == "X-Request-Id" {
			continue
		}
		names = append(names, name)
	}
	// A stable order, since map iteration is not.
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return names
}

// The half that makes the silence honest rather than merely quiet: somebody has to learn the two cases
// differ, and the only party entitled to know is whoever controls the address.
func TestATakenAddressIsToldAndAFreshOneIsInvited(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	a.newAccount("ada", "ada@example.com", "laptop")

	before := a.mail.countOfKind(mail.KindEmailVerification)

	a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "someone", "email": "ada@example.com", "password": testPassword,
	})
	notice, ok := a.mail.lastOfKind(mail.KindRegistrationNotice)
	require.True(t, ok, "the address that already has an account must be told")
	assert.Equal(t, "ada@example.com", notice.To)
	assert.Equal(t, before, a.mail.countOfKind(mail.KindEmailVerification),
		"no verification link may be sent for an account that was not created")

	a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "another", "email": "nobody@example.com", "password": testPassword,
	})
	verification, ok := a.mail.lastOfKind(mail.KindEmailVerification)
	require.True(t, ok, "a new account must be sent a link")
	assert.Equal(t, "nobody@example.com", verification.To)
}

// The timing half of the claim, and the mechanism behind it rather than a wall-clock measurement.
//
// argon2id runs in both branches — 64 MiB and tens of milliseconds — which swamps the single insert the
// two differ by. Moving the hash below the address check would reintroduce the oracle in a form no response
// body shows, so what this asserts is that the expensive work happens either way.
//
// Measured as a ratio rather than an absolute, because absolute timings on a shared CI runner are noise.
// Deliberately loose: this catches "one branch skips argon2id entirely", which is the regression that
// matters, and does not pretend to detect a few microseconds.
func TestBothRegistrationBranchesDoTheExpensiveWork(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	a.newAccount("ada", "ada@example.com", "laptop")

	// Valid usernames, which matters more than it looks: an invalid one is refused before the password is
	// hashed, so both branches would fail identically at validation and this test would measure nothing.
	// It was written with "taken@x" and passed while asserting nothing at all.
	taken := timeRequest(t, a, "takenbranch", "ada@example.com")
	fresh := timeRequest(t, a, "freshbranch", "nobody@example.com")

	ratio := float64(taken) / float64(fresh)
	assert.Greater(t, ratio, 0.2,
		"the taken-address branch must not be an order of magnitude cheaper: taken=%v fresh=%v", taken, fresh)
	assert.Less(t, ratio, 5.0,
		"nor an order of magnitude more expensive: taken=%v fresh=%v", taken, fresh)
}

// An unverified account cannot sign in, and is refused with the *same* answer a wrong password gets.
//
// The distinct answer is the obvious design and it reopens the oracle registration just closed, in two
// requests: register an address with a password of your choosing, then log in with it. If the address was
// free an account now exists with that password and the login says "unverified"; if it was taken nothing
// was created and the same login says "wrong password". That was measured — 403 against 401 — before this
// test was written, which is why it compares the two rather than asserting a nice message.
func TestAnUnverifiedAccountIsRefusedLikeAWrongPassword(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	a.newAccount("ada", "ada@example.com", "laptop")

	// The probe. Both registrations are accepted identically; the question is whether the logins are.
	const probePassword = "a-password-the-prober-chose"
	require.Equal(t, http.StatusAccepted, a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "probe1", "email": "ada@example.com", "password": probePassword,
	}).Code)
	taken := a.call(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "ada@example.com", "password": probePassword, "device_id": "d",
	})

	require.Equal(t, http.StatusAccepted, a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "probe2", "email": "free@example.com", "password": probePassword,
	}).Code)
	free := a.call(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "free@example.com", "password": probePassword, "device_id": "d",
	})

	assert.Equal(t, http.StatusUnauthorized, taken.Code, taken)
	assert.Equal(t, taken.Code, free.Code, "two requests must not enumerate an address")
	// Compared field by field: request_id is unique per request by design and is the only thing that may
	// differ between two responses.
	assert.Equal(t, taken.errorBody().Code, free.errorBody().Code, "nor may the code differ")
	assert.Equal(t, taken.errorBody().Message, free.errorBody().Message, "nor the message")

	// The difference goes to the mailbox, as everything else in this milestone does: the person who owns
	// the address is told why they could not sign in, and given a fresh link.
	reminder, ok := a.mail.lastOfKind(mail.KindEmailVerification)
	require.True(t, ok)
	assert.Equal(t, "free@example.com", reminder.To)
	assert.Contains(t, reminder.Body, "not been confirmed")

	// And confirming makes it work.
	a.confirmAddress("free@example.com")
	a.call(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "free@example.com", "password": probePassword, "device_id": "d",
	})
}

// A wrong password on an unverified account queues nothing, so the reminder cannot be used to mail-bomb
// somebody by guessing at their address.
func TestAWrongPasswordOnAnUnverifiedAccountSendsNothing(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)

	require.Equal(t, http.StatusAccepted, a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "ada", "email": "ada@example.com", "password": testPassword,
	}).Code)
	before := a.mail.countOfKind(mail.KindEmailVerification)

	for range 3 {
		resp := a.call(http.MethodPost, "/api/v1/auth/login", map[string]string{
			"email": "ada@example.com", "password": "not-the-right-password", "device_id": "d",
		})
		require.Equal(t, http.StatusUnauthorized, resp.Code)
	}
	assert.Equal(t, before, a.mail.countOfKind(mail.KindEmailVerification),
		"a reminder may only follow a correct password")
}

// The link works once. A leaked mail — forwarded, backed up, read from a shared mailbox — must not be
// redeemable a second time.
func TestAVerificationLinkIsSingleUse(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)

	require.Equal(t, http.StatusAccepted, a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "ada", "email": "ada@example.com", "password": testPassword,
	}).Code)

	msg, ok := a.mail.lastOfKind(mail.KindEmailVerification)
	require.True(t, ok)
	token := tokenFromBody(t, msg.Body, "/verify?token=")

	first := a.postForm("/verify", "token="+token)
	require.Equal(t, http.StatusOK, first.Code, first)

	second := a.postForm("/verify", "token="+token)
	assert.Equal(t, http.StatusBadRequest, second.Code, second)
	assert.Contains(t, second.String(), "no longer valid")
}

// The GET does not verify. Anything that follows links in mail — scanning gateways, chat-client
// previewers, antivirus — would otherwise confirm an address the person never acted on, and spend the link
// before they clicked it. Rule 4 says the same thing for a different reason.
func TestOpeningTheVerificationPageDoesNotVerify(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)

	require.Equal(t, http.StatusAccepted, a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "ada", "email": "ada@example.com", "password": testPassword,
	}).Code)

	msg, ok := a.mail.lastOfKind(mail.KindEmailVerification)
	require.True(t, ok)
	token := tokenFromBody(t, msg.Body, "/verify?token=")

	opened := a.call(http.MethodGet, "/verify?token="+token, nil)
	require.Equal(t, http.StatusOK, opened.Code, opened)

	// Still cannot log in, because nothing was confirmed. Refused as a wrong password would be — see
	// TestAnUnverifiedAccountIsRefusedLikeAWrongPassword for why that answer is uniform.
	refused := a.call(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "ada@example.com", "password": testPassword, "device_id": "laptop",
	})
	assert.Equal(t, http.StatusUnauthorized, refused.Code, refused)
}

// An instance with no relay cannot verify anything, so it creates accounts verified rather than refusing
// to register anybody. The accepted limitation this milestone ships with — pinned so it stays deliberate
// rather than becoming accidental, and so a future change that quietly makes registration unusable on such
// an instance fails here.
func TestWithNoRelayRegistrationAutoVerifies(t *testing.T) {
	a := newAPIWithoutMail(t, auth.RegistrationOpen)

	require.Equal(t, http.StatusAccepted, a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "ada", "email": "ada@example.com", "password": testPassword,
	}).Code)

	// No confirmation step, and login works.
	a.login("ada@example.com", "laptop")
}

// The resend path, and that it says nothing about what it found.
func TestRequestingVerificationAnswersIdenticallyForAnyAddress(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	a.newAccount("ada", "ada@example.com", "laptop")

	verified := a.call(http.MethodPost, "/api/v1/auth/verify/request", map[string]string{
		"email": "ada@example.com",
	})
	unknown := a.call(http.MethodPost, "/api/v1/auth/verify/request", map[string]string{
		"email": "nobody@example.com",
	})

	assert.Equal(t, http.StatusAccepted, verified.Code, verified)
	assert.Equal(t, verified.Code, unknown.Code)
	assert.Equal(t, verified.String(), unknown.String())
}

// Requesting again supersedes the earlier link, so the newest is the only one that works.
func TestResendingSupersedesTheEarlierLink(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)

	require.Equal(t, http.StatusAccepted, a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "ada", "email": "ada@example.com", "password": testPassword,
	}).Code)
	first, ok := a.mail.lastOfKind(mail.KindEmailVerification)
	require.True(t, ok)

	require.Equal(t, http.StatusAccepted, a.call(http.MethodPost, "/api/v1/auth/verify/request",
		map[string]string{"email": "ada@example.com"}).Code)
	second, ok := a.mail.lastOfKind(mail.KindEmailVerification)
	require.True(t, ok)
	require.NotEqual(t, first.Body, second.Body, "a resend must issue a new token")

	stale := a.postForm("/verify", "token="+tokenFromBody(t, first.Body, "/verify?token="))
	assert.Equal(t, http.StatusBadRequest, stale.Code, "the superseded link must stop working")

	fresh := a.postForm("/verify", "token="+tokenFromBody(t, second.Body, "/verify?token="))
	assert.Equal(t, http.StatusOK, fresh.Code, fresh)
}

// The verification token is not a Bearer credential and must not be routed to the Bearer verifier — the
// reason nrp_, nos_, noc_ and nod_ are all absent from LooksLikeOpaqueToken.
func TestAVerificationTokenAuthenticatesNothingElse(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)

	require.Equal(t, http.StatusAccepted, a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "ada", "email": "ada@example.com", "password": testPassword,
	}).Code)
	msg, ok := a.mail.lastOfKind(mail.KindEmailVerification)
	require.True(t, ok)
	token := tokenFromBody(t, msg.Body, "/verify?token=")
	require.True(t, strings.HasPrefix(token, "nev_"))

	resp := a.call(http.MethodGet, "/api/v1/users/@me", nil, withToken(token))
	assert.Equal(t, http.StatusUnauthorized, resp.Code, resp)
}

// postForm submits a urlencoded form, which is how both server-rendered pages are driven.
func (a *api) postForm(path, body string) *response {
	a.t.Helper()
	return a.call(http.MethodPost, path, body,
		withHeader("Content-Type", "application/x-www-form-urlencoded"))
}

// timeRequest measures one registration attempt, and fails if it was not actually accepted.
//
// The status check is what keeps this honest. A request refused early — an invalid username, a rejected
// body — never reaches argon2id, so timing two of those would compare two cheap refusals and pass while
// measuring nothing.
func timeRequest(t *testing.T, a *api, username, email string) time.Duration {
	t.Helper()

	start := time.Now()
	resp := a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": username, "email": email, "password": testPassword,
	})
	elapsed := time.Since(start)

	require.Equal(t, http.StatusAccepted, resp.Code,
		"a timing comparison is meaningless unless both requests were accepted: %s", resp)
	return elapsed
}

// The same probe as TestAnUnverifiedAccountIsRefusedLikeAWrongPassword, on a **gated** instance and with a
// made-up invite code — which is where a review found the oracle still open after the first fix.
//
// The address used to be checked before the invite was redeemed, so a taken address returned the silent
// 202 while a free one fell through to `invite_invalid`. Anybody could test any address on a private
// instance with a code they invented, for free, without even holding an invite. Measured at 202 against
// 403 before the ordering was swapped.
func TestAGatedInstanceDoesNotLeakAddressesToABogusInvite(t *testing.T) {
	a := newAPI(t, auth.RegistrationInvite)

	invite := mintInvite(t, a, map[string]any{})
	require.Equal(t, http.StatusAccepted, a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "ada", "email": "ada@example.com", "password": testPassword,
		"invite_code": invite.Code,
	}).Code)

	// A well-formed code that was never issued.
	const bogus = "BBBBBBBBBBBBBBBB"
	taken := a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "probe1", "email": "ada@example.com", "password": testPassword,
		"invite_code": bogus,
	})
	free := a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "probe2", "email": "nobody@example.com", "password": testPassword,
		"invite_code": bogus,
	})

	assert.Equal(t, http.StatusForbidden, taken.Code, taken)
	assert.Equal(t, taken.Code, free.Code, "a bogus invite must be refused whatever the address")
	assert.Equal(t, taken.errorBody().Code, free.errorBody().Code)
}

// And a real invite is not spent by a registration whose address turns out to be taken. Whoever holds the
// code did nothing wrong, and burning a use for somebody else's typo would cost them the thing they were
// given — so the transaction rolls back rather than returning early.
func TestATakenAddressDoesNotSpendTheInvite(t *testing.T) {
	a := newAPI(t, auth.RegistrationInvite)

	first := mintInvite(t, a, map[string]any{})
	require.Equal(t, http.StatusAccepted, a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "ada", "email": "ada@example.com", "password": testPassword,
		"invite_code": first.Code,
	}).Code)

	invite := mintInvite(t, a, map[string]any{"max_uses": 1})
	require.Equal(t, http.StatusAccepted, a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "probe", "email": "ada@example.com", "password": testPassword,
		"invite_code": invite.Code,
	}).Code, "a taken address is accepted silently, gated or not")

	// Still usable by the person it was meant for.
	assert.Equal(t, http.StatusAccepted, a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "grace", "email": "grace@example.com", "password": testPassword,
		"invite_code": invite.Code,
	}).Code, "the invite must survive a registration that created nothing")
}

// The device verification page authenticates with a password too, and when the unverified gate lived in
// Login it walked straight past it — handing a waiting CLI a full token pair on an account that ordinary
// login refused. That is the address-squatting takeover M10's gate exists to close: register somebody
// else's address, complete a device flow, and hold a session on it.
//
// Found by review. The gate moved into verifyCredentials, which is the one function every password-to-
// session path goes through.
func TestTheDevicePageCannotSignInAnUnverifiedAccount(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)

	require.Equal(t, http.StatusAccepted, a.call(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "squatter", "email": "victim@example.com", "password": testPassword,
	}).Code)

	issued := a.call(http.MethodPost, "/api/v1/auth/device/code", map[string]string{
		"device_id": "dev-1", "device_name": "probe",
	})
	require.Equal(t, http.StatusOK, issued.Code, issued)
	var dc struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
	}
	issued.decode(&dc)

	entered := a.postForm("/device", url.Values{"user_code": {dc.UserCode}}.Encode())
	require.Equal(t, http.StatusOK, entered.Code, entered)

	signin := a.postForm("/device/signin", url.Values{
		"device_token": {hiddenField(t, entered, "device_token")},
		"email":        {"victim@example.com"},
		"password":     {testPassword},
	}.Encode())

	// Refused, and refused the way a wrong password is — the page must not become the oracle either.
	assert.NotContains(t, signin.String(), "Approve",
		"an unverified account must not reach the approval step")

	// And the waiting client gets nothing.
	poll := a.call(http.MethodPost, "/api/v1/auth/device/token", map[string]string{"device_code": dc.DeviceCode})
	assert.NotEqual(t, http.StatusOK, poll.Code, "no token pair may be issued: %s", poll)
}
