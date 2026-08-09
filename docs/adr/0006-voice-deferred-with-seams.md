# ADR 0006: Voice/video deferred to a later phase, with seams built now

## Status
**Superseded entirely** by [ADR 0012: Voice in v1](0012-voice-in-v1.md). This ADR's core decision — defer
all real media to a later, undetermined phase — is **reversed**, not merely refined: voice (audio) is real,
working v1 functionality on every client, including the CLI, built on a self-hosted custom Pion SFU and an
embedded TURN server. Only video/screen-share is still deferred-but-seamed, the same treatment this ADR
originally proposed for voice+video together. The historical record below is kept intact — it accurately
describes the reasoning and the seams as they stood before voice was activated; ADR 0012 is the current
source of truth for the actual v1 voice architecture.

## Context
Full Discord feature parity, including voice/video channels, is the long-term goal, but building a
WebRTC/SFU media layer alongside the initial text platform would roughly double the v1 scope and delay
everything else. We still don't want the eventual voice phase to require restructuring the schema,
permission model, gateway protocol, or frontend component boundaries.

## Decision
V1 ships text-only (guilds, channels, roles, messaging, DMs, presence, invites). The following seams are
built now, inert until a later milestone (M5 scaffolds them further; real media integration is explicitly
**M6+, out of the current plan's detailed scope**):
- `channels.type` enum already includes `GUILD_VOICE`/`GUILD_STAGE_VOICE`; `bitrate`/`user_limit` columns
  already exist on `channels`; `voice_states` table exists.
- Permission bitfield already defines `PermConnectVoice`, `PermSpeakVoice`, `PermVideoVoice`,
  `PermMuteMembers`, `PermDeafenMembers` — unused by any v1 handler, but the bitfield's shape (and anything
  serialized from it, like the REST/OpenAPI permission representation) never has to change later.
  See `docs/architecture.md` Section 2 "Permission system".
- Gateway op-code `4` (`Voice State Update`) and dispatch events `VOICE_STATE_UPDATE`/`VOICE_SERVER_UPDATE`
  are part of the protocol from day one; `internal/voice.MediaCoordinator` is a real Go interface with a
  stub implementation returning "not yet available." See `docs/architecture.md` Section 2 "Voice seam".
- Frontend: a `VoiceClient` interface with a `NoopVoiceClient` implementation; voice channels render in the
  channel list (disabled "coming soon", behind a `FEATURE_VOICE` flag); voice permission rows exist (hidden)
  in the role/permission UI; the Settings "Voice & Video" device-selection tab is built for real now since
  it needs no signaling. See `docs/architecture.md` Section 3 "Voice seam".

## Consequences
- When real voice is built, the gateway's job doesn't change: on `op 4`, call
  `MediaCoordinator.AllocateSession`, dispatch `VOICE_SERVER_UPDATE` with the result, and the client
  negotiates media directly with the media server — not through the text gateway. Only `internal/voice`'s
  internals and the frontend's `VoiceClient` implementation need to be written; no schema migration,
  permission-bitfield change, or gateway-protocol change should be needed at that point.
- The choice of actual media server (self-hosted Pion-based SFU vs. an external LiveKit process) is
  explicitly deferred to when that milestone is actually planned — building it now would be speculative.
- Risk: if a real requirement surfaces during M1–M5 that these seams didn't anticipate, update this ADR and
  `docs/architecture.md` rather than silently deviating.

## Alternatives considered
- **Build voice/video from the start**: rejected — roughly doubles v1 scope and delays shipping any usable
  text-chat product; the seams above are the cheaper way to protect the later investment.
- **Design nothing for voice now, revisit later**: rejected — retrofitting a channel-type enum, permission
  bitfield, and gateway protocol after they're already in production use is riskier and more disruptive
  than reserving the shape now at near-zero cost.
