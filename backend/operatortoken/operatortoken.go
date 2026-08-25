// Package operatortoken defines the credential that authorizes instance administration before any
// account exists.
//
// # Why this is a public package and internal/auth is not
//
// Every other credential in this system is minted by the server and presented by a client. This one is
// minted by the *client* — `norite instance bootstrap` builds it from the signing key it reads out of
// instance.toml — because the thing it proves is possession of that file, and the server cannot vouch for
// that on the client's behalf.
//
// So the format has two implementers in two modules, and it must have exactly one definition. Go's
// internal/ rule makes backend/internal/auth unreachable from the CLI, which leaves the choice between a
// package here and a second copy of the claim shape in the CLI. A second copy drifts, and the failure mode
// is a bootstrap reporting a bad signature against a token that is perfectly well formed — a message that
// sends an operator looking at their key when the bug is in a struct tag.
//
// daemon/credentials is the same decision made at M7 for the same reason: the daemon module owns what a
// stored credential is, and sits outside internal/ so the CLI can write one.
//
// # What lives here and what does not
//
// The format: the claims, the type, the lifetime, the algorithm pin. Not the authorization decision —
// which requests an operator token may authorize is auth.AuthenticateInstanceAdmin's business, and that
// stays inside the backend where the routes are.
package operatortoken

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Issuer is the `iss` claim every token this project signs carries. It must match internal/auth's
// tokenIssuer, which a test in that package asserts.
const Issuer = "norite"

// TokenType is the `typ` claim separating an operator token from every other JWT signed with the same key.
//
// Without it, every access token on the instance is also an operator token — and so is a device entry
// token, which carries the same issuer, a live expiry, and no subject, because at that point in M9's flow
// nobody has authenticated yet.
const TokenType = "operator"

// TTL is how long a minted token stays valid.
//
// Two minutes, far shorter than any other credential here and sized for its actual use: mint one, make one
// request, discard it. Nothing stores one and nothing refreshes one. The short life matters because this
// is the only credential in the system that cannot be revoked — there is no row to delete, so the clock is
// the only thing that ends it.
const TTL = 2 * time.Minute

// ErrInvalid is the one refusal every way of failing produces.
//
// Undifferentiated deliberately: telling a caller their signature was good but their token had expired
// confirms they hold a genuine signing key.
var ErrInvalid = errors.New("invalid operator token")

// Claims is what an operator token carries, which is as close to nothing as a JWT gets.
//
// No subject: there is no account. No scopes: the authority is not delegable, so there is nothing to
// narrow it to. What the signature proves is the whole message — that whoever produced this could read
// the instance's signing key — and any further claim would be a fact the server can check for itself.
type Claims struct {
	jwt.RegisteredClaims

	TokenType string `json:"typ"`
}

// Issue mints a token proving possession of the instance signing key.
//
// now is a parameter rather than a call to time.Now so the caller's clock is the one that decides, which
// is what lets both sides be tested without waiting.
func Issue(key []byte, now time.Time) (string, error) {
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(TTL)),
		},
		TokenType: TokenType,
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(key)
}

// Verify reports whether raw is a live operator token signed with key.
func Verify(key []byte, raw string, now time.Time) error {
	var claims Claims

	_, err := jwt.ParseWithClaims(raw, &claims, func(token *jwt.Token) (any, error) {
		// The algorithm pin. Without the type assertion a token whose header says `"alg":"none"` would
		// verify, which is the single most-exploited JWT misconfiguration there is.
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrInvalid
		}
		return key, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(Issuer),
		// Required rather than defaulted: an operator token with no expiry never stops working, and for a
		// credential nothing can revoke that means forever.
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(func() time.Time { return now }),
	)
	if err != nil {
		return ErrInvalid
	}

	if claims.TokenType != TokenType {
		return ErrInvalid
	}
	// An operator names no account, and a token that does is not one this package minted. Refused rather
	// than read past: honoring the parts of an unfamiliar token that happen to parse is how a confused
	// deputy starts.
	if claims.Subject != "" {
		return ErrInvalid
	}
	return nil
}
