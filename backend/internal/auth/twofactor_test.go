package auth

import (
	"net/netip"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// nextStepCode is a code for the step *after* now.
//
// Needed because confirming an enrollment spends its own step (RFC 6238 §5.2), so a test that enrolls and
// then immediately proves the factor is asking to reuse a spent code. The instance's skew accepts one step
// ahead, which is what makes this work without controlling the clock — and it is what a real person does
// too, by waiting for the number to change.
func nextStepCode(t *testing.T, secret string) string {
	t.Helper()
	code, err := totp.GenerateCode(secret, time.Now().Add(totpPeriod*time.Second))
	require.NoError(t, err)
	return code
}

// enableFactor turns the second factor on for an account and returns its secret and recovery codes.
func enableFactor(t *testing.T, svc *Service, userID int64, email string) (string, []string) {
	t.Helper()
	secret, _, err := svc.BeginTOTPEnrollment(t.Context(), userID, email)
	require.NoError(t, err)

	code, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	codes, err := svc.ConfirmTOTPEnrollment(t.Context(), userID, code)
	require.NoError(t, err)
	return secret, codes
}

// A proof obtained for one account must not satisfy a session being minted for another.
//
// The compile-time half of this — that a caller cannot construct a proof at all — is what the type is for
// and cannot be tested from inside the package, since a test here can write the literal. This is the
// runtime half, and it is the confused-deputy case: two account ids travel separately through the sign-in
// paths, because the challenge carries one and the caller supplies the other.
func TestAProofDoesNotAuthorizeAnotherAccount(t *testing.T) {
	proof := factorProof{userID: 42, proved: true}

	assert.True(t, proof.authorizes(42))
	assert.False(t, proof.authorizes(43), "a proof is for one account, not for whoever presents it")
	assert.False(t, factorProof{}.authorizes(42), "the zero value proves nothing")
	assert.False(t, factorProof{userID: 42}.authorizes(42), "and neither does an unproved one")
	assert.False(t, factorProof{proved: true}.authorizes(0),
		"nor one naming no account, which is what a missing subject would produce")
}

// An access token must not be spendable as a two-factor challenge, and a challenge must not be spendable
// as an access token. Same issuer, same key, same subject shape, live expiry — `typ` is the only thing
// between them, which is precisely the confusion ADR 0029 found for operator tokens.
func TestAnAccessTokenIsNotSpendableAsAChallenge(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	user, pair := registerAndLogin(t, svc, "ada@example.com", "laptop")

	_, err := svc.parseTwoFactorChallenge(pair.AccessToken)
	assert.ErrorIs(t, err, ErrTwoFactorChallenge, "an access token is not a challenge")

	challenge, err := svc.issueTwoFactorChallenge(user.ID, LoginInput{DeviceID: "laptop"})
	require.NoError(t, err)

	_, err = svc.AuthenticateAccessToken(t.Context(), challenge)
	assert.ErrorIs(t, err, ErrInvalidToken, "a challenge is not an access token")

	// And it is not a device continuation either, in any of the three shapes that file mints.
	for _, want := range []string{deviceEntryTokenType, deviceFactorTokenType, deviceApprovalTokenType} {
		_, err := svc.parseDeviceToken(challenge, want)
		assert.ErrorIs(t, err, ErrDeviceContinuation, "a challenge must not parse as %s", want)
	}
}

// A challenge names the device the eventual session lands on, and that device comes out of the signature
// rather than from the call that redeems it — a client that could name a different one on the second half
// of a sign-in could move somebody's session onto an identity of its choosing.
func TestTheChallengeCarriesTheDeviceItWasIssuedFor(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")

	raw, err := svc.issueTwoFactorChallenge(user.ID, LoginInput{DeviceID: "phone", DeviceName: "ada's phone"})
	require.NoError(t, err)

	parsed, err := svc.parseTwoFactorChallenge(raw)
	require.NoError(t, err)
	assert.Equal(t, user.ID, parsed.UserID)
	assert.Equal(t, "phone", parsed.Login.DeviceID)
	assert.Equal(t, "ada's phone", parsed.Login.DeviceName)
}

// An expired challenge is refused. Five minutes is long enough to find a phone and short enough that one
// left on a shared machine stops being useful.
func TestAnExpiredChallengeIsRefused(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")

	raw, err := svc.issueTwoFactorChallenge(user.ID, LoginInput{DeviceID: "laptop"})
	require.NoError(t, err)

	now := time.Now().Add(twoFactorChallengeTTL + time.Minute)
	svc.now = func() time.Time { return now }
	svc.issuer.now = func() time.Time { return now }

	_, err = svc.parseTwoFactorChallenge(raw)
	assert.ErrorIs(t, err, ErrTwoFactorChallenge)
}

// Enrolling over a live factor is refused. Replacing one is a change to the account's security state and
// goes through the disable path, which asks for the current factor first — otherwise anyone holding a
// session inside §17.10's window could quietly swap the factor for one of their own.
func TestEnrollingOverALiveFactorIsRefused(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")
	enableFactor(t, svc, user.ID, "ada@example.com")

	_, _, err := svc.BeginTOTPEnrollment(t.Context(), user.ID, "ada@example.com")
	assert.ErrorIs(t, err, ErrTwoFactorAlreadyEnabled)
}

// Regenerating replaces the whole set, so a code from the old one stops working.
func TestRegeneratingInvalidatesTheOldRecoveryCodes(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")
	secret, old := enableFactor(t, svc, user.ID, "ada@example.com")

	fresh, err := svc.RegenerateRecoveryCodes(t.Context(), user.ID, nextStepCode(t, secret))
	require.NoError(t, err)
	require.Len(t, fresh, recoveryCodeCount)
	assert.NotEqual(t, old, fresh)

	_, err = svc.proveFactor(t.Context(), user.ID, old[0])
	assert.ErrorIs(t, err, ErrInvalidFactorCode, "a code from the replaced set must not work")

	_, err = svc.proveFactor(t.Context(), user.ID, fresh[0])
	assert.NoError(t, err, "and one from the new set must")
}

// Disabling revokes the account's other sessions through the M11 primitive rather than through a cleanup
// path of its own (rule 17): the sessions that predate it were established under different rules.
func TestDisablingTheFactorRevokesTheOtherSessions(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	user, keep := registerAndLogin(t, svc, "ada@example.com", "laptop")
	secret, _ := enableFactor(t, svc, user.ID, "ada@example.com")

	other, err := svc.startSession(t.Context(), snowflake.ID(user.ID), "phone", "ada's phone",
		netip.Addr{}, provedFactor(user.ID))
	require.NoError(t, err)

	actor, err := svc.AuthenticateAccessToken(t.Context(), keep.AccessToken)
	require.NoError(t, err)
	result, err := svc.DisableTwoFactor(t.Context(), actor.UserID, actor.SessionID, nextStepCode(t, secret))
	require.NoError(t, err)
	assert.Positive(t, result.Sessions, "the other device must have been signed out")

	_, err = svc.Refresh(t.Context(), other.RefreshToken)
	assert.Error(t, err, "the revoked device must not be able to refresh")

	_, err = svc.Refresh(t.Context(), keep.RefreshToken)
	assert.NoError(t, err, "and this one must still work")
}

// RFC 6238 §5.2 makes single use a MUST: "The verifier MUST NOT accept the second attempt of the OTP after
// the successful validation has been issued for the first OTP."
//
// Without it a code stays good for its whole window — ninety seconds with the skew this instance allows —
// so a phishing page that harvests a password and a code has that long to use both, which is the attack a
// second factor is otherwise good at stopping. Found by audit, after the tests were green.
func TestATOTPCodeCannotBeReplayed(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")
	secret, _ := enableFactor(t, svc, user.ID, "ada@example.com")

	code := nextStepCode(t, secret)

	_, err := svc.proveFactor(t.Context(), user.ID, code)
	require.NoError(t, err, "the first use of a code must work")

	_, err = svc.proveFactor(t.Context(), user.ID, code)
	assert.ErrorIs(t, err, ErrInvalidFactorCode, "the same code must not work twice")
}

// And the code that turned the factor on is spent too — that is the one most likely to have been seen over
// a shoulder, and leaving it live would mean the act of enabling the factor handed somebody a way past it.
func TestTheConfirmingCodeIsAlsoSpent(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")

	secret, _, err := svc.BeginTOTPEnrollment(t.Context(), user.ID, "ada@example.com")
	require.NoError(t, err)
	code, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	_, err = svc.ConfirmTOTPEnrollment(t.Context(), user.ID, code)
	require.NoError(t, err)

	_, err = svc.proveFactor(t.Context(), user.ID, code)
	assert.ErrorIs(t, err, ErrInvalidFactorCode,
		"the code that enabled the factor must not also get past it")
}

// A code from an earlier step inside the skew window must not work after a later one has been accepted,
// which is the case a naive "record the newest" would miss.
func TestAnEarlierStepIsRefusedAfterALaterOne(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")
	secret, _ := enableFactor(t, svc, user.ID, "ada@example.com")

	now := time.Now()
	earlier, err := totp.GenerateCode(secret, now)
	require.NoError(t, err)
	later, err := totp.GenerateCode(secret, now.Add(totpPeriod*time.Second))
	require.NoError(t, err)
	if earlier == later {
		t.Skip("the two steps produced the same code, which happens once in a million")
	}

	_, err = svc.proveFactor(t.Context(), user.ID, later)
	require.NoError(t, err, "the later step is accepted first")

	_, err = svc.proveFactor(t.Context(), user.ID, earlier)
	assert.ErrorIs(t, err, ErrInvalidFactorCode, "a step behind the last accepted one is spent")
}

// A code with whitespace around it is the code, not a wrong one.
//
// This regressed when matchTOTPStep was hand-rolled: comparing GenerateCodeCustom's output directly loses
// the TrimSpace the library does first, so a code pasted with a trailing space failed against every step
// and reported as simply wrong — indistinguishable, to the person typing it, from getting it wrong.
func TestACodeWithWhitespaceIsStillTheCode(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")
	secret, _ := enableFactor(t, svc, user.ID, "ada@example.com")

	code := nextStepCode(t, secret)
	_, err := svc.proveFactor(t.Context(), user.ID, " "+code+"\n")
	assert.NoError(t, err, "surrounding whitespace must not make a good code look wrong")
}

// A secret that decrypts but is not valid base32 is an error about the account, not about the code.
//
// The hand-rolled matcher swallowed it with a `continue` and retried the same failure once per step, so
// every code the account typed answered "that code is not valid", permanently, with nothing logged — and
// an operator had no way to tell it from a user with a wrong clock.
func TestACorruptSecretIsReportedRatherThanReadAsAWrongCode(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")
	enableFactor(t, svc, user.ID, "ada@example.com")

	// Sealed correctly, so it opens — and is nonsense inside, which is the case ErrSealedSecretInvalid
	// cannot catch.
	sealed, err := svc.issuer.sealTOTPSecret("not-valid-base32-!!!")
	require.NoError(t, err)
	_, err = svc.pool.Exec(t.Context(),
		"UPDATE user_totp SET secret_encrypted = $1 WHERE user_id = $2", sealed, user.ID)
	require.NoError(t, err)

	_, err = svc.proveFactor(t.Context(), user.ID, "123456")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrInvalidFactorCode,
		"a broken secret is the instance's problem to see, not the user's to retype")
}
