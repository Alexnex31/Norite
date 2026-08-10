package httpx

import (
	"net/http"
	"strconv"
	"time"
)

// hstsMaxAge is the Strict-Transport-Security lifetime advertised over TLS. One year is the widely-used
// value and the minimum most preload lists accept.
const hstsMaxAge = 365 * 24 * time.Hour

// SecureHeaders sets the response headers that harden this API against browser-side attack classes.
//
// The CLI, GUI, and daemon ignore all of these — they matter for the future web SPA and its BFF layer
// (docs/architecture.md §9), and for the handful of server-rendered pages the backend serves directly
// (the device-code completion page, Milestone M9). Setting them from the foundational milestone means
// those clients arrive to an already-hardened surface rather than needing a retrofit, which is exactly
// what CLAUDE.md rule 21 asks for.
//
// Notably absent: CORS. This API is same-origin with the future BFF and has no browser client today, so
// no cross-origin access is granted rather than granted permissively "for now" — the SPA milestone adds
// an explicit, narrow policy if it turns out to need one.
//
// trustProxyHeaders controls whether X-Forwarded-Proto is consulted to decide the connection is really
// TLS-terminated upstream. It must mirror the router's RealIP decision: honoring a client-settable header
// on a directly-exposed process lets any caller claim HTTPS.
func SecureHeaders(trustProxyHeaders bool) func(http.Handler) http.Handler {
	hsts := "max-age=" + strconv.Itoa(int(hstsMaxAge.Seconds())) + "; includeSubDomains"

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()

			// Content-type sniffing turns an attacker-controlled JSON string or uploaded file into
			// executable HTML in some browsers; nosniff is what keeps the declared type authoritative.
			h.Set("X-Content-Type-Options", "nosniff")
			// Nothing this API serves is ever meant to be framed. X-Frame-Options covers older browsers,
			// the CSP frame-ancestors directive below covers current ones.
			h.Set("X-Frame-Options", "DENY")
			// Never leak a full API URL (which can carry IDs, and on some routes tokens) to a third party
			// via the Referer header.
			h.Set("Referrer-Policy", "no-referrer")
			// A JSON API loads no subresources at all, so the tightest possible policy is also the
			// correct one. Server-rendered pages added later override this per-route rather than
			// loosening it globally.
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
			// Browser features this API has no use for.
			h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=(), payment=()")

			// HSTS is meaningful only over a secure transport — the spec requires user agents to ignore
			// it over plain HTTP, and advertising it anyway would be a lie on the LAN-only, ACME-off
			// self-hosted deployment shape (docs/architecture.md §11, ADR 0020).
			if isTLS(r, trustProxyHeaders) {
				h.Set("Strict-Transport-Security", hsts)
			}

			next.ServeHTTP(w, r)
		})
	}
}

// isTLS reports whether the client's connection to the edge was encrypted.
func isTLS(r *http.Request, trustProxyHeaders bool) bool {
	if r.TLS != nil {
		return true
	}
	return trustProxyHeaders && r.Header.Get("X-Forwarded-Proto") == "https"
}
