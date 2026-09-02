package auth

import (
	"fmt"
	"net/netip"
	"strconv"

	"github.com/golang-jwt/jwt/v5"
)

// The continuation that carries a half-finished sign-in.
//
// A password login against an account with a second factor cannot return a token pair, so it returns this:
// a signed statement that the password was right, naming the account and the device the eventual session
// belongs to. The client presents it back with a code and gets the pair.
//
// # Why it is signed rather than stored
//
// Every short-lived value in this package that is *stored* is stored because something has to be spent or
// counted — a reset token is single-use, a device code is polled, an OAuth state carries a verifier that
// must not travel through a browser. None of that applies here: nothing about a challenge needs revoking,
// and it authorizes nothing without a code. So it is a signature rather than a row, the same shape as M6's
// OAuth signup continuation, with no cleanup for the sweeper to own.
//
// The two mints are not equally cheap to replay, and the weaker one is the one to reason from. After a
// *password* login, holding a challenge proves nothing an attacker did not already have — they had the
// password, and replaying the challenge buys nothing that re-submitting it would not. After an *OAuth
// exchange* that is not true: the exchange code is single-use and spent, so a captured challenge is worth
// more than what its holder started with — five minutes of second-factor attempts without redoing the
// provider round trip.
//
// What bounds that is the rate limit on /auth/2fa/verify and the five-minute life, not the challenge being
// unforgeable, and it is a bounded window rather than an unlimited one. Recorded because the case for
// storing nothing was originally written from the password path alone; if a challenge ever needs a real
// attempt counter, this is the paragraph that says which path demanded it, and a row is the upgrade.
//
// # What it is not
//
// It is not a credential. It authorizes nothing on its own — presenting one without a valid code produces
// exactly the same refusal as presenting a wrong password. What it removes is the need to send the
// password twice.
//
// The device identity travels *inside* the signature rather than being supplied again with the code. A
// client that could name a different device on the second call could move somebody's session onto a device
// id of its choosing, which is the confusion ADR 0028 avoided by putting the device code id inside the
// device continuation rather than in a form field.

// twoFactorTokenType is this continuation's `typ`. Distinct from every other token this package signs, for
// the reason deviceEntryTokenType gives: an access token must never be spendable as a challenge, and a
// challenge must never be spendable as anything.
const twoFactorTokenType = "two_factor"

// twoFactorClaims is what the challenge carries.
type twoFactorClaims struct {
	jwt.RegisteredClaims

	TokenType string `json:"typ"`
	// The account is in the registered `sub` claim.

	// DeviceID, DeviceName and IP are the session the second call will create. Carried here rather than
	// re-supplied so the client cannot change them between the two halves of one sign-in.
	DeviceID   string `json:"did"`
	DeviceName string `json:"dn,omitempty"`
	IP         string `json:"ip,omitempty"`
}

// twoFactorChallenge is a parsed, valid challenge.
type twoFactorChallenge struct {
	UserID int64
	Login  LoginInput
}

// issueTwoFactorChallenge mints the continuation for an account that owes a factor.
//
// Minted at exactly the points a password or a provider has already resolved to an account — which is what
// makes "holding one proves you had the password" true rather than merely intended.
func (s *Service) issueTwoFactorChallenge(userID int64, in LoginInput) (string, error) {
	now := s.now()

	ip := ""
	if in.IP.IsValid() {
		ip = in.IP.String()
	}

	signed, err := s.issuer.sign(twoFactorClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			Subject:   strconv.FormatInt(userID, 10),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(twoFactorChallengeTTL)),
			ID:        newJTI(),
		},
		TokenType:  twoFactorTokenType,
		DeviceID:   in.DeviceID,
		DeviceName: in.DeviceName,
		IP:         ip,
	})
	if err != nil {
		return "", fmt.Errorf("signing two-factor challenge: %w", err)
	}
	return signed, nil
}

// parseTwoFactorChallenge validates a challenge and recovers the sign-in it belongs to.
//
// The `typ` check is what stops an access token — same issuer, same key, same subject shape, live expiry —
// being presented here to skip the password entirely. That is not hypothetical: it is the exact confusion
// ADR 0029 found for operator tokens, where a device entry token would otherwise have authenticated as the
// instance operator. Both directions have a test.
func (s *Service) parseTwoFactorChallenge(raw string) (twoFactorChallenge, error) {
	var claims twoFactorClaims

	_, err := jwt.ParseWithClaims(raw, &claims, s.issuer.keyFunc,
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(tokenIssuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(s.issuer.now),
	)
	if err != nil {
		return twoFactorChallenge{}, ErrTwoFactorChallenge
	}
	if claims.TokenType != twoFactorTokenType {
		return twoFactorChallenge{}, ErrTwoFactorChallenge
	}

	userID, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil || userID <= 0 {
		return twoFactorChallenge{}, ErrTwoFactorChallenge
	}

	// The device id is re-normalized rather than trusted. It was normalized when the challenge was issued,
	// so this cannot fail in practice — which is exactly why it is cheap to keep: the day something mints
	// a challenge by another route, the session it produces still cannot carry a device id that login
	// would have refused.
	deviceID, err := normalizeDeviceID(claims.DeviceID)
	if err != nil {
		return twoFactorChallenge{}, ErrTwoFactorChallenge
	}

	login := LoginInput{DeviceID: deviceID, DeviceName: claims.DeviceName}
	if claims.IP != "" {
		if addr, err := netip.ParseAddr(claims.IP); err == nil {
			login.IP = addr
		}
	}

	return twoFactorChallenge{UserID: userID, Login: login}, nil
}
