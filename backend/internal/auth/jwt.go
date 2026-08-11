package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// AccessTokenTTL is how long an access token stays valid (docs/architecture.md §2).
//
// Short on purpose, and it is what makes revocation work at all: sessions and API tokens are revoked in the
// database instantly, but a JWT is only checked against its own signature, so a stolen access token stays
// usable until it expires. Fifteen minutes bounds that window while keeping refreshes infrequent enough
// that a daemon is not constantly round-tripping.
const AccessTokenTTL = 15 * time.Minute

// MinSigningKeyLength is the shortest signing secret accepted.
//
// HS256 is HMAC-SHA-256, whose security is capped by the key's entropy. 32 bytes matches the hash output
// size, which is the point past which a longer key buys nothing and below which it starts to matter. The
// wizard generates exactly this much; the check exists to catch a hand-edited config with a memorable
// string in it.
const MinSigningKeyLength = 32

// Token type and issuer claims.
const (
	tokenIssuer   = "norite"
	tokenTypeName = "access"
)

// Errors returned by TokenIssuer.
var (
	ErrSigningKeyTooShort = fmt.Errorf("auth: the JWT signing key must be at least %d bytes", MinSigningKeyLength)
	ErrInvalidToken       = errors.New("invalid or expired access token")
)

// Claims is the access-token payload.
//
// Deliberately small. A JWT is a bearer credential the server does not re-read from the database on every
// request, so anything embedded here is a fact frozen for up to AccessTokenTTL — putting a username or a
// permission set in it would mean a rename or a demotion took fifteen minutes to take effect. The subject
// and the session it came from are enough to look up anything current.
type Claims struct {
	jwt.RegisteredClaims

	// TokenType distinguishes access tokens from any other JWT this service might sign later. Without it,
	// a token minted for one purpose could be replayed at another (the "confused deputy" shape).
	TokenType string `json:"typ"`

	// SessionID ties the access token to the refresh session that issued it, so a future revocation check
	// (M11) can invalidate outstanding access tokens by session rather than only by user.
	SessionID string `json:"sid"`
}

// UserID returns the subject as a snowflake.
func (c Claims) UserID() (snowflake.ID, error) { return snowflake.Parse(c.Subject) }

// Session returns the originating session ID.
func (c Claims) Session() (snowflake.ID, error) { return snowflake.Parse(c.SessionID) }

// TokenIssuer mints and verifies access tokens.
type TokenIssuer struct {
	key []byte
	now func() time.Time
}

// NewTokenIssuer builds an issuer over an HS256 signing key.
func NewTokenIssuer(signingKey []byte) (*TokenIssuer, error) {
	if len(signingKey) < MinSigningKeyLength {
		return nil, ErrSigningKeyTooShort
	}
	// Copy: the caller's slice may be reused or zeroed, and a signing key that silently changes underneath
	// the issuer would invalidate every token already issued.
	key := make([]byte, len(signingKey))
	copy(key, signingKey)

	return &TokenIssuer{key: key, now: time.Now}, nil
}

// Issue returns a signed access token for a user and the session it belongs to.
func (t *TokenIssuer) Issue(userID, sessionID snowflake.ID) (string, time.Time, error) {
	now := t.now()
	expiresAt := now.Add(AccessTokenTTL)

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			// A unique ID per token, so a future denylist (M11) can name one token rather than a whole
			// session, and so two tokens issued in the same second are never byte-identical.
			ID: newJTI(),
		},
		TokenType: tokenTypeName,
		SessionID: sessionID.String(),
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.key)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("signing access token: %w", err)
	}
	return signed, expiresAt, nil
}

// Verify parses and validates an access token, returning its claims.
//
// Every failure comes back as ErrInvalidToken. The distinction between "expired", "wrong signature" and
// "malformed" is useful in a log and dangerous in a response: telling a caller their signature was valid
// but the token expired confirms they hold a genuine key, and telling them it was malformed helps them
// iterate toward a well-formed forgery.
func (t *TokenIssuer) Verify(raw string) (Claims, error) {
	var claims Claims

	_, err := jwt.ParseWithClaims(raw, &claims, func(token *jwt.Token) (any, error) {
		// Pin the algorithm. Without this check a token whose header says `"alg":"none"` — or one signed
		// with a different family entirely — would be accepted, which is the single most-exploited JWT
		// misconfiguration there is.
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %q", token.Header["alg"])
		}
		return t.key, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(tokenIssuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(t.now),
	)
	if err != nil {
		return Claims{}, ErrInvalidToken
	}

	// A token of another type must not authenticate a request even when its signature is genuine.
	if claims.TokenType != tokenTypeName {
		return Claims{}, ErrInvalidToken
	}
	if _, err := claims.UserID(); err != nil {
		return Claims{}, ErrInvalidToken
	}
	return claims, nil
}

// newJTI returns a random token identifier.
func newJTI() string {
	raw, _, err := generateOpaqueToken("")
	if err != nil {
		// generateOpaqueToken only fails when crypto/rand does, which is unrecoverable — and an
		// unidentifiable token is better than a predictable one, so the claim is simply left empty.
		return ""
	}
	return raw
}
