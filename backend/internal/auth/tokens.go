package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Opaque-token sizing and presentation.
const (
	// tokenEntropyBytes is 256 bits, per docs/architecture.md §2. Well past any brute-force reach, and the
	// tokens are looked up by an indexed hash so length costs nothing at verification time.
	tokenEntropyBytes = 32

	// refreshTokenPrefix and apiTokenPrefix make a leaked credential identifiable on sight — in a paste, a
	// log someone shouldn't have written, or a public repository. The same reasoning as GitHub's `ghp_`
	// convention: secret scanners key off a distinctive prefix, and a human who sees one knows immediately
	// what they are looking at and what to revoke.
	refreshTokenPrefix = "nrt_"
	apiTokenPrefix     = "nat_"
	// passwordResetPrefix marks a reset token. Deliberately absent from LooksLikeOpaqueToken: a reset
	// token authenticates exactly one endpoint, and routing it to the Bearer verifier would be the first
	// step toward it ever authenticating anything else.
	passwordResetPrefix = "nrp_"
)

// ErrMalformedToken reports a token that cannot be a Norite token at all — wrong prefix, wrong length, not
// valid base64. Distinguished from "no such token" only server-side; clients are told the same thing either
// way, so a caller cannot use the difference to probe.
var ErrMalformedToken = errors.New("malformed token")

// TokenHash is the SHA-256 of a raw token, which is the only form ever stored.
//
// SHA-256 rather than argon2id, and that difference is deliberate rather than an oversight: these tokens
// are 256 bits of output from crypto/rand, so there is no guessable input for a slow hash to protect. The
// threat a password KDF defends against — an attacker brute-forcing a low-entropy human choice from a
// stolen hash — does not exist here, and a slow hash on the token-verification path would instead make
// every authenticated request pay ~50ms.
type TokenHash []byte

// HashToken reduces a raw token to its stored form.
func HashToken(raw string) TokenHash {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

// Equal reports whether two hashes match, in constant time.
func (h TokenHash) Equal(other TokenHash) bool { return constantTimeEquals(h, other) }

// GenerateRefreshToken returns a new refresh token and its stored hash.
//
// The raw value is returned exactly once, to be put in the issuing response and then forgotten by the
// server. Nothing recoverable from the database can be presented as a credential (CLAUDE.md rule 8).
func GenerateRefreshToken() (raw string, hash TokenHash, err error) {
	return generateOpaqueToken(refreshTokenPrefix)
}

// GenerateAPIToken returns a new scoped API token and its stored hash.
func GenerateAPIToken() (raw string, hash TokenHash, err error) {
	return generateOpaqueToken(apiTokenPrefix)
}

// GeneratePasswordResetToken mints a single-use reset token.
func GeneratePasswordResetToken() (raw string, hash TokenHash, err error) {
	return generateOpaqueToken(passwordResetPrefix)
}

// ParsePasswordResetToken hashes a raw reset token for lookup, rejecting anything of the wrong shape.
func ParsePasswordResetToken(raw string) (TokenHash, error) {
	return parseOpaqueToken(raw, passwordResetPrefix)
}

func generateOpaqueToken(prefix string) (string, TokenHash, error) {
	buf := make([]byte, tokenEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing means the system has no usable entropy source. There is no safe fallback —
		// emitting a predictable credential would be far worse than refusing to issue one.
		return "", nil, fmt.Errorf("generating token: %w", err)
	}

	// Raw (unpadded) URL-safe base64: no '=' to be mangled by a URL, a shell, or a TOML file, and no '+'
	// or '/' to be re-encoded by something in the middle.
	raw := prefix + base64.RawURLEncoding.EncodeToString(buf)
	return raw, HashToken(raw), nil
}

// ParseRefreshToken validates a refresh token's shape and returns its hash.
//
// Shape-checking before touching the database turns a flood of obvious junk into a cheap rejection rather
// than an indexed lookup each. It deliberately does not report *which* check failed.
func ParseRefreshToken(raw string) (TokenHash, error) {
	return parseOpaqueToken(raw, refreshTokenPrefix)
}

// ParseAPIToken validates an API token's shape and returns its hash.
func ParseAPIToken(raw string) (TokenHash, error) { return parseOpaqueToken(raw, apiTokenPrefix) }

func parseOpaqueToken(raw, prefix string) (TokenHash, error) {
	if !strings.HasPrefix(raw, prefix) {
		return nil, ErrMalformedToken
	}
	body := raw[len(prefix):]

	decoded, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(decoded) != tokenEntropyBytes {
		return nil, ErrMalformedToken
	}
	return HashToken(raw), nil
}

// LooksLikeOpaqueToken reports whether a bearer credential is one of this package's opaque tokens rather
// than a JWT.
//
// The middleware needs to route a credential to the right verifier, and guessing by trying both in turn
// would mean a failed JWT parse on every API-token request. A prefix check is exact and free.
func LooksLikeOpaqueToken(raw string) bool {
	return strings.HasPrefix(raw, apiTokenPrefix) || strings.HasPrefix(raw, refreshTokenPrefix)
}

// RedactToken renders a token safe to appear in a log line.
//
// Nothing in this codebase should be logging a token at all (CLAUDE.md rule 8), but debugging pressure is
// real and an obvious safe helper is a better outcome than someone reaching for the raw value. It keeps
// only the prefix, which identifies the *kind* of credential without revealing any of its entropy.
func RedactToken(raw string) string {
	for _, prefix := range []string{refreshTokenPrefix, apiTokenPrefix} {
		if strings.HasPrefix(raw, prefix) {
			return prefix + "…"
		}
	}
	return "…"
}
