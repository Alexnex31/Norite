package auth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The two continuations are signed with the same key as every other token this package issues, so what
// keeps them apart is entirely what is checked on the way back in. These isolate that, which the HTTP
// tests cannot: from outside, several guards refuse the same request and any one of them passing the test
// makes the others look load-bearing when they may not be.

// The confusion that matters. An entry token says a browser has entered a live code; an approval token
// says it has also proved whose account this is. Accepting the first where the second belongs would make
// the sign-in step decorative.
func TestAnEntryTokenIsNotAnApprovalToken(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)

	entry, err := svc.issueDeviceEntryToken(42, "BCDFGHJK")
	require.NoError(t, err)

	_, err = svc.parseDeviceToken(entry, deviceApprovalTokenType)
	assert.ErrorIs(t, err, ErrDeviceContinuation)

	// And it is accepted as what it is, so the refusal above is about the type rather than about the
	// token being broken.
	got, err := svc.parseDeviceToken(entry, deviceEntryTokenType)
	require.NoError(t, err)
	assert.Equal(t, int64(42), got.DeviceCodeID)
	assert.Equal(t, "BCDFGHJK", got.UserCode)
	assert.Zero(t, got.UserID, "an entry token names no account")
}

// The other direction is not a security problem — it would let somebody redo a step they have already
// passed — but it is the same check, and a `typ` that holds in only one direction is one somebody will
// later assume holds in neither.
func TestAnApprovalTokenIsNotAnEntryToken(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)

	approval, err := svc.issueDeviceApprovalToken(42, "BCDFGHJK", 99, provedFactor(99))
	require.NoError(t, err)

	_, err = svc.parseDeviceToken(approval, deviceEntryTokenType)
	assert.ErrorIs(t, err, ErrDeviceContinuation)

	got, err := svc.parseDeviceToken(approval, deviceApprovalTokenType)
	require.NoError(t, err)
	assert.Equal(t, int64(99), got.UserID)
}

// Every other token this package signs, presented at both steps. An access token is the one to think
// about: any signed-in user holds one, it names a real account in `sub`, and it is signed with this same
// key.
//
// `typ` is the declared guard and is checked first, but it is not the only thing refusing these — the
// claim shapes do it independently, since nothing else this package signs carries a `dvc`. Confirmed by
// disabling the `typ` check, which leaves this test passing and TestAnApprovalTokenIsNotAnEntryToken
// failing. Both are kept: the shape check is a coincidence of what these tokens happen to contain today,
// and `typ` is the statement of intent.
func TestNoOtherTokenIsADeviceContinuation(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	user, pair := registerAndLogin(t, svc, "ada@example.com", "laptop")

	signup, err := svc.issueOAuthSignupToken(OAuthIdentity{
		Provider:      ProviderGoogle,
		UserID:        "google-1",
		Email:         "ada@example.com",
		EmailVerified: true,
	}, oauthDestination{Challenge: TokenHash(make([]byte, 32))})
	require.NoError(t, err)

	for _, tc := range []struct{ why, token string }{
		{"an access token, which every signed-in user holds", pair.AccessToken},
		{"a refresh token", pair.RefreshToken},
		{"an OAuth signup continuation", signup},
		{"a value that is not a token at all", "nod_not-a-jwt"},
		{"nothing", ""},
	} {
		for _, want := range []string{deviceEntryTokenType, deviceApprovalTokenType} {
			_, err := svc.parseDeviceToken(tc.token, want)
			assert.ErrorIs(t, err, ErrDeviceContinuation, "%s, at the %s step", tc.why, want)
		}
	}
	assert.NotZero(t, user.ID)
}

// A continuation outlives neither the person's attention span nor the authorization it belongs to.
func TestADeviceContinuationExpires(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)

	svc.now = func() time.Time { return time.Now().Add(-2 * deviceTokenTTL) }
	stale, err := svc.issueDeviceApprovalToken(42, "BCDFGHJK", 99, provedFactor(99))
	require.NoError(t, err)
	svc.now = time.Now

	_, err = svc.parseDeviceToken(stale, deviceApprovalTokenType)
	assert.ErrorIs(t, err, ErrDeviceContinuation)
}

// The user code inside a continuation is displayed on the approval page, so it is re-validated on the way
// out rather than trusted because this service wrote it — the same backstop the signup token's redirect
// and challenge get, and for the same reason.
func TestAContinuationCarryingAnImpossibleUserCodeIsRefused(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)

	bad, err := svc.signDeviceToken(deviceApprovalTokenType, 42, "<script>", 99)
	require.NoError(t, err)

	_, err = svc.parseDeviceToken(bad, deviceApprovalTokenType)
	assert.ErrorIs(t, err, ErrDeviceContinuation)
}
