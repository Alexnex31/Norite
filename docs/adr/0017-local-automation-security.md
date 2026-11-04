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
a plain TCP surface and must **not** receive the same trust level as first-party clients. Messages sent by
an API token are visually tagged in the UI via the shared `messages.type` "sent via automation" value, the
same mechanism incoming webhooks use.

**Amended at M22.** This paragraph said "a plain TCP/HTTP surface" while ADR 0010 said the port uses the
attach socket's length-prefixed JSON framing; the framing is what M22 builds, and `norite automation request`
speaks it for a script that cannot. It also said messages "sent through it" are tagged, which left the tag
to the daemon: a script could avoid it by presenting the same token to the instance directly. The instance
writes the tag for any message an API token sends or edits. And the port is closed until the user enables
it.

**Amended at M28: a third tier, the voice-worker's pipes.**
- The daemon spawns `norite-voice` and speaks to it over two anonymous pipes the worker inherits. No other
  process can reach them, so neither the socket's file permissions nor the port's secret applies.
- What sets this tier apart is the direction of trust. The worker is the daemon's own child, and from M29
  it decodes audio a stranger sent, in C. A worker that was exploited holds the pipe.
- So the daemon trusts it least of anything it talks to: every frame is bounded, rated and strictly
  decoded, and the first violation kills it.
- It is given no credential but the TURN one, which reaches the SFU's port and nothing else (ADR 0023).
- Its environment is an allow-list, so nothing in the daemon's own reaches it.
- **What this bounds is a worker that misbehaves on its pipe, and no more.** The worker runs as the account,
  so one that was exploited reaches whatever the account reaches: the attach socket, which is this ADR's
  first tier and lets it act as the account through the daemon, and the credential store. The pipe's bounds
  keep a broken or confused worker from taking the daemon down. They do not confine an exploited one.
  Whether the worker is confined by the OS is M29's decision, owed before it decodes anybody's audio
  (`/security-sweep` at M28).

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
