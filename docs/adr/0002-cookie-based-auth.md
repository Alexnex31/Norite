# ADR 0002: httpOnly cookie auth end-to-end (REST + WS gateway), not frontend-managed JWTs

## Status
**Superseded** by [ADR 0011: Token-based client auth](0011-token-based-client-auth.md) for the CLI and
native GUI, which have no browser cookie jar and are now the primary clients (see
[ADR 0009](0009-cli-and-gui-client-architecture.md)). The reasoning below is **still correct and still
applies** to the web SPA, now the later, third-priority client (Phase O) — when it's built, it gets its own
BFF-style httpOnly-cookie exchange layer in front of the token API described in ADR 0011, so the browser
client still never holds a raw Bearer token in JS. This ADR's historical rationale is kept intact below,
unedited, as the record of why cookies were the right call for a browser client specifically.

## Context
Two independent design passes proposed conflicting token transports: the backend plan defaulted to a JWT
access token sent in the WebSocket `IDENTIFY` frame body (mirroring Discord's real client-token model,
which is fine for native apps); the frontend plan wanted httpOnly cookies with no JS-visible tokens, for
XSS safety (a JS-readable token is exfiltratable by any successful XSS; an httpOnly cookie is not).

## Decision
Access and refresh tokens are set as **httpOnly, Secure, SameSite=Lax cookies** by the backend on
login/refresh/OAuth-callback. The frontend never reads or stores a raw token in JS. Browsers automatically
attach cookies to the WebSocket upgrade HTTP request, so the gateway authenticates the connection **at
upgrade time** from the cookie (plus an `Origin` allow-list check — see the CSWSH mitigation in
`docs/architecture.md` Section 7.5). The `IDENTIFY` frame is reduced to carrying client properties/intents
only; `RESUME` carries `session_id`+`seq` only — never a bearer token.

A second, deliberately **non**-httpOnly `csrf_token` cookie plus an `X-CSRF-Token` header check
(double-submit pattern) protects state-changing REST endpoints, since `SameSite=Lax` alone is not sufficient
CSRF protection.

## Consequences
- No token handling code in the frontend at all — smaller attack surface, no localStorage token storage bug
  class possible.
- Production deployment should be same-origin (frontend embedded in the Go binary, see ADR 0001) to keep
  cookie/CORS handling simple; cross-origin deployments are possible but need explicit CORS +
  `SameSite=None` consideration, not the default path.
- Every new mutating REST route must be checked against the CSRF middleware; every new WS-adjacent code path
  must not assume a token is available in `IDENTIFY`.
- A future native/desktop/mobile client (not in current scope) would need a different token transport, since
  it can't rely on a browser's cookie jar — that's an explicit non-goal for now, not a design constraint we
  paid for today.

## Alternatives considered
- **JWT in a JS-readable store (localStorage/memory) sent in `Authorization`/`IDENTIFY`**: rejected — a
  single successful XSS exfiltrates the token; also the originally-proposed backend design.
