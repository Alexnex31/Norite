package auth

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// The primitive's own tests.
//
// The password-reset path is covered where it lives — TestResettingRevokesSessionsAndAPITokens,
// TestAPasswordResetRevokesOutstandingExchangeCodes, TestRevokingSessionsAlsoRevokesAnApprovedDeviceCode —
// and those three passing unchanged is what says the M11 extraction moved code rather than behavior.
//
// These test the primitive directly, which those three cannot: they exercise it through one caller, so a
// step added to that caller and not to the primitive would still pass all three. The list of claims lives
// in one function now, and this is where the list itself is pinned.

// All four kinds of outstanding claim, revoked in one call.
//
// One test rather than four, because the property is the *list*: a claim that stops being revoked is only
// visible against the others. Assembling all four needs the OAuth stub and the device-code helpers as well
// as an ordinary login, which is the reason no existing test does it.
func TestTheRevocationPrimitiveRevokesEveryClaim(t *testing.T) {
	svc, stub := oauthService(t, RegistrationOpen)
	pool := svc.pool
	ctx := t.Context()

	user, pair := registerAndLogin(t, svc, "ada@example.com", "laptop")

	minted, err := svc.MintAPIToken(ctx, snowflake.ID(user.ID), MintAPITokenInput{
		Name: "bot", Scopes: []Scope{ScopeIdentify},
	})
	require.NoError(t, err)

	stub.asGoogle("google-1", "ada@example.com", true)
	outcome, verifier, err := signInBound(t, svc, stub, "google")
	require.NoError(t, err)
	require.NotEmpty(t, outcome.ExchangeCode)

	deviceAuth := startDeviceAuth(t, svc, pool)
	approve(t, svc, deviceAuth, user.ID)

	// Every one of them works beforehand, so a green test cannot be one that revoked nothing.
	_, err = svc.AuthenticateAPIToken(ctx, minted.Raw)
	require.NoError(t, err)

	result, err := revokeEverything(ctx, svc.queries, user.ID, RevocationScope{})
	require.NoError(t, err)

	assert.EqualValues(t, 1, result.Sessions, "the one live session")
	assert.EqualValues(t, 1, result.APITokens, "the one API token")
	assert.EqualValues(t, 1, result.ExchangeCodes, "the one outstanding exchange code")
	assert.EqualValues(t, 1, result.DeviceCodes, "the one approved device authorization")
	assert.EqualValues(t, 4, result.Total(), "counts are what a caller reports; they must add up")

	_, err = svc.Refresh(ctx, pair.RefreshToken)
	assert.ErrorIs(t, err, ErrInvalidRefreshToken)

	_, err = svc.AuthenticateAPIToken(ctx, minted.Raw)
	assert.ErrorIs(t, err, ErrInvalidToken)

	// Redeemed with the correct verifier, so what refuses it is the revocation and nothing else.
	_, err = svc.ExchangeOAuthCode(ctx, outcome.ExchangeCode, verifier, LoginInput{DeviceID: "laptop"})
	assert.ErrorIs(t, err, ErrOAuthExchangeCode)

	letThePollIntervalPass(t, pool)
	_, err = svc.RedeemDeviceCode(ctx, deviceAuth.DeviceCode, netip.Addr{})
	assert.ErrorIs(t, err, ErrDeviceCodeExpired)
}

// Sparing a device spares its whole family, and nothing else.
func TestSparingADeviceSparesItAndNoOther(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	ctx := t.Context()

	user, laptop := registerAndLogin(t, svc, "ada@example.com", "laptop")
	phone, err := svc.Login(ctx, LoginInput{
		Email: "ada@example.com", Password: testPassword, DeviceID: "phone",
	})
	require.NoError(t, err)
	tablet, err := svc.Login(ctx, LoginInput{
		Email: "ada@example.com", Password: testPassword, DeviceID: "tablet",
	})
	require.NoError(t, err)

	result, err := revokeEverything(ctx, svc.queries, user.ID, RevocationScope{KeepDeviceID: "laptop"})
	require.NoError(t, err)
	assert.EqualValues(t, 2, result.Sessions, "the phone and the tablet, not the laptop")

	_, err = svc.Refresh(ctx, laptop.RefreshToken)
	assert.NoError(t, err, "the spared device must still be able to refresh")

	for name, tok := range map[string]string{"phone": phone.RefreshToken, "tablet": tablet.RefreshToken} {
		_, err = svc.Refresh(ctx, tok)
		assert.ErrorIs(t, err, ErrInvalidRefreshToken, "%s should have been signed out", name)
	}
}

// Sparing a device spares the family, not one row of it.
//
// The distinction is invisible until the spared device refreshes. A revocation that spared the *session*
// the caller was using would leave every older row in that family revoked and the newest one live — which
// looks identical until the next rotation walks off the end of the chain. Rotating twice here is what makes
// the difference observable.
func TestASparedDeviceSurvivesItsNextRotations(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	ctx := t.Context()

	user, laptop := registerAndLogin(t, svc, "ada@example.com", "laptop")

	// Rotate first, so the family already has a revoked predecessor when the revocation runs.
	rotated, err := svc.Refresh(ctx, laptop.RefreshToken)
	require.NoError(t, err)

	_, err = revokeEverything(ctx, svc.queries, user.ID, RevocationScope{KeepDeviceID: "laptop"})
	require.NoError(t, err)

	again, err := svc.Refresh(ctx, rotated.RefreshToken)
	require.NoError(t, err, "the spared family must keep rotating after the revocation")

	_, err = svc.Refresh(ctx, again.RefreshToken)
	assert.NoError(t, err, "and keep rotating after that")
}

// An account with nothing outstanding is not an error, and reports nothing.
//
// Worth pinning because a caller renders these counts: "signed out 0 other devices" has to be reachable
// rather than becoming a failure somebody has to handle.
func TestRevokingAnAccountWithNothingOutstandingIsQuiet(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	ctx := t.Context()

	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")

	_, err := revokeEverything(ctx, svc.queries, user.ID, RevocationScope{})
	require.NoError(t, err)

	result, err := revokeEverything(ctx, svc.queries, user.ID, RevocationScope{})
	require.NoError(t, err)
	assert.Zero(t, result.Total(), "a second pass has nothing left to revoke")
}
