package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/operatortoken"
)

// The operator token is signed with the same key as every access token on the instance, so nothing about
// the signature distinguishes them — what does is entirely what is checked on the way back in. These
// isolate that, which the HTTP tests cannot: from outside, the router and the `typ` check refuse the same
// request, and either one passing alone would make the other look load-bearing when it is not.

// operatorIssuer builds an issuer over a throwaway key, with no database behind it.
//
// Deliberately not newService: none of the properties here involve a row, and a token test that needs a
// container is one that stops being run in the inner loop.
func operatorIssuer(t *testing.T) *TokenIssuer {
	t.Helper()
	issuer, err := NewTokenIssuer([]byte("an-operator-test-signing-key-of-sufficient-length"))
	require.NoError(t, err)
	return issuer
}

// The one constant now defined in two modules' worth of code, and the one that cannot be allowed to
// drift: operatortoken.Verify pins `iss`, so an operator token minted by the CLI is refused outright if
// these ever disagree — with the undifferentiated error every other failure produces, which would make it
// look like a key problem.
func TestTheIssuerClaimIsTheSameOnBothSides(t *testing.T) {
	assert.Equal(t, tokenIssuer, operatortoken.Issuer)
}

// Every signed-in user holds an access token signed with this same key; if that satisfied the operator
// check, every account on the instance could administer it.
//
// Note what actually refuses it, because it is not the `typ` check: an access token names an account, so
// the empty-subject check below catches it first. Confirmed by disabling `typ`, which leaves this test
// passing. The token `typ` is load-bearing against is the device entry token — see the test after next.
func TestAnAccessTokenIsNotAnOperatorToken(t *testing.T) {
	issuer := operatorIssuer(t)

	access, _, err := issuer.Issue(1, 2)
	require.NoError(t, err)

	assert.ErrorIs(t, issuer.VerifyOperatorToken(access), ErrNotOperator)
}

// The other direction. An operator token names no account, so accepting one as an access token would mean
// authenticating a request as user 0 — the shape Actor.IsZero reads as "nobody" and several handlers would
// read as "the actor is present, its ID is just zero".
func TestAnOperatorTokenIsNotAnAccessToken(t *testing.T) {
	issuer := operatorIssuer(t)

	operator, err := issuer.IssueOperatorToken()
	require.NoError(t, err)

	_, err = issuer.Verify(operator)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

// The confusion `typ` exists for, and the one the shape checks do not catch.
//
// A device entry token is signed with this same key, carries the same issuer, has a live expiry, and names
// no account — because at that point in the flow nobody has authenticated yet. Every structural property
// an operator token has, it has. Without the `typ` check, entering a valid device code on the verification
// page would hand the browser instance-operator authority, which is several tiers above what completing
// that whole flow is supposed to grant.
//
// Built through the real Service rather than by hand, so it keeps reflecting what a device token actually
// looks like rather than what this test once believed.
func TestADeviceEntryTokenIsNotAnOperatorToken(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)

	entry, err := svc.issueDeviceEntryToken(42, "BCDFGHJK")
	require.NoError(t, err)
	// Valid as what it is, so the refusal below is about the type rather than about a broken token.
	_, err = svc.parseDeviceToken(entry, deviceEntryTokenType)
	require.NoError(t, err)

	assert.ErrorIs(t, svc.issuer.VerifyOperatorToken(entry), ErrNotOperator)
}

// What the signature is actually asserting: that whoever produced this could read *this instance's*
// signing key. A token minted against another instance's config file must buy nothing here.
func TestAnOperatorTokenFromAnotherInstanceIsRefused(t *testing.T) {
	mine := operatorIssuer(t)
	theirs, err := NewTokenIssuer([]byte("a-different-instances-signing-key-entirely!!"))
	require.NoError(t, err)

	token, err := theirs.IssueOperatorToken()
	require.NoError(t, err)

	assert.ErrorIs(t, mine.VerifyOperatorToken(token), ErrNotOperator)
}

// The only thing that ends an operator token is the clock — there is no row to delete, so it is the one
// credential here that cannot be revoked. That makes the expiry check the whole of its containment.
func TestAnExpiredOperatorTokenIsRefused(t *testing.T) {
	issuer := operatorIssuer(t)

	minted := time.Now()
	issuer.now = func() time.Time { return minted }
	token, err := issuer.IssueOperatorToken()
	require.NoError(t, err)

	// Still good a moment before it lapses, so the refusal below is about the deadline rather than about
	// the token never having been valid.
	issuer.now = func() time.Time { return minted.Add(OperatorTokenTTL - time.Second) }
	require.NoError(t, issuer.VerifyOperatorToken(token))

	issuer.now = func() time.Time { return minted.Add(OperatorTokenTTL + time.Second) }
	assert.ErrorIs(t, issuer.VerifyOperatorToken(token), ErrNotOperator)
}

// An operator is not an account, and a token claiming to be both is one this package did not mint. Refused
// rather than read past: honoring the parts of an unfamiliar token that happen to parse is how a confused
// deputy starts.
func TestAnOperatorTokenNamingAnAccountIsRefused(t *testing.T) {
	issuer := operatorIssuer(t)

	now := time.Now()
	forged, err := issuer.sign(operatortoken.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    operatortoken.Issuer,
			Subject:   "12345",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
		},
		TokenType: operatortoken.TokenType,
	})
	require.NoError(t, err)

	assert.ErrorIs(t, issuer.VerifyOperatorToken(forged), ErrNotOperator)
}

// The algorithm pin, on this token as on every other. A token whose header says `"alg":"none"` carries no
// signature at all, so accepting one would mean anybody could mint operator authority by typing it.
func TestAnUnsignedOperatorTokenIsRefused(t *testing.T) {
	issuer := operatorIssuer(t)

	now := time.Now()
	unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, operatortoken.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    operatortoken.Issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
		},
		TokenType: operatortoken.TokenType,
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)

	assert.ErrorIs(t, issuer.VerifyOperatorToken(unsigned), ErrNotOperator)
}

// A token with no expiry never stops working, which for an unrevocable credential means forever. The
// parser is told to require one rather than defaulting it, so a hand-built token cannot omit it.
func TestAnOperatorTokenWithoutAnExpiryIsRefused(t *testing.T) {
	issuer := operatorIssuer(t)

	eternal, err := issuer.sign(operatortoken.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:   tokenIssuer,
			IssuedAt: jwt.NewNumericDate(time.Now()),
		},
		TokenType: operatortoken.TokenType,
	})
	require.NoError(t, err)

	assert.ErrorIs(t, issuer.VerifyOperatorToken(eternal), ErrNotOperator)
}
