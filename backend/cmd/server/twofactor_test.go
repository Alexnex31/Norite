package main

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/auth"
)

// The second factor, end to end over the real router.
//
// The property most of these exist for is not "the factor works" — it is that turning one on changes
// nothing an unauthenticated caller can see. Every anti-enumeration guarantee M4 through M10 built lives
// on the paths a factor threads through, and a prompt in the wrong place undoes one of them silently.

// enableTwoFactor enrolls an account and returns its secret and recovery codes.
func enableTwoFactor(t *testing.T, a *api, accessToken string) (secret string, codes []string) {
	t.Helper()

	begun := a.call(http.MethodPost, "/api/v1/auth/2fa/totp", nil, withToken(accessToken))
	require.Equal(t, http.StatusOK, begun.Code, "begin enrollment: %s", begun)
	var enrollment struct {
		Secret string `json:"secret"`
		URI    string `json:"uri"`
	}
	begun.decode(&enrollment)
	require.NotEmpty(t, enrollment.Secret)
	require.Contains(t, enrollment.URI, "otpauth://totp/", "an app has to be able to scan this")

	confirmed := a.call(http.MethodPost, "/api/v1/auth/2fa/totp/confirm",
		map[string]string{"code": confirmingCode(t, enrollment.Secret)}, withToken(accessToken))
	require.Equal(t, http.StatusOK, confirmed.Code, "confirm: %s", confirmed)
	var result struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	confirmed.decode(&result)
	require.Len(t, result.RecoveryCodes, 10)

	return enrollment.Secret, result.RecoveryCodes
}

// totpCode is a code for the step *after* now.
//
// The next step rather than the current one, because confirming an enrollment spends its own step
// (RFC 6238 §5.2) and every test here enrolls first — asking for the current code would be asking to reuse
// a spent one. The instance accepts one step of skew either way, which is what makes this work without
// controlling the server's clock, and it is what a real person does by waiting for the number to change.
func totpCode(t *testing.T, secret string) string {
	t.Helper()
	code, err := totp.GenerateCode(secret, time.Now().Add(30*time.Second))
	require.NoError(t, err)
	return code
}

// confirmingCode is the current step, used only where an enrollment is being confirmed.
func confirmingCode(t *testing.T, secret string) string {
	t.Helper()
	code, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	return code
}

// twoFactorChallenge is the 202 body.
type twoFactorChallenge struct {
	TwoFactorRequired bool      `json:"two_factor_required"`
	Challenge         string    `json:"challenge"`
	ExpiresAt         time.Time `json:"expires_at"`
}

// A password login against an account with a factor answers 202 and a challenge, never a pair.
func TestALoginOwesTheSecondFactor(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	ada := a.newAccount("ada", "ada@example.com", "device-1")
	secret, _ := enableTwoFactor(t, a, ada.Tokens.AccessToken)

	resp := a.call(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "ada@example.com", "password": testPassword, "device_id": "device-2",
	})
	require.Equal(t, http.StatusAccepted, resp.Code, "a factor is owed, so no session yet: %s", resp)

	var challenge twoFactorChallenge
	resp.decode(&challenge)
	assert.True(t, challenge.TwoFactorRequired)
	assert.NotEmpty(t, challenge.Challenge)
	assert.NotContains(t, resp.String(), "access_token", "a 202 must not carry a session")

	// And the challenge plus a code is the session.
	done := a.call(http.MethodPost, "/api/v1/auth/2fa/verify", map[string]string{
		"challenge": challenge.Challenge, "code": totpCode(t, secret),
	})
	require.Equal(t, http.StatusOK, done.Code, "verify: %s", done)
	var pair tokenPair
	done.decode(&pair)
	assert.NotEmpty(t, pair.AccessToken)
	assert.NotEmpty(t, pair.RefreshToken)
}

