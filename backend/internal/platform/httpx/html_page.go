package httpx

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
)

// The API's global CSP is `default-src 'none'; … form-action 'none'`, which is exactly right for a JSON
// API and completely unusable for a page: form-action 'none' forbids the form from submitting anywhere at
// all, so a reset page under it renders and then silently does nothing.
//
// The fix is a per-route override, as SecureHeaders' own comment anticipates — never a loosening of the
// global policy, which would relax every JSON endpoint to buy something only two routes need.

// nonceContextKey is unexported so nothing outside this package can inject a nonce and have a template
// trust it.
type nonceContextKey struct{}

// HTMLPage narrows the response headers for a server-rendered page.
//
// It grants exactly two things the JSON policy denies: a nonce-scoped inline stylesheet, and submitting a
// form back to this origin. Everything else stays denied — no scripts of any kind, no images, no frames,
// no external anything.
//
// 'unsafe-inline' is deliberately not used for the style. A nonce costs one random value per request and
// keeps the policy honest: an injected <style> or <script> cannot execute even if user content somehow
// reached the page, which is the property that makes the CSP worth setting at all.
func HTMLPage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce, err := newNonce()
		if err != nil {
			// crypto/rand failing is unrecoverable, and serving the page without a nonce would mean
			// serving it with a policy that blocks its own stylesheet.
			WriteError(w, r, err)
			return
		}

		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'none'; style-src 'nonce-"+nonce+"'; form-action 'self'; "+
				"base-uri 'none'; frame-ancestors 'none'")
		// A reset URL carries a token in its query string. no-referrer is already set globally, and it
		// matters far more here than on a JSON route: without it, any link or resource on this page would
		// hand the token to a third party.
		h.Set("Referrer-Policy", "no-referrer")
		// The token is in the URL, so no cache anywhere may keep this page.
		h.Set("Cache-Control", "no-store, max-age=0")

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), nonceContextKey{}, nonce)))
	})
}

// NonceFrom returns the CSP nonce for this request, or "" outside an HTMLPage-wrapped route.
func NonceFrom(ctx context.Context) string {
	nonce, _ := ctx.Value(nonceContextKey{}).(string)
	return nonce
}

// newNonce returns 128 bits of randomness, base64 for use in a header and an attribute.
func newNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(buf), nil
}
