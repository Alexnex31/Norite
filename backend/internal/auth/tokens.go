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
	// oauthStatePrefix and oauthExchangePrefix mark the two short-lived values the OAuth flow hands out.
	// Both are deliberately absent from LooksLikeOpaqueToken for the same reason nrp_ is: each
	// authenticates exactly one endpoint, and routing either to the Bearer verifier would be the first
	// step toward it authenticating something else.
	oauthStatePrefix    = "nos_"
	oauthExchangePrefix = "noc_"
	// oauthFlowPrefix marks the flow verifier — the secret a client keeps to prove that the sign-in it is
	// redeeming is the one it started. Never stored, never sent to a provider, and it crosses the browser
	// exactly never; only its hash does.
	oauthFlowPrefix = "nof_"
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

// GenerateOAuthState mints the state parameter for an authorization request.
//
// A CSRF token in the OAuth sense: the provider echoes it back, so a callback carrying a state this server
// never issued is a request nobody here started.
func GenerateOAuthState() (raw string, hash TokenHash, err error) {
	return generateOpaqueToken(oauthStatePrefix)
}

// ParseOAuthState hashes a state value for lookup, rejecting anything of the wrong shape.
func ParseOAuthState(raw string) (TokenHash, error) { return parseOpaqueToken(raw, oauthStatePrefix) }

// GenerateOAuthExchangeCode mints the one-time code a client trades for a token pair.
func GenerateOAuthExchangeCode() (raw string, hash TokenHash, err error) {
	return generateOpaqueToken(oauthExchangePrefix)
}

// ParseOAuthExchangeCode hashes an exchange code for lookup.
func ParseOAuthExchangeCode(raw string) (TokenHash, error) {
	return parseOpaqueToken(raw, oauthExchangePrefix)
}

// GenerateOAuthFlowVerifier mints the secret that binds a sign-in to the client that started it.
//
// # Why this exists, given that `state` already exists
//
// A state proves the callback belongs to an authorization request *this server issued*. It does not prove
// it belongs to the request *this client started*, and nothing else in the flow does either — so any
// browser can complete any outstanding state. That gap is login CSRF: an attacker consents with their own
// provider account, hands the resulting callback to someone else, and the exchange code that comes back
// signs the victim into the attacker's account, where everything they write afterwards lands.
//
// This closes it the same way PKCE closes the equivalent gap one leg further out. The client keeps the
// verifier and publishes only its hash, so the code a callback produces is redeemable by the client that
// began the flow and by nobody else — including whoever crafted the link. It is PKCE for the client↔Norite
// hop, which is the one hop the flow had no binding on at all.
//
// Enforced server-side rather than by asking each client to compare the state it sent with the one it got
// back. That check works, and it protects only the clients that remember to implement it; this package's
// standing preference is for the guarantee to live where it cannot be forgotten (see
// ConsumePasswordResetToken's WHERE clause, and the verifier this one is named after).
func GenerateOAuthFlowVerifier() (raw string, challenge TokenHash, err error) {
	return generateOpaqueToken(oauthFlowPrefix)
}

// ParseOAuthFlowVerifier hashes a verifier into the challenge it must match.
func ParseOAuthFlowVerifier(raw string) (TokenHash, error) {
	return parseOpaqueToken(raw, oauthFlowPrefix)
}

// ParseOAuthFlowChallenge decodes the challenge a client presents at /authorize.
//
// Unlike every other value in this file the challenge is not a token — it is already a hash, arriving from
// the client rather than issued to it, so there is no prefix to check and nothing to hash again. Only its
// shape can be validated, which is exactly enough: a challenge that is not 32 bytes cannot be the SHA-256
// of anything, so accepting one would mean recording a binding no verifier could ever satisfy.
func ParseOAuthFlowChallenge(raw string) (TokenHash, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(decoded) != sha256.Size {
		return nil, ErrMalformedToken
	}
	return decoded, nil
}

// OAuthFlowChallengeFor renders a challenge for transport in a URL.
func OAuthFlowChallengeFor(challenge TokenHash) string {
	return base64.RawURLEncoding.EncodeToString(challenge)
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
