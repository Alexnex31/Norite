package auth

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/golang-jwt/jwt/v5"
)

// The continuation token that carries a verified provider identity across the username step.
//
// # Why nothing is written to `users` until a username arrives
//
// The obvious implementation of "choose a username before the account works" is a half-created account row
// with a pending flag. This does not do that. A pending row is a state every later milestone has to know
// about — every query that counts accounts, lists members, resolves a mention, or enforces a unique
// username has to remember that some rows are not real yet — and the first one that forgets produces a
// ghost account nobody can sign in to.
//
// Holding the identity in a signed token instead means the account, its oauth_identities row, and its first
// session are all created in one transaction, from nothing, at the moment the username is known. There is
// no intermediate state to respect because there is no intermediate row.
//
// # Why a signed token rather than another table
//
// Every other short-lived value in this package is an opaque token backed by a row, and this one is
// deliberately not. Those exist because something has to be *revoked* or *spent exactly once* —
// server-side state is the only way to enforce that. This token needs neither: replaying it cannot create
// a second account, because oauth_identities' unique constraint on (provider, provider_user_id) refuses
// the second attempt. Single-use falls out of the schema, so a table would buy nothing but rows to clean
// up.
//
// The `typ` claim is what keeps it from being anything else. It is the same mechanism that stops an access
// token being replayed at a different purpose (see Claims.TokenType), and it means a signup token
// presented as a Bearer credential is rejected before its signature is even considered relevant.

// oauthSignupTokenType is the `typ` claim value. Anything else — an access token, most of all — is
// refused by parseOAuthSignupToken.
const oauthSignupTokenType = "oauth_signup"

// oauthSignupClaims is the identity a signup token carries.
//
// Only what CompleteOAuthSignup needs to create the account: which provider vouched, who they said this
// is, and the address they verified. No display name, no picture, nothing the person has not yet agreed
// to have stored.
type oauthSignupClaims struct {
	jwt.RegisteredClaims

	TokenType string `json:"typ"`
	Provider  string `json:"prv"`
	// The provider's user ID travels in the registered `sub` claim rather than a custom one: it is the
	// subject in the ordinary sense, and it keeps the token readable with any standard JWT tool.
	Email string `json:"eml"`
	// EmailVerified is carried even though only a verified identity ever reaches this point, so that a
	// future path which mints one differently cannot silently skip the check.
	EmailVerified bool   `json:"evf"`
	DisplayName   string `json:"nam"`
	// FlowChallenge carries the client binding across the username step.
	//
	// The signup path is the one place a flow's binding could be lost: the callback ends on a form, and the
	// exchange code is only minted once that form comes back. Rebuilding the binding from the submission
	// would mean trusting the submitter, which is the thing being guarded against — so it rides inside the
	// signed token, where the signature makes it as tamper-proof as the identity beside it.
	FlowChallenge string `json:"flw"`
	// ClientRedirect carries the loopback listener across the username step, for the same reason
	// FlowChallenge does and against the same party.
	//
	// The form is submitted by whoever is looking at it. If the redirect came back as a hidden field, the
	// submitter would choose where the exchange code is delivered — which is precisely the choice the
	// binding beside it exists to take away from them. Inside the signature it is as tamper-proof as the
	// identity.
	//
	// Empty for a flow that named no listener, which is every flow a browser starts.
	ClientRedirect string `json:"rdr,omitempty"`
	// DeviceCode carries which device authorization is waiting on this sign-up, for a flow started from
	// the verification page (M9). Inside the signature for the same reason as the two above and against
	// the same party: the username form is submitted by whoever is looking at it, and a hidden field here
	// would let them choose whose machine gets authorized.
	//
	// A decimal string rather than a number, because JSON numbers are float64 on the way back through and
	// a snowflake does not survive that — the same reason every ID this API emits is quoted.
	DeviceCode string `json:"dvc,omitempty"`
}

// oauthSignupContinuation is what a valid continuation token carries.
//
// A struct rather than three return values, because two of the three are strings with very different
// meanings and the third is a hash — an argument list nobody should have to get right by position.
type oauthSignupContinuation struct {
	Identity  OAuthIdentity
	Challenge TokenHash
	// ClientRedirectURI is where to send the browser once the account exists, or empty to render.
	ClientRedirectURI string
	// DeviceCodeID is the authorization waiting on this sign-up, or zero.
	DeviceCodeID int64
}

