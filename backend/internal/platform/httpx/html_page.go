// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

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

// PageStyle is the stylesheet every server-rendered page shares: the reset, verification and device pages
// in internal/auth, and since easier-setup the invite page in internal/guilds. Here rather than in either
// package, so the second shares it rather than copying it.
//
// One definition rather than one per page: they are seen by someone who is locked out, half-signed-up or
// not yet a user, and two copies drifting would mean the pages slowly stop looking like the same product.
// It is inlined under a per-request nonce rather than served as a file, because a stylesheet at its own URL
// would need a route, a cache policy, and a CSP source — three things to get right for one screenful of CSS.
const PageStyle = `
  body { font-family: system-ui, sans-serif; max-width: 32rem; margin: 4rem auto; padding: 0 1.5rem;
         line-height: 1.5; color: #1a1a1a; background: #fafafa; }
  h1 { font-size: 1.5rem; }
  label { display: block; margin-top: 1.5rem; font-weight: 600; }
  input { width: 100%; padding: 0.6rem; margin-top: 0.4rem; font-size: 1rem;
          border: 1px solid #bbb; border-radius: 4px; box-sizing: border-box; }
  button { margin-top: 1.5rem; padding: 0.6rem 1.2rem; font-size: 1rem; border: 0; border-radius: 4px;
           background: #2b5eaa; color: #fff; cursor: pointer; }
  code.code { display: inline-block; padding: 0.4rem 0.6rem; background: #eee; border-radius: 4px;
              font-size: 1.1rem; word-break: break-all; }
  .note { color: #555; font-size: 0.9rem; }
  .error { color: #a12; font-weight: 600; }
  button.deny { background: #a12; }
  a.provider { display: inline-block; margin: 0.3rem 0.4rem 0 0; padding: 0.6rem 1.2rem;
               border: 1px solid #2b5eaa; border-radius: 4px; color: #2b5eaa; text-decoration: none; }
`

// NonceFrom returns the CSP nonce for this request, or "" outside an HTMLPage-wrapped route.
func NonceFrom(ctx context.Context) string {
	nonce, _ := ctx.Value(nonceContextKey{}).(string)
	return nonce
}

// newNonce returns 128 bits of randomness, encoded so the header and the HTML attribute are byte-identical.
//
// URL-safe base64 rather than standard, and that is not cosmetic. Standard base64 contains "+" and "/", and
// html/template escapes "+" to "&#43;" inside an attribute — so the response header would read
// `nonce-Aha+uhd…` while the document read `nonce="Aha&#43;uhd…"`. A browser decodes the entity before
// matching, so it happens to work, but the two are then no longer comparable to anything else: not to a
// test, not to a proxy, not to anyone reading a response by eye. The URL-safe alphabet (A-Za-z0-9-_) holds
// nothing HTML escapes, and CSP's own base64-value grammar admits "-" and "_" explicitly.
func newNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
