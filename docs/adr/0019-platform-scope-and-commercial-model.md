# ADR 0019: Federation and mobile as deliberate non-goals; platform scope boundaries

## Status
Accepted

## Context
Two scope questions recur enough to need one settled, documented answer rather than being silently
re-litigated in every feature discussion: should instances federate with each other, and should there be a
mobile client.

## Decision
**Federation is explicitly out of scope.** Each instance is an island, like real Discord, not Matrix/
ActivityPub — no cross-instance guilds, no cross-instance identity, no inter-instance protocol. This is a
deliberate non-goal, not a silent gap. Consequently there is no cross-instance hierarchy to design (ADR
0008): guilds are pure independent peers within one instance, and instances are pure independent peers with
zero relationship to each other (see ADR 0007's two-deployment commercial model).

**Mobile clients are out of scope for v1**, with no dedicated client planned. Since the token-auth model
(ADR 0011) is already device/OS-keychain-based rather than assuming a browser, no extra seam work is needed
beyond making sure REST/gateway contracts don't quietly assume a desktop-only client (the same ongoing
browser-constraint check ADR 0011 already requires for the web SPA). This is revisited only if the project
grows enough to justify it.

**No "Platform Operator" tier exists** above Instance Admin. The flagship instance's own admins use the
exact same Instance Admin tier (ADR 0013) any self-hoster gets — the flagship just carries more legal
exposure as a real public service, making Instance Admin's proactive-intervention capability more
operationally relevant there, not structurally different.

## Consequences
- No federation protocol, no cross-instance identity system, no cross-instance moderation coordination needs
  to be designed, ever, under this decision — a real scope reduction that keeps the whole platform's
  authority model (ADR 0008) simple.
- A mobile client, if built later, slots into the existing token-auth model without an auth redesign — the
  seam already exists even though the client doesn't.
- Because there's no Platform Operator tier, "the flagship instance" is architecturally just another instance
  in every code path — no special-cased backend logic exists anywhere for "am I the flagship."

## Alternatives considered
- **ActivityPub-style federation**: rejected — a materially different, much larger architecture (identity,
  moderation, and content all become cross-instance concerns) that doesn't serve the project's actual goal
  (a good self-hosted Discord alternative, not a federated network).
- **A dedicated mobile client now**: rejected — real scope (a fourth client, a new platform's UI paradigms)
  with no clear v1 demand signal; the CLI/GUI/web priority order already reflects where the real leverage is.
