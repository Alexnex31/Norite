# ADR 0011: Token-based auth for CLI/GUI/daemon, superseding cookies for these clients

## Status
Accepted. Supersedes [ADR 0002](0002-cookie-based-auth.md) for the CLI and native GUI specifically; ADR
0002's cookie approach is retained, unchanged, for the future web SPA.

## Context
Neither the CLI nor the native GUI has a browser cookie jar, so ADR 0002's httpOnly-cookie transport doesn't
apply to them at all. These are now the primary clients (ADR 0009), so client auth needs a real token-based
design, not a stopgap.

## Decision
Fully token-based: a short-lived (15-minute) JWT access token plus a refresh token, Bearer-style. The
**daemon is the sole holder** of its account's tokens — one keychain entry (`zalando/go-keyring`), one
process; the CLI and GUI never independently store a token copy, every authenticated action they trigger is
relayed through the daemon over the local IPC socket (ADR 0010). Refresh tokens are scoped per `device_id`,
so rotating one device's daemon never invalidates another device's session/refresh-token family (a user may
run daemons on more than one machine). Personal API tokens support scopes (e.g. `messages:send`-only), not
just one full-access token per user, for bots/automation.

`norite login` supports direct password login and, for OAuth (Google, GitHub), a system-browser-plus-localhost-
callback loopback flow — a fixed registered local port with a documented fallback-port list, since GitHub
OAuth Apps require an exact pre-registered callback URL. A headless/SSH context (no local browser, or
`--no-browser`) falls back to a device-code flow (`gh`/`gcloud`/`aws`-style): a code and URL completed on any
other device with a browser, backed by a `device_code` table and a minimal server-rendered completion page
shipped as part of the backend, independent of the web SPA.

Cookie/CSRF/double-submit auth is retired entirely for the CLI/GUI/daemon REST surface — there's no ambient
browser credential to protect against CSRF for a token-authenticated client. It returns, decided but not yet
built, for the web SPA (Phase O): a BFF-style httpOnly-cookie exchange layer in front of this same token API,
so the browser client still never holds a raw Bearer token in JS.

## Consequences
- Every REST/gateway contract addition should be sanity-checked against real browser constraints (CORS,
  chattiness, BFF-compatibility) as it's written, even though the web SPA isn't built until Phase O — waiting
  until then risks discovering the contracts quietly assumed native-only capabilities.
- The daemon becomes a real secret-holding process that needs the same credential-hygiene rigor as a backend
  service — logging, error messages, and crash reports must never leak a token (same rule as the backend).
- Local bot-automation tokens (scoped `api_tokens`) are a deliberate exception to "daemon holds everything":
  they're secrets for external scripts, not the daemon's own session.

## Alternatives considered
- **Keep cookies for the CLI/GUI via an embedded cookie jar**: rejected — solves nothing a browser gives for
  free (automatic attachment, `httpOnly` protection) and adds real complexity (a fake cookie jar, CSRF logic
  that protects against a threat model — ambient browser credentials — that doesn't exist for these clients).
- **Each attach client (CLI, GUI) independently holds its own token**: rejected — duplicates credential
  storage, and contradicts the daemon being the sole gateway client (ADR 0010); the same reasoning later
  applied to E2E key material (ADR 0014).