// issueOAuthSignupToken mints the token that stands in for an account that does not exist yet.
func (s *Service) issueOAuthSignupToken(identity OAuthIdentity, dest oauthDestination) (string, error) {
	now := s.now()

	deviceCode := ""
	if dest.forDevice() {
		deviceCode = strconv.FormatInt(*dest.DeviceCodeID, 10)
	}

	claims := oauthSignupClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			Subject:   identity.UserID,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(OAuthSignupTTL)),
			ID:        newJTI(),
		},
		TokenType:      oauthSignupTokenType,
		Provider:       string(identity.Provider),
		Email:          identity.Email,
		EmailVerified:  identity.EmailVerified,
		DisplayName:    identity.DisplayName,
		FlowChallenge:  OAuthFlowChallengeFor(dest.Challenge),
		ClientRedirect: dest.Redirect,
		DeviceCode:     deviceCode,
	}

	signed, err := s.issuer.sign(claims)
	if err != nil {
		return "", fmt.Errorf("signing oauth signup token: %w", err)
	}
	return signed, nil
}

// parseOAuthSignupToken validates a continuation token and returns what it carries.
func (s *Service) parseOAuthSignupToken(raw string) (oauthSignupContinuation, error) {
	var claims oauthSignupClaims

	_, err := jwt.ParseWithClaims(raw, &claims, s.issuer.keyFunc,
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(tokenIssuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(s.issuer.now),
	)
	if err != nil {
		return oauthSignupContinuation{}, ErrOAuthSignupToken
	}

	// The check that makes `typ` worth carrying: an access token is signed with the same key, and without
	// this an ordinary user's access token would be accepted here as a signup for an account of their
	// choosing.
	if claims.TokenType != oauthSignupTokenType {
		return oauthSignupContinuation{}, ErrOAuthSignupToken
	}
	if !ValidOAuthProvider(claims.Provider) || claims.Subject == "" || claims.Email == "" {
		return oauthSignupContinuation{}, ErrOAuthSignupToken
	}
	// A token minted without a verified address should not exist, since resolveOAuthIdentity refuses that
	// case before ever getting here. Checked anyway: this is the one place the linking rule could be
	// bypassed by a future caller, and the cost of the check is a comparison.
	if !claims.EmailVerified {
		return oauthSignupContinuation{}, ErrOAuthSignupToken
	}

	// A token with no usable binding cannot produce a redeemable code, so it is refused here rather than
	// allowed to create an account whose sign-in then fails.
	challenge, err := ParseOAuthFlowChallenge(claims.FlowChallenge)
	if err != nil {
		return oauthSignupContinuation{}, ErrOAuthSignupToken
	}

	// Re-validated rather than trusted, exactly as the challenge above is, and for the same reason its
	// comment gives: a backstop against a future caller minting a token differently. The cost is one parse
	// of a value this service itself wrote.
	redirect, err := ParseOAuthClientRedirect(claims.ClientRedirect)
	if err != nil {
		return oauthSignupContinuation{}, ErrOAuthSignupToken
	}

	// Zero unless this sign-up came from the verification page. Parsed rather than carried as a number for
	// the reason the claim's comment gives.
	var deviceCodeID int64
	if claims.DeviceCode != "" {
		deviceCodeID, err = strconv.ParseInt(claims.DeviceCode, 10, 64)
		if err != nil || deviceCodeID == 0 {
			return oauthSignupContinuation{}, ErrOAuthSignupToken
		}
	}

	return oauthSignupContinuation{
		Identity: OAuthIdentity{
			Provider:      OAuthProviderName(claims.Provider),
			UserID:        claims.Subject,
			Email:         claims.Email,
			EmailVerified: claims.EmailVerified,
			DisplayName:   claims.DisplayName,
		},
		Challenge:         challenge,
		ClientRedirectURI: redirect,
		DeviceCodeID:      deviceCodeID,
	}, nil
}

// sign produces a token from any claims this package defines.
//
// Shared with Issue rather than duplicated so there is exactly one place the signing method is chosen. A
// second signing call site is how a token eventually gets signed with a different algorithm than the one
// Verify pins.
func (t *TokenIssuer) sign(claims jwt.Claims) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.key)
}

// keyFunc supplies the verification key, pinning the algorithm family.
//
// Extracted so every parse in this package uses the same one. Without the type assertion a token whose
// header says `"alg":"none"` would verify, which is the single most-exploited JWT misconfiguration there
// is — and it has to hold for the signup token exactly as it does for an access token.
func (t *TokenIssuer) keyFunc(token *jwt.Token) (any, error) {
	if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
		return nil, errors.New("unexpected signing method")
	}
	return t.key, nil
}
