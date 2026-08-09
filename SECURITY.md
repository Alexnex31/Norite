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

## Scope

This is self-hosted software — each deployment/instance operator is responsible for their own operational
security (TLS termination, secrets management, network exposure, keeping their deployment up to date). This
policy covers vulnerabilities in the project's own code (backend, frontend, migrations, default
configuration), not misconfiguration of a specific self-hosted instance.
