# ADR 0013: Public matchmaking, friends, blocks, Instance Admin, custom emoji, webhooks

## Status
Accepted

## Context
Several v1 features share one underlying need: a platform-wide authority tier and moderation system that
sits above any single guild, since public matchmaking channels and DM/whisper reports have no guild owner to
escalate to at all. Custom emoji and webhooks are grouped into this same ADR as closely-coupled v1-scope
community features, not because they depend on Instance Admin technically.

## Decision
**Public matchmaking**: a genuinely new guild-less top-level channel type (not a special auto-guild), shipped
as a voice-and-text pair, instance-level toggle defaulting ON. Moderation uses a fixed, platform-level
ruleset — no custom roles, no ownership, no "channel moderator" concept, since there's no owner to grant that
role. Anti-abuse: a minimum account-age/verification threshold on top of existing rate limiting (including
its global IPv6 `/64`-subnet grouping). A 7–30 day "recently met" list; a 48-hour post-empty message
retention window for report investigation, then permanent purge.

**Friends**: a real mutual request/accept system (`friend_requests`/`friendships`), purely an organizational
label — it grants no permission, since DMing is already open to anyone regardless of friend status.

**Blocks**: a full, server-enforced, unilateral restriction (`blocks` table). Reaches into shared guild
channels via server-side gateway-dispatch filtering (a cached per-connection block-set, invalidated
immediately on block/unblock) — never a client-side-only filter, both to avoid shipping a blocked author's
content over the wire at all and to prevent a modified client from ignoring the filter. A blocked account's
DM/whisper/mention attempt fails silently, with no distinguishable "you are blocked" signal.

**Instance Admin**: a boolean/flag-based tier, multiple admins supported, sitting entirely outside
`roles.Resolve` (see ADR 0008). Can issue full-account `instance_bans` (optional `expires_at`, enforced via
the general-purpose revoke-all-sessions primitive), review reports (guild-level and instance-level halves),
break-glass into a reported whisper (audit-logged), and proactively intervene without a filed report (with a
mandatory logged justification). A dedicated `instance_audit_log`, separate from per-guild
`audit_log_entries`. A last-admin-removal safety rail, and a server-side filesystem-gated lockout-recovery
CLI command.

**Reports**: one unified `reports` system serving every scope — guild-level routes to `PermManageMessages`
holders, everything without a guild owner (matchmaking, whispers, plain DM/Group DM) routes to Instance
Admins. Rate-limited per user; reporter history shown in triage; export asymmetry (filed-by-you included,
filed-against-you excluded) to protect reporters from retaliation.

**Custom emoji**: a `guild_emojis` table, `PermManageEmojis`, static-image-only for v1 with stricter
validation (resolution cap, format allow-list, decompression-bomb guard) than regular attachments, since
emoji render automatically and repeatedly for every viewer.

**Webhooks**: a `webhooks` table, `PermManageWebhooks`, `POST /webhooks/{id}/{token}`, high-entropy hashed
tokens, independent per-webhook rate limiting, content routed through the exact same rendering/sanitization
pipeline as any other message via the shared `messages.type` "sent via automation" value.

## Consequences
- Public-matchmaking voice abuse has no recorded evidence to review, by design (no call recording, ever —
  ADR 0012) — moderation there is necessarily corroborating-multi-report-based only.
- Blocking never touches guild membership/permissions/authority (ADR 0008) — a blocked account stays a fully
  normal guild member from everyone else's point of view.
- The account data export asymmetry pattern (your-own-action included, action-against-you excluded) recurs
  for both blocks and reports — documented once here so future similar features follow the same rule
  automatically rather than re-deriving it.
- The flagship instance's paid "custom emoji anywhere" perk (ADR 0007's `user_entitlements` seam) is a real
  upsell layered on top of a complete, free, per-guild base feature — never a paywall on the base feature.

## Alternatives considered
- **A lightweight "channel moderator" role for public matchmaking**: rejected — there's no owner to grant it
  from, and a platform-fixed ruleset plus Instance Admin escalation is simpler and matches "guild-less."
- **Client-side-only block filtering**: rejected — ships blocked content over the wire regardless (wasted
  bandwidth, and trivially bypassed by a modified client).
