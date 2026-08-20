package auth

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The accepted shapes. Kept small on purpose: this parameter decides where a credential is delivered, so
// the set of things it says yes to should be enumerable in one screen.
func TestAcceptedLoopbackRedirects(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"http://127.0.0.1:51763/callback", "http://127.0.0.1:51763/callback"},
		// Any loopback address, not just the conventional one: 127.0.0.0/8 is loopback in its entirety.
		{"http://127.0.0.2:1/cb", "http://127.0.0.2:1/cb"},
		{"http://127.0.0.1:65535/cb", "http://127.0.0.1:65535/cb"},
		// IPv6 loopback is accepted even though this repository's CLI binds v4. The server has no business
		// encoding a client's implementation choice, and a future client may well bind v6.
		{"http://[::1]:51763/callback", "http://[::1]:51763/callback"},
		// An absent path is canonicalised, so what is stored is what will be emitted.
		{"http://127.0.0.1:51763", "http://127.0.0.1:51763/"},
		{"", ""},
	} {
		got, err := ParseOAuthClientRedirect(tc.in)
		require.NoError(t, err, "%q must be accepted", tc.in)
		assert.Equal(t, tc.want, got)
	}
}

// One table, one refusal each, because the value of this function is entirely in what it says no to. Every
// row names the thing it is defending against — a row whose reason nobody can state is a row that will be
// deleted by whoever next finds it inconvenient.
func TestRefusedLoopbackRedirects(t *testing.T) {
	for _, tc := range []struct{ why, in string }{
		{"an ordinary remote host", "http://evil.example/cb"},
		{"a remote host over TLS", "https://evil.example/cb"},
		{"a hostname that merely begins with the loopback address", "http://127.0.0.1.evil.example:51763/cb"},
		{"the link-local metadata address", "http://169.254.169.254/cb"},
		{"a private-range address", "http://10.0.0.1:51763/cb"},
		{"a public address that happens to be numeric", "http://93.184.216.34:51763/cb"},

		// The one refusal a reader will question. localhost is a *name*: it is resolved by the browser
		// through /etc/hosts and DNS, either of which can be made to point off-machine. An IP literal is
		// resolved by nobody, which is the entire guarantee (RFC 8252 §8.3).
		{"the name localhost rather than an address", "http://localhost:51763/cb"},
		{"any other name", "http://my-dev-box:51763/cb"},

		{"userinfo hiding the real host", "http://127.0.0.1:51763@evil.example/cb"},
		{"userinfo even with a loopback host", "http://user:pass@127.0.0.1:51763/cb"},
		{"a scheme-relative URI with no origin of its own", "//evil.example/cb"},
		{"a bare path", "/cb"},
		{"a scheme that is not a browser navigation", "javascript:alert(1)"},
		{"a data URL", "data:text/html,x"},
		{"a local file", "file:///etc/passwd"},
		{"an application scheme", "norite://cb"},
		{"loopback over TLS, which can never present a valid certificate", "https://127.0.0.1:51763/cb"},
		{"an opaque URL with no authority", "http:127.0.0.1:51763/cb"},

		{"no explicit port", "http://127.0.0.1/cb"},
		{"port zero", "http://127.0.0.1:0/cb"},
		{"a port above the range", "http://127.0.0.1:99999/cb"},
		{"a non-numeric port", "http://127.0.0.1:http/cb"},

		{"a query that would collide with the code parameter", "http://127.0.0.1:51763/cb?code=x"},
		{"an empty forced query", "http://127.0.0.1:51763/cb?"},
		{"a fragment", "http://127.0.0.1:51763/cb#f"},

		{"a zoned loopback address", "http://[::1%25eth0]:51763/cb"},
		{"a carriage return, which would split the Location header", "http://127.0.0.1:51763/cb\r\nX: y"},
		{"a newline", "http://127.0.0.1:51763/cb\nX: y"},
		{"a NUL", "http://127.0.0.1:51763/cb\x00"},
		{"a value longer than the cap", "http://127.0.0.1:51763/" + strings.Repeat("a", maxClientRedirect)},
	} {
		got, err := ParseOAuthClientRedirect(tc.in)
		assert.ErrorIs(t, err, ErrOAuthClientRedirect, "%s: %q must be refused", tc.why, tc.in)
		assert.Empty(t, got, "%s: a refusal must return nothing usable", tc.why)
	}
}

// The returned value is the parser's own re-serialisation, not the input. That is what makes "no query, no
// fragment, no userinfo" a property of the stored string rather than of a check that ran once — a later
// reader gets to trust the column without re-deriving whether the parser and the bytes agreed.
func TestTheCanonicalFormIsWhatComesBack(t *testing.T) {
	got, err := ParseOAuthClientRedirect("http://127.0.0.1:51763")
	require.NoError(t, err)

	u, err := url.Parse(got)
	require.NoError(t, err)
	assert.Equal(t, "/", u.Path)
	assert.Empty(t, u.RawQuery)
	assert.Empty(t, u.Fragment)
	assert.Nil(t, u.User)

	// Idempotent, so re-validating a stored value — which parseOAuthSignupToken does — cannot reject what
	// this function itself produced.
	again, err := ParseOAuthClientRedirect(got)
	require.NoError(t, err)
	assert.Equal(t, got, again)
}

// The code is assigned into the query, never appended to whatever was there, and the input is refused if
// it carries a query at all. Both halves matter: this asserts the second half cannot be reached and the
// first half produces exactly one parameter.
func TestTheReturnURLCarriesOnlyWhatItWasGiven(t *testing.T) {
	redirect, err := ParseOAuthClientRedirect("http://127.0.0.1:51763/callback")
	require.NoError(t, err)

	got := oauthReturnURL(redirect, url.Values{"code": {"noc_abc"}})
	u, err := url.Parse(got)
	require.NoError(t, err)

	assert.Equal(t, "127.0.0.1:51763", u.Host)
	assert.Equal(t, "/callback", u.Path)
	assert.Equal(t, url.Values{"code": {"noc_abc"}}, u.Query())
}