// The challenge is not a credential. Presenting one without a code, or with a wrong one, is refused exactly
// as a wrong password is.
func TestAChallengeAloneIsNotASession(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	ada := a.newAccount("ada", "ada@example.com", "device-1")
	enableTwoFactor(t, a, ada.Tokens.AccessToken)

	resp := a.call(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "ada@example.com", "password": testPassword, "device_id": "device-2",
	})
	require.Equal(t, http.StatusAccepted, resp.Code)
	var challenge twoFactorChallenge
	resp.decode(&challenge)

	wrong := a.call(http.MethodPost, "/api/v1/auth/2fa/verify", map[string]string{
		"challenge": challenge.Challenge, "code": "000000",
	})
	assert.Equal(t, http.StatusUnauthorized, wrong.Code,
		"a challenge without a valid code authorizes nothing: %s", wrong)
}

// The property this milestone must not break.
//
// Every way a login can fail must answer identically whether or not the account has a second factor. The
// factor is asked about strictly after verifyCredentials succeeds, so this holds by construction — and a
// test says so, because "by construction" is a claim that stops being true when somebody moves a check.
func TestAFailedLoginIsUnchangedByTheSecondFactor(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	a.newAccount("grace", "grace@example.com", "device-1")
	guarded := a.newAccount("ada", "ada@example.com", "device-1")
	enableTwoFactor(t, a, guarded.Tokens.AccessToken)

	for name, email := range map[string]string{
		"an account with a factor":   "ada@example.com",
		"an account without one":     "grace@example.com",
		"an address with no account": "nobody@example.com",
	} {
		t.Run(name, func(t *testing.T) {
			resp := a.call(http.MethodPost, "/api/v1/auth/login", map[string]string{
				"email": email, "password": "not-the-right-password-at-all", "device_id": "device-9",
			})
			require.Equal(t, http.StatusUnauthorized, resp.Code, "%s: %s", name, resp)
			assert.Equal(t, "unauthorized", resp.errorBody().Code)
			assert.Equal(t, "invalid email or password", resp.errorBody().Message,
				"one message for every failure, whatever the account has")
		})
	}
}

// A recovery code signs in, and does so exactly once.
func TestARecoveryCodeWorksExactlyOnce(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	ada := a.newAccount("ada", "ada@example.com", "device-1")
	_, codes := enableTwoFactor(t, a, ada.Tokens.AccessToken)

	signIn := func(code string) *response {
		resp := a.call(http.MethodPost, "/api/v1/auth/login", map[string]string{
			"email": "ada@example.com", "password": testPassword, "device_id": "device-2",
		})
		require.Equal(t, http.StatusAccepted, resp.Code)
		var challenge twoFactorChallenge
		resp.decode(&challenge)
		return a.call(http.MethodPost, "/api/v1/auth/2fa/verify", map[string]string{
			"challenge": challenge.Challenge, "code": code,
		})
	}

	first := signIn(codes[0])
	require.Equal(t, http.StatusOK, first.Code, "a recovery code is a way in: %s", first)

	second := signIn(codes[0])
	assert.Equal(t, http.StatusUnauthorized, second.Code,
		"the same recovery code must not work twice: %s", second)

	// A different one still does, so spending one does not spend the set.
	third := signIn(codes[1])
	assert.Equal(t, http.StatusOK, third.Code, "%s", third)
}

// Turning the factor off requires holding it — a session alone is not enough.
//
// This is the case §17.10's residual window makes real: an access token outlives its session by up to
// fifteen minutes, so without the step-up a stolen session could remove the control standing between an
// intruder and the account.
func TestDisablingTheFactorNeedsTheFactor(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	ada := a.newAccount("ada", "ada@example.com", "device-1")
	secret, _ := enableTwoFactor(t, a, ada.Tokens.AccessToken)

	refused := a.call(http.MethodDelete, "/api/v1/auth/2fa/totp",
		map[string]string{"code": "000000"}, withToken(ada.Tokens.AccessToken))
	assert.Equal(t, http.StatusUnauthorized, refused.Code,
		"a live session is not enough to remove the factor: %s", refused)

	// Still on.
	me := a.call(http.MethodGet, "/api/v1/users/@me", nil, withToken(ada.Tokens.AccessToken))
	require.Equal(t, http.StatusOK, me.Code)
	assert.Contains(t, me.String(), `"two_factor_enabled":true`)

	done := a.call(http.MethodDelete, "/api/v1/auth/2fa/totp",
		map[string]string{"code": totpCode(t, secret)}, withToken(ada.Tokens.AccessToken))
	require.Equal(t, http.StatusOK, done.Code, "%s", done)

	// And now a password alone signs in again.
	back := a.call(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "ada@example.com", "password": testPassword, "device_id": "device-3",
	})
	assert.Equal(t, http.StatusOK, back.Code, "the factor is gone: %s", back)
}

