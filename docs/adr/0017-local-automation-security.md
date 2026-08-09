# ADR 0017: Local automation trust tiers — scoped tokens, dual IPC, integrated shell

## Status
Accepted

## Context
Local bot automation, CLI piping, and an integrated shell all let something other than the interactive user
act through the client — each needs an explicit trust-boundary decision so "local automation" doesn't quietly
become a privilege-escalation path.

## Decision
**Local bot-automation port**: a separate, localhost-only TCP listener with its own per-session secret
(a `0600` file or environment variable), authenticated via scoped `api_tokens` (ADR 0011) — kept deliberately
separate from the daemon-attach Unix socket (ADR 0010), because external scripts in arbitrary languages need
a plain TCP/HTTP surface and must **not** receive the same trust level as first-party clients. Messages sent
through it are visually tagged in the UI via the shared `messages.type` "sent via automation" value, the same
mechanism incoming webhooks use.

**Integrated shell**: spawns the user's actual shell — the same trust boundary as a real terminal, with no
extra sandboxing layer. It's not a new capability the user didn't already have; it's a convenience surface
for a capability they already had via any other terminal on the same machine.

## Consequences
- Any new local IPC surface must state its trust tier explicitly in its own design (`CLAUDE.md` rule 16) —
  OS-permission-protected (first-party) vs. secret-protected (external scripts) — the two are never
  interchangeable, and a future feature must pick one deliberately rather than assuming.
- Scoped tokens and local bot automation must respect the `blocks` table like every other delivery path
  (ADR 0013) — automation is not a way to route around a block.
- The integrated shell inherits whatever access the user already has on their own machine; it adds no new
  privilege boundary to reason about beyond "this is really just a terminal."

## Alternatives considered
- **One shared trust tier for both first-party attach clients and external automation scripts**: rejected —
  collapses a meaningful distinction (an OS-permission-protected socket only the owning user can open, vs. a
  TCP port needing its own secret) that exists specifically to keep external script access from silently
  inheriting first-party client trust.
- **Sandbox the integrated shell**: rejected — the user already has an unsandboxed shell available via any
  other terminal; sandboxing this one specifically would be a false sense of security, not a real boundary.
