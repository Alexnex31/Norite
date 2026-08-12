package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chi's RequestID adopts an inbound X-Request-Id verbatim. That value is echoed to the client, written
// into every log line for the request, and returned in every error body — so on a directly-exposed
// process it must not be the caller's to choose.

// serveWithSanitizer runs a request through the sanitizer and chi's RequestID, returning the ID the
// handler actually saw.
func serveWithSanitizer(t *testing.T, header string, trustProxyHeaders bool) string {
	t.Helper()

	var seen string
	handler := SanitizeInboundRequestID(trustProxyHeaders)(
		middleware.RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			seen = middleware.GetReqID(r.Context())
		})))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if header != "" {
		req.Header.Set(RequestIDHeaderName, header)
	}
	handler.ServeHTTP(httptest.NewRecorder(), req)

	require.NotEmpty(t, seen, "a request ID must always be present, generated when not adopted")
	return seen
}

func TestInboundRequestIDIsIgnoredWhenProxyHeadersAreNotTrusted(t *testing.T) {
	got := serveWithSanitizer(t, "attacker-chosen-id", false)

	assert.NotEqual(t, "attacker-chosen-id", got,
		"a directly-exposed process must not adopt the caller's correlation ID")
}

// Two callers sending the same header must not end up sharing an ID — that is what makes "quote your
// request ID" useless as a support path.
func TestUntrustedCallersCannotCollideRequestIDs(t *testing.T) {
	first := serveWithSanitizer(t, "same-id-please", false)
	second := serveWithSanitizer(t, "same-id-please", false)

	assert.NotEqual(t, first, second, "unrelated requests must not be collapsable under one ID")
}

func TestInboundRequestIDIsAdoptedFromATrustedProxy(t *testing.T) {
	got := serveWithSanitizer(t, "0af7651916cd43dd8448eb211c80319c", true)

	assert.Equal(t, "0af7651916cd43dd8448eb211c80319c", got,
		"behind a trusted proxy the upstream ID is what correlates the two sides")
}

// Even trusted, the value is bounded and charset-restricted: a proxy typically forwards what the client
// sent rather than minting its own, so "trusted" describes the hop, not the string.
func TestATrustedProxysRequestIDIsStillBoundedAndFiltered(t *testing.T) {
	rejected := map[string]string{
		"too long":     strings.Repeat("a", maxInboundRequestIDLength+1),
		"newline":      "abc\ndef",
		"carriage ret": "abc\rdef",
		"space":        "abc def",
		"quote":        `abc"def`,
		"non-ascii":    "abc\u202edef",
		"control":      "abc\x00def",
		"tab":          "abc\tdef",
	}

	for name, id := range rejected {
		t.Run(name, func(t *testing.T) {
			got := serveWithSanitizer(t, id, true)
			assert.NotEqual(t, id, got, "must not be adopted even from a trusted proxy")
		})
	}

	// The boundary itself is allowed, so the cap is a cap and not an off-by-one.
	atLimit := strings.Repeat("a", maxInboundRequestIDLength)
	assert.Equal(t, atLimit, serveWithSanitizer(t, atLimit, true))
}

func TestRequestIDIsGeneratedWhenNoHeaderIsSent(t *testing.T) {
	assert.NotEmpty(t, serveWithSanitizer(t, "", false))
	assert.NotEmpty(t, serveWithSanitizer(t, "", true))
}

// The nonce must survive html/template's attribute escaping unchanged, or the header and the document
// disagree about what it is.
//
// Standard base64 does not: it contains "+", which html/template writes as "&#43;" inside an attribute. A
// browser decodes the entity before matching so the page still works, which is precisely what makes the
// bug survive review — the two values are simply no longer comparable to anything but a browser. This
// pins the alphabet rather than the symptom.
func TestNonceContainsNothingHTMLWouldEscape(t *testing.T) {
	// Enough draws that a "+" or "/" would appear with near-certainty under standard base64: each of the
	// 22 characters has a 2/64 chance, so a single nonce avoids both about half the time and 200 do not.
	for range 200 {
		nonce, err := newNonce()
		require.NoError(t, err)

		assert.NotEmpty(t, nonce)
		for _, r := range nonce {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			default:
				t.Fatalf("nonce %q contains %q, which is outside the URL-safe base64 alphabet and may be "+
					"escaped into an attribute differently than it appears in the header", nonce, r)
			}
		}
	}
}

// Two nonces must never repeat — the whole guarantee rests on unpredictability per response.
func TestNoncesAreUnique(t *testing.T) {
	seen := make(map[string]struct{}, 500)
	for range 500 {
		nonce, err := newNonce()
		require.NoError(t, err)
		_, duplicate := seen[nonce]
		require.False(t, duplicate, "nonce %q was generated twice", nonce)
		seen[nonce] = struct{}{}
	}
}