// The classic bypass: reset the password, sign in, skip the factor. Closed by shape — ConfirmPasswordReset
// starts no session — but asserted, because "closed by construction" stops being true quietly.
func TestAPasswordResetDoesNotBypassTheSecondFactor(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	ada := a.newAccount("ada", "ada@example.com", "device-1")
	enableTwoFactor(t, a, ada.Tokens.AccessToken)

	token := a.issueResetToken(t, "ada@example.com")
	const newPassword = "an-entirely-different-password"
	reset := a.call(http.MethodPost, "/api/v1/auth/password/reset", map[string]string{
		"token": token, "new_password": newPassword,
	})
	require.Equal(t, http.StatusNoContent, reset.Code, "%s", reset)

	resp := a.call(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "ada@example.com", "password": newPassword, "device_id": "device-2",
	})
	assert.Equal(t, http.StatusAccepted, resp.Code,
		"a reset changes the password; it does not remove the factor: %s", resp)
}

// An enrollment somebody started and abandoned is not a factor, and must not lock them out.
func TestAnAbandonedEnrollmentDoesNotLockTheAccount(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	ada := a.newAccount("ada", "ada@example.com", "device-1")

	begun := a.call(http.MethodPost, "/api/v1/auth/2fa/totp", nil, withToken(ada.Tokens.AccessToken))
	require.Equal(t, http.StatusOK, begun.Code, "%s", begun)

	resp := a.call(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "ada@example.com", "password": testPassword, "device_id": "device-2",
	})
	assert.Equal(t, http.StatusOK, resp.Code,
		"an unconfirmed enrollment is not a factor: %s", resp)

	me := a.call(http.MethodGet, "/api/v1/users/@me", nil, withToken(ada.Tokens.AccessToken))
	assert.Contains(t, me.String(), `"two_factor_enabled":false`,
		"and it must not read as protected either")
}

// A refresh is not asked for the factor: the session it rotates proved one when it was established.
func TestARefreshIsNotAskedForTheSecondFactor(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	ada := a.newAccount("ada", "ada@example.com", "device-1")
	enableTwoFactor(t, a, ada.Tokens.AccessToken)

	resp := a.call(http.MethodPost, "/api/v1/auth/refresh", map[string]string{
		"refresh_token": ada.Tokens.RefreshToken,
	})
	assert.Equal(t, http.StatusOK, resp.Code,
		"rotating a session that already proved the factor must not ask again: %s", resp)
}

// An API token may not touch the factor, for the reason it may not mint tokens or list sessions: a
// delegated credential that can remove its owner's second factor can lock its owner out of the protection
// they turned on.
func TestAnAPITokenCannotTouchTheSecondFactor(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	ada := a.newAccount("ada", "ada@example.com", "device-1")

	minted := a.call(http.MethodPost, "/api/v1/auth/tokens", map[string]any{
		"name": "bot", "scopes": []string{"identify"},
	}, withToken(ada.Tokens.AccessToken))
	require.Equal(t, http.StatusCreated, minted.Code, "%s", minted)
	var token struct {
		Value string `json:"value"`
	}
	minted.decode(&token)
	require.NotEmpty(t, token.Value)

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/auth/2fa/totp"},
		{http.MethodPost, "/api/v1/auth/2fa/totp/confirm"},
		{http.MethodDelete, "/api/v1/auth/2fa/totp"},
		{http.MethodPost, "/api/v1/auth/2fa/recovery-codes"},
	} {
		resp := a.call(tc.method, tc.path, map[string]string{"code": "000000"}, withToken(token.Value))
		assert.Equal(t, http.StatusForbidden, resp.Code, "%s %s: %s", tc.method, tc.path, resp)
	}
}

