# Security Policy

Norite handles user accounts, authentication, credential/key material (auth tokens, E2E key material), and
user-generated content across a multi-tenant permission model on every instance that runs it — flagship or
self-hosted — so security reports are taken seriously regardless of the project's personal-use-first stage.
See `docs/architecture.md`'s security deep-dive section for the threat model and security design, and
`CLAUDE.md` for the non-negotiable engineering rules meant to prevent common classes of vulnerability (authz
bypass, injection, XSS, token/secret leakage, SSRF, E2E key-boundary violations).

## Reporting a vulnerability

**Please do not open a public GitHub issue for security vulnerabilities.**

Preferred: use [GitHub's private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing/privately-reporting-a-security-vulnerability)
on this repository (Security tab → "Report a vulnerability"). This creates a private advisory visible only
to maintainers until a fix is ready.

Please include:
- A description of the vulnerability and its potential impact.
- Steps to reproduce (a minimal repro is very helpful).
- The affected version/commit, if known.

## What to expect

- Acknowledgement of your report as soon as practical.
- We'll work with you to understand and confirm the issue, develop a fix, and coordinate disclosure timing
  before any public advisory or patch release that would reveal the vulnerability.
- Credit in the advisory/release notes, if you'd like it.

## How a fix is developed and released

The promise above — to coordinate disclosure timing *before any public advisory or patch release that would
reveal the vulnerability* — is not something a public repository keeps by default. The fix commit is visible
the moment it is pushed, self-hosters have not upgraded yet, and for most classes of bug the diff **is** the
exploit. AGPL §13's source offer will sharpen it further once that ships: it names the exact revision the
flagship is running, so an attacker reading a fix commit will know immediately whether the live instance is
affected. That is a deliberate consequence of the license and is accepted as a limitation
(`architecture.md` §17) — which is precisely what makes the workflow below load-bearing rather than a
formality.

So, for anything reported through the private channel above:

- **The fix is developed privately**, in a GitHub security advisory's private fork or an equivalent private
  branch — never as a commit pushed to the public repository ahead of the release. Private vulnerability
  reporting is already enabled on this repository, and the private fork it creates is the intended place.
- **The patched release, the advisory, and the public commit go out together**, not over a period of days.
  A fix that lands publicly before the release it belongs to is the same as publishing the vulnerability.
- **Self-hosters are notified through the advisory**, which is the only channel that reaches them; the
  flagship is upgraded before or at the same time, never after.
- **The commit message and the advisory describe the vulnerability plainly** once both are public. Vague
  commit subjects to obscure a security fix are not a substitute for the timing above — they delay
  discovery by defenders and self-hosters more than by anyone reading the diff.

Nothing here is live yet: no release has shipped and the flagship accepts no non-developer account before
v1 (ADR 0032's release posture), so there is currently nobody to coordinate with. This section is written
now rather than at the first phase beta because that is the point at which a build first reaches somebody
else, and it is a bad moment to be designing a disclosure process.

## Scope

This is self-hosted software — each deployment/instance operator is responsible for their own operational
security (TLS termination, secrets management, network exposure, keeping their deployment up to date). This
policy covers vulnerabilities in the project's own code (backend, frontend, migrations, default
configuration), not misconfiguration of a specific self-hosted instance.
