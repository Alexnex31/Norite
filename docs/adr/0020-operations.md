# ADR 0020: Client auto-update, self-hosting ops (migrations, HTTPS, email, telemetry)

## Status
Accepted

## Context
A daemon-based client architecture that self-updates, plus a self-hosting story that needs to stay
one-command-simple, both need real operational decisions: how updates are verified and rolled back, how
schema migrations run without an operator thinking about it, and what the project's telemetry/privacy
posture actually is.

## Decision
**Auto-update is client-side only.** The daemon (plus the CLI/GUI binaries it manages) self-updates by
polling a version-check endpoint, verifying a **Sigstore/cosign** signature (keyless, OIDC identity + public
Rekor transparency log) via self-contained, offline-verifiable bundles — never a live call to Sigstore's
infrastructure at update-check time, both for reliability and to avoid a phone-home dependency. Anti-
downgrade protection (refuses an older, validly-signed version without an explicit force), fail-closed on
verify failure (current version keeps running, never falls back to unverified), and auto-rollback on
repeated crash-loop after an update (the daemon keeps the previous binary). This Sigstore/cosign scheme is
**deliberately separate from** the license file's own Ed25519-JWT signing scheme (ADR 0007) — publishing
every issued license file's signature to Sigstore's public transparency log, as release binaries are, would
leak customer purchase records. **The backend/server binary is explicitly not auto-updated** — unattended
updates to a server holding other people's data is a materially bigger risk than a client updating itself;
it surfaces a passive "update available" notice to Instance Admins instead. Code signing is self-signed/
unsigned for now (personal/early-use phase); real paid signing is a tracked gap before commercial
distribution, alongside the final reviewed license text (ADR 0007).

**Schema migrations auto-run on backend startup**, guarded by a Postgres advisory lock (so two accidentally-
concurrent processes never race), and **block startup** — `/healthz` stays unavailable until complete — so a
self-hosted single-process instance gets the same "never serves against a not-yet-migrated schema" guarantee
the flagship's Helm `pre-upgrade` Job hook gives that deployment (ADR 0021). Same `golang-migrate` tooling
both places; only the trigger differs, for the same reason ACME differs by deployment shape below.

**Automatic HTTPS** via `caddyserver/certmagic` is built into the backend for self-hosted instances — point
it at a domain, it provisions/renews its own cert — itself a deployment-time opt-out for LAN-only self-
hosters. The flagship deployment disables this path entirely and uses `cert-manager` instead (ADR 0021),
since every replica independently managing the same Let's Encrypt cert would race and hit rate limits.

**Transactional email (SMTP)** is a generic relay an Instance Admin configures, itself a deployment-time
opt-out — password-reset-via-email and the email half of matchmaking's anti-abuse gate (ADR 0013) are simply
unavailable without it, and the instance still runs. Email sending is always asynchronous/backgrounded, never
blocking the HTTP response.

**Base rate limiting** (`ulule/limiter`) is wired in from the foundational backend milestone, not left
implicit — in-memory store self-hosted, Redis-backed for the flagship (ADR 0021) — with IPv6 traffic grouped
by `/64` subnet globally, not exact address, since a single actor trivially controls an entire `/64` block.
`pgx` connection-pool sizing per backend replica is kept intentionally small and documented, with PgBouncer
recommended once a self-hoster's instance/device count outgrows a small direct pool.

**Data retention** is a configurable, default-**disabled** pruning seam for `audit_log_entries`/
`instance_audit_log` only — never message history or reports, which stay permanent by design (both are core
product data users expect to keep, not an operational-storage concern to silently solve by deletion).

**Telemetry** is opt-in crash reports only, posted to a self-operated endpoint, never a third-party APM
vendor (a crash report can contain local file paths/usernames) — no usage/behavioral analytics, ever.

## Consequences
- Two genuinely different signing schemes exist in the system (release-binary Sigstore/cosign vs.
  license-file Ed25519-JWT) for two different reasons — never described as one mechanism.
- A self-hoster on a LAN/Tailscale network with no public domain, no SMTP relay, and voice disabled (ADR
  0012) still gets a fully working instance — every one of these is an independent opt-out, not a bundled
  all-or-nothing feature flag.
- The migration advisory-lock/blocking-startup design and the base rate-limiter wiring are both foundational
  enough that every later milestone can simply assume they exist, the same way the permission engine or
  audit logging can be assumed once built.

## Alternatives considered
- **Auto-update the backend/server too**: rejected — unattended updates to software holding other users'
  data crosses a real risk line a client self-update doesn't.
- **A single signing scheme for both releases and licenses**: rejected — the transparency-log property that
  makes release signing trustworthy is the exact property that would leak customer purchase data for licenses.
- **Rate limiting left implicit/ungoverned until a feature needs it**: rejected once identified as a gap —
  several features (password reset, report filing, matchmaking) already assumed it existed; better to make
  it real foundational infrastructure than have it silently missing under multiple later features.
