package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
)

// Sealing the one credential in this schema that cannot be hashed.
//
// # Why this exists at all
//
// Everything else here is stored as a hash — passwords under argon2id, refresh tokens, API tokens, reset
// tokens and recovery codes under SHA-256 — because nothing ever needs the original back. Verifying a TOTP
// code is different in kind: it recomputes HMAC over the shared secret and compares, so the secret has to
// survive in recoverable form. Stored bare, a single SELECT would be a permanent bypass of the factor,
// which is the opposite of what the factor is for.
//
// So it is sealed, and the asymmetry is stated rather than smoothed over (ADR 0031). A database compromise
// that *also* yields instance.toml yields every enrolled secret, because the key below is derived from the
// instance signing key. That is a real weakening relative to the rest of this schema. The alternative is an
// HSM or a KMS, which is infrastructure this project does not have and a self-hosted instance would not
// have either — and it concedes nothing new in the operator's direction, since ADR 0029 already treats
// whoever can read that file as the highest authority on the instance.
//
// # Why the key is derived rather than reused
//
// The signing key signs JWTs. Using those same bytes as an AES key would mean one secret doing two jobs
// with two different algorithms, which is how key-reuse weaknesses get built. HKDF with a fixed info string
// gives a key that is independent of the signing use and stays stable across restarts — no new
// configuration setting, no migration when a value that was never configured changes.
//
// The consequence to know about: rotating the signing key invalidates every enrolled authenticator, since
// the derived key moves with it. There is no key rotation today; when there is, re-enrolment is the answer
// and this is the paragraph that says so.

// totpSecretInfo is the HKDF info string. Fixed, and distinct from any other derivation this package might
// add later — that distinctness is the whole point of the parameter.
const totpSecretInfo = "norite/totp-secret/v1"

// ErrSealedSecretInvalid means a stored secret could not be opened: truncated, corrupt, or sealed under a
// different instance key. Undifferentiated for the reason every other credential failure here is — the
// three cases are indistinguishable to anyone who should be asking.
var ErrSealedSecretInvalid = errors.New("the stored authenticator secret could not be read")

// totpAEAD builds the AEAD used to seal and open TOTP secrets.
func totpAEAD(signingKey []byte) (cipher.AEAD, error) {
	// Salt is nil deliberately. HKDF's salt exists to decorrelate derivations from *low-entropy* inputs;
	// this input is the instance signing key, which config validation already requires to be at least
	// MinSigningKeyLength of it. The info string is what separates this derivation from any other.
	derived, err := hkdf.Key(sha256.New, signingKey, nil, totpSecretInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("deriving the secret-sealing key: %w", err)
	}

	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, fmt.Errorf("building the secret cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("building the secret AEAD: %w", err)
	}
	return aead, nil
}

// sealTOTPSecret encrypts a base32 TOTP secret for storage.
//
// The nonce is random per call and prepended to the ciphertext, so re-sealing the same secret produces
// different bytes and a row cannot be compared against another to learn that two accounts share one.
func (t *TokenIssuer) sealTOTPSecret(secret string) ([]byte, error) {
	aead, err := totpAEAD(t.key)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generating the secret nonce: %w", err)
	}

	// Seal appends to its first argument, so passing the nonce puts it in front of the ciphertext and the
	// two travel as one value — there is no second column to keep in step, and no way to store one without
	// the other.
	return aead.Seal(nonce, nonce, []byte(secret), nil), nil
}

// openTOTPSecret recovers a secret sealed by sealTOTPSecret.
func (t *TokenIssuer) openTOTPSecret(sealed []byte) (string, error) {
	aead, err := totpAEAD(t.key)
	if err != nil {
		return "", err
	}

	if len(sealed) < aead.NonceSize() {
		return "", ErrSealedSecretInvalid
	}
	nonce, ciphertext := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]

	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// Not wrapped: the underlying error distinguishes authentication failure from a malformed input,
		// and neither is a distinction the caller should act on or the log should carry near a secret.
		return "", ErrSealedSecretInvalid
	}
	return string(plaintext), nil
}