// The device flow is the path where the factor can only be asked for in the browser: the waiting client
// redeems its code without proving anything, because there is nobody at that terminal. So this is the one
// place the gate could have been left out and nothing else would have noticed.
func TestTheDevicePageAsksForTheCodeBeforeApproving(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	ada := a.newAccount("ada", "ada@example.com", "device-1")
	secret, _ := enableTwoFactor(t, a, ada.Tokens.AccessToken)

	issued := a.issueDeviceCode(t)
	entry := a.enterUserCode(t, issued.UserCode)

	// The password alone gets the factor step, not the approval step.
	signedIn := a.call(http.MethodPost, "/device/signin", formBody(url.Values{
		"device_token": {entry}, "email": {"ada@example.com"}, "password": {testPassword},
	}), asForm)
	require.Equal(t, http.StatusOK, signedIn.Code, "%s", signedIn)
	require.Contains(t, signedIn.String(), "/device/2fa",
		"a password on a 2FA account must reach the code form, not the approval form")
	require.NotContains(t, signedIn.String(), "/device/approve",
		"and must not be offered approval yet")

	pending := deviceTokenFrom(t, signedIn.String())

	// A wrong code does not get past it.
	refused := a.call(http.MethodPost, "/device/2fa", formBody(url.Values{
		"device_token": {pending}, "code": {"000000"},
	}), asForm)
	assert.Equal(t, http.StatusUnauthorized, refused.Code, "%s", refused)

	// The right one does, and only then is approval on offer.
	passed := a.call(http.MethodPost, "/device/2fa", formBody(url.Values{
		"device_token": {pending}, "code": {totpCode(t, secret)},
	}), asForm)
	require.Equal(t, http.StatusOK, passed.Code, "%s", passed)
	require.Contains(t, passed.String(), "/device/approve")

	a.decideDevice(t, deviceTokenFrom(t, passed.String()), "approve")
	pair := a.pollUntilSignedIn(t, issued.DeviceCode)
	assert.NotEmpty(t, pair.AccessToken)
}

// The factor continuation is its own type, and the three device tokens are not interchangeable.
//
// ADR 0028 argued for two rather than one because a single token with an optional user field authorizes
// before authentication has happened. A browser that has typed a correct password on a 2FA account is at
// neither existing point, so it gets a third — and presenting it where an approval token belongs must not
// skip the step it exists to impose.
func TestADeviceFactorTokenIsNotAnApprovalToken(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	ada := a.newAccount("ada", "ada@example.com", "device-1")
	enableTwoFactor(t, a, ada.Tokens.AccessToken)

	issued := a.issueDeviceCode(t)
	entry := a.enterUserCode(t, issued.UserCode)
	signedIn := a.call(http.MethodPost, "/device/signin", formBody(url.Values{
		"device_token": {entry}, "email": {"ada@example.com"}, "password": {testPassword},
	}), asForm)
	require.Equal(t, http.StatusOK, signedIn.Code)
	pending := deviceTokenFrom(t, signedIn.String())

	// The factor token, presented at approval, must not authorize.
	resp := a.call(http.MethodPost, "/device/approve", formBody(url.Values{
		"device_token": {pending}, "decision": {"approve"},
	}), asForm)
	assert.Equal(t, http.StatusBadRequest, resp.Code,
		"a browser that has not passed the factor must not be able to approve: %s", resp)

	// And the code was never authorized, so the waiting client still cannot redeem.
	poll := a.call(http.MethodPost, "/api/v1/auth/device/token",
		map[string]string{"device_code": issued.DeviceCode})
	assert.NotEqual(t, http.StatusOK, poll.Code, "no session may exist: %s", poll)
}
