# ADR 0008: Consolidated guild/channel authority hierarchy

## Status
Accepted

## Context
Growing v1 scope (Instance Admin, public matchmaking, whispers, friends, blocks, E2E) each introduce a
concept that *feels* like it could be a new authority tier, but most of them aren't — they're personal
labels, delivery filters, or privacy layers with no permission-granting effect. Without one consolidated
statement of what is and isn't part of the authority hierarchy, it's easy for a future feature to
accidentally treat one of these as more powerful than intended (e.g. gating an action on friendship, or
routing guild moderation through blocks).

## Decision
Six layers, top to bottom, resolved in this order:
1. **Instance Admin** — sits outside any guild's role hierarchy entirely, not resolved via `roles.Resolve`.
   Boolean/flag-based, multiple admins supported, last-admin-removal safety rail.
2. **Guild Owner** — one per guild, bypasses all permission checks within that guild.
3. **A guild role with `PermAdministrator`** — the same short-circuit, scoped to that one guild.
4. **Regular guild roles** — permission bits OR'd across all roles a member holds; role `position` governs
   who can manage whom.
5. **Permission overwrites** — `@everyone` → role overwrites → per-member overwrite, most specific wins.
6. **"Guild moderator" is not a separate concept** — it's any role holding `PermManageMessages`.

Parallel concepts that deliberately sit **outside** this hierarchy, never resolved through it: public
matchmaking's fixed platform-wide ruleset (no owner exists to grant a "channel moderator" role); whispers (a
message-visibility restriction, not an authority tier); friends (a personal, mutual, non-gating label —
DMing is already open regardless of friend status); blocks (a personal, unilateral, gateway-dispatch-layer
filter — the one concept here that touches guild-channel *content delivery* without ever touching guild
*authority*, membership, or permissions); E2E-encrypted DMs (a privacy layer, not authority — DMs were never
guild-scoped); and the confirmed absence of any guild-to-guild or instance-to-instance relationship (guilds
and instances are pure independent peers, like real Discord, not Matrix/ActivityPub).

## Consequences
- Any new feature that wants to gate an action must place itself explicitly in this list, not invent an
  ad hoc check — if it doesn't fit one of the six layers or one of the parallel concepts, that's a signal the
  feature needs a real design decision, not a quick permission check.
- Blocks reaching into guild-channel content delivery (server-side gateway-dispatch filtering, not
  client-side) is the one deliberate exception to "authority and delivery are separate" — documented here so
  it's never mistaken for scope creep into the permission engine itself.
- `roles.Resolve` never needs to know about Instance Admin, public matchmaking, whispers, friends, or blocks
  — those are all checked at a different layer (middleware, gateway fan-out, or a dedicated tier check).

## Alternatives considered
- **Model Instance Admin as a synthetic "super role" inside `roles.Resolve`**: rejected — it isn't scoped to
  a guild at all, and forcing it through guild-shaped resolution would require every guild to implicitly
  carry an Instance-Admin-holding role, adding complexity for no benefit over a direct, separate check.
- **Give public matchmaking channels a synthetic "auto-guild" with real roles**: rejected in the parent
  feature design (see ADR 0013) — there's no owner to grant a moderator role to, so a role system would be
  inert scaffolding.
