package httpx

import (
	"net"
	"net/http"
	"strings"
)

// RealIP rewrites r.RemoteAddr to the client address reported by a trusted reverse proxy, so every
// downstream layer — the rate limiter above all — sees one consistent answer to "who is the client".
//
// Mount it ONLY when the process actually sits behind a proxy you control. It is the router's single
// decision point for client identity; nothing downstream re-reads forwarded headers.
//
// # Why not chi's middleware.RealIP
//
// chi's version is deprecated as vulnerable to spoofing (GHSA-3fxj-6jh8-hvhx and friends), and the flaw
// matters here specifically. It takes the *leftmost* X-Forwarded-For entry. But XFF is append-only: a
// proxy appends the peer it saw to whatever the client already sent. So a client that sends
//
//	X-Forwarded-For: 198.51.100.9
//
// arrives at the application as
//
//	X-Forwarded-For: 198.51.100.9, 203.0.113.7      <- real client appended by the proxy
//
// and the leftmost value is the one the attacker chose. Since the client IP is the rate limiter's
// grouping key, that hands every caller an unlimited supply of fresh identities — even behind a perfectly
// well-configured proxy.
//
// This implementation counts from the right instead. With `hops` trusted proxies between the client and
// this process, the client's real address is the entry `hops` positions from the end: each trusted proxy
// contributes exactly one appended entry, and everything to the left of those is client-supplied and
// therefore worthless.
//
// If the header is missing or has fewer entries than there are trusted hops, r.RemoteAddr is left alone.
// That yields the proxy's own address rather than the client's — less precise, but never attacker-chosen,
// which is the right way to fail here.
//
// X-Real-IP and True-Client-IP are deliberately ignored. They carry no positional information, so there
// is no way to tell a value a trusted proxy set from one a client sent; honoring them would reopen the
// hole this function exists to close.
//
// # Known gap: the immediate peer is not verified (closed at Milestone M114)
//
// This trusts X-Forwarded-For whenever the caller enabled it, without checking that r.RemoteAddr is
// actually one of the trusted proxies. That is sound wherever the process is reachable *only* through its
// proxy, which covers the self-hosted deployment shape. It is not sound on Kubernetes: any pod can dial
// the API Service directly, bypassing the Ingress, and one forged header per request would then mint an
// unlimited supply of rate-limit identities. M114 adds a configurable trusted-peer CIDR list checked
// before any forwarded header is read — see docs/roadmap.md (M114, Phase P) and docs/architecture.md §14.18.
func RealIP(hops int) func(http.Handler) http.Handler {
	if hops < 1 {
		hops = 1
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ip := forwardedClientIP(r, hops); ip != "" {
				// Keep the documented "IP:port" shape of RemoteAddr. The forwarded header carries no
				// client port, so the proxy's is reused — it is meaningless either way, but a bare IP
				// here would break any consumer doing the conventional net.SplitHostPort(r.RemoteAddr)
				// and bailing on error, and it would do so *only* on proxied requests, which
				// direct-connection tests never cover.
				r.RemoteAddr = net.JoinHostPort(ip, originalPort(r.RemoteAddr))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// originalPort returns the port from the socket peer's address, or "0" when it cannot be determined.
func originalPort(remoteAddr string) string {
	if _, port, err := net.SplitHostPort(remoteAddr); err == nil {
		return port
	}
	return "0"
}

// forwardedClientIP returns the client address from X-Forwarded-For, counting hops from the right, or ""
// when the header cannot be trusted to contain one.
func forwardedClientIP(r *http.Request, hops int) string {
	// Multiple X-Forwarded-For header lines are equivalent to one comma-joined line (RFC 7230 §3.2.2),
	// and proxies do emit them separately, so flatten before counting positions.
	var entries []string
	for _, header := range r.Header.Values("X-Forwarded-For") {
		for _, part := range strings.Split(header, ",") {
			if part = strings.TrimSpace(part); part != "" {
				entries = append(entries, part)
			}
		}
	}

	idx := len(entries) - hops
	if idx < 0 {
		// Fewer entries than trusted hops: either the header is absent, or something upstream is not
		// appending as configured. Either way there is no trustworthy value to take.
		return ""
	}

	// An entry may be "ip" or "ip:port"; and IPv6 may or may not be bracketed.
	candidate := entries[idx]
	if host, _, err := net.SplitHostPort(candidate); err == nil {
		candidate = host
	}
	candidate = strings.Trim(candidate, "[]")

	if net.ParseIP(candidate) == nil {
		return ""
	}
	return candidate
}
