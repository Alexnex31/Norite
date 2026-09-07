package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testIssuer(t *testing.T) *TokenIssuer {
	t.Helper()
	issuer, err := NewTokenIssuer([]byte(testSigningKey))
	require.NoError(t, err)
	return issuer
}

func TestASealedSecretRoundTrips(t *testing.T) {
	issuer := testIssuer(t)
	const secret = "JBSWY3DPEHPK3PXP"

	sealed, err := issuer.sealTOTPSecret(secret)
	require.NoError(t, err)
	assert.NotContains(t, string(sealed), secret, "the secret must not survive in the stored bytes")

	opened, err := issuer.openTOTPSecret(sealed)
	require.NoError(t, err)
	assert.Equal(t, secret, opened)
}

// A random nonce per call, so two accounts sharing a secret do not share a row value — and so nobody can
// learn that they do by comparing the columns.
func TestSealingTheSameSecretTwiceDiffers(t *testing.T) {
	issuer := testIssuer(t)

	first, err := issuer.sealTOTPSecret("JBSWY3DPEHPK3PXP")
	require.NoError(t, err)
	second, err := issuer.sealTOTPSecret("JBSWY3DPEHPK3PXP")
	require.NoError(t, err)

	assert.NotEqual(t, first, second)
}

// The key is derived from the instance signing key, so a secret sealed by one instance is unreadable by
// another. That is the property that makes "a database read is not a bypass" true, and the one that makes
// rotating the signing key a re-enrollment event.
func TestASecretSealedByAnotherInstanceIsRefused(t *testing.T) {
	mine := testIssuer(t)
	theirs, err := NewTokenIssuer([]byte("a-different-instance-signing-key-of-sufficient-length"))
	require.NoError(t, err)

	sealed, err := theirs.sealTOTPSecret("JBSWY3DPEHPK3PXP")
	require.NoError(t, err)

	_, err = mine.openTOTPSecret(sealed)
	assert.ErrorIs(t, err, ErrSealedSecretInvalid)
}

// GCM authenticates; a flipped bit is a refusal rather than a wrong answer.
func TestATamperedSecretIsRefused(t *testing.T) {
	issuer := testIssuer(t)

	sealed, err := issuer.sealTOTPSecret("JBSWY3DPEHPK3PXP")
	require.NoError(t, err)
	sealed[len(sealed)-1] ^= 0x01

	_, err = issuer.openTOTPSecret(sealed)
	assert.ErrorIs(t, err, ErrSealedSecretInvalid)
}

func TestATruncatedSecretIsRefusedRatherThanPanicking(t *testing.T) {
	issuer := testIssuer(t)
	for _, short := range [][]byte{nil, {}, {0x01, 0x02, 0x03}} {
		_, err := issuer.openTOTPSecret(short)
		assert.ErrorIs(t, err, ErrSealedSecretInvalid)
	}
}
