package auth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/operatortoken"
)

// The two halves of the operator token live in two modules — the CLI mints, the backend verifies — and
// nothing but this asserts they agree. A format with two implementers and no test spanning both is a
// format that drifts, and the failure mode is a bootstrap reporting a bad signature against a token that
// is perfectly well formed: a message that sends an operator hunting for a key problem.
//
// This is the backend side of it. The CLI's TestBootstrapPresentsAnOperatorTokenTheInstanceCanVerify is
// the other, and it verifies with operatortoken directly; between them, a token produced the way the CLI
// produces one reaches the check the middleware actually runs.
func TestATokenMintedTheWayTheCLIMintsOneVerifiesHere(t *testing.T) {
	const key = "an-instance-signing-key-of-at-least-32-bytes"

	// Exactly what cli/internal/instanceadmin calls — no Service, no TokenIssuer, just the key it read
	// off disk and the shared package.
	minted, err := operatortoken.Issue([]byte(key), time.Now())
	require.NoError(t, err)

	issuer, err := NewTokenIssuer([]byte(key))
	require.NoError(t, err)

	assert.NoError(t, issuer.VerifyOperatorToken(minted),
		"a token minted the way the CLI mints one must pass the check the middleware runs")
}
