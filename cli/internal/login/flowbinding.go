package login

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
)

// The secret that makes an exchange code redeemable by this process and by nobody else.
//
// Norite's OAuth flow binds the client that started it: the client publishes a hash at /authorize and
// presents the secret at /exchange, so a code that reaches anyone else is worthless. Without it any browser
// could complete any outstanding authorization request, and the code that came back would sign whoever
// opened the link into whichever account consented.
//
// # Why this is duplicated rather than shared
//
// The backend has the same construction in internal/auth. cli and backend are separate Go modules and one
// cannot import the other, so there is no third place to put it that both can reach. The source of truth is
// therefore neither implementation: it is the description of /auth/oauth/{provider}/authorize in
// contracts/openapi.yaml, and each side has a test that builds the pair from that description without
// calling its own helper.

// flowVerifierPrefix marks the secret half, as every opaque credential in this system is marked — a value
// that turns up somewhere it should not be is identifiable on sight.
const flowVerifierPrefix = "nof_"

// mintFlowBinding returns the secret this client keeps and the challenge it publishes.
//
// The one detail worth stating twice, because it is the one a reimplementation gets wrong and it fails only
// at the last request of the flow: the challenge hashes the **whole prefixed string**, not the 32 raw
// bytes.
func mintFlowBinding() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generating a sign-in secret: %w", err)
	}

	verifier = flowVerifierPrefix + base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// authorizeURL builds the link that starts a sign-in.
//
// Safe to print, and that is a property of what goes in it rather than a judgement call: the challenge is a
// hash, publishable by construction, and the verifier it was derived from never appears here. Printing this
// URL is what rescues a person whose browser opened the wrong profile, so it is worth being able to do
// without hesitating (rule 8).
func authorizeURL(instanceURL, provider, challenge, redirectURI string) string {
	query := url.Values{
		"flow_challenge":      {challenge},
		"client_redirect_uri": {redirectURI},
	}
	return strings.TrimSuffix(instanceURL, "/") +
		"/api/v1/auth/oauth/" + provider + "/authorize?" + query.Encode()
}
