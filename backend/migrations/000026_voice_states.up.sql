-- Milestone M33 — who is in a voice channel.
--
-- One row per voice connection, which is what the SFU admits: ADR 0023 as amended at M26 lets an account
-- hold two (a second machine; M105's separate video connection). §2 drew this table keyed by (guild_id,
-- user_id), one state per account per guild as Discord has it, which contradicts that; this migration is
-- the table's authority now, and §2 says so.
--
-- A row is written once the SFU has answered the connection's offer, and deleted at every end of the call.
-- What a row records is the state the gateway has told the channel about, so a client connecting later
-- reads the same thing the others were sent.

CREATE TABLE voice_states (
  -- The connection id the backend minted for Provision, and the one VOICE_STATE_DELETE names.
  connection_id  bigint PRIMARY KEY,
  guild_id       bigint NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
  -- A deleted channel takes its rows; the SFU's room is closed by the voice re-check that the deletion
  -- publishes.
  channel_id     bigint NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  user_id        bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  -- The gateway session the call belongs to, and its device: a session dropped ends its call, and a join
  -- from the same device replaces the call an older session of it left behind (M33's D3).
  session_id     text NOT NULL,
  device_id      text NOT NULL,
  -- The rights the gateway last told the channel about, kept current by the voice re-check: the server
  -- mute and deafen a moderator set (guild_members), and suppress, the caller lacking PermSpeakVoice. The
  -- client applies them (M33's D6); the SFU enforces none of the three.
  mute           boolean NOT NULL DEFAULT false,
  deaf           boolean NOT NULL DEFAULT false,
  suppress       boolean NOT NULL DEFAULT false,
  -- What the client said it can do. Nothing reads it until video (M105); terminal clients send false.
  supports_video boolean NOT NULL DEFAULT false,
  created_at     timestamptz NOT NULL DEFAULT now()
);

-- The channel's participants: the REST listing, and user_limit's count when a join is authorized.
CREATE INDEX voice_states_channel_id_idx ON voice_states (channel_id);
-- The foreign keys' other two, which a deletion of a guild or an account would otherwise scan for (M11's
-- lesson), and the guild one is also what the re-check reads.
CREATE INDEX voice_states_guild_id_idx ON voice_states (guild_id);
-- An account's calls, by device: a join replaces its device's older call, and an account deletion cascades.
CREATE INDEX voice_states_user_id_device_id_idx ON voice_states (user_id, device_id);
