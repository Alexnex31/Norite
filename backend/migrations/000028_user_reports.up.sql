-- Milestone M34 — reporting a participant of a voice call.
--
-- target_type 3, user, has been reserved since 000021 and is accepted now, from a voice channel. A user
-- belongs to no one guild, so a report against one names the channel it was filed from, which is how it
-- reaches that guild's moderators: guild_id is denormalized from it at filing, as it is from a message's
-- channel. NULL for a message, whose channel is the message's own.
--
-- ON DELETE SET NULL rather than CASCADE: a deleted channel is not a reason to lose a moderation record,
-- which guild_id still routes. The index is what that SET NULL reads (M11's lesson on every foreign key),
-- partial since every message report holds NULL here.
ALTER TABLE reports ADD COLUMN channel_id bigint REFERENCES channels(id) ON DELETE SET NULL;
CREATE INDEX reports_channel_id_idx ON reports (channel_id) WHERE channel_id IS NOT NULL;

-- One open report per reporter per target **per guild**: the same person can be reported in two guilds,
-- to two sets of moderators, and the second is not a duplicate of the first. A message has one guild, so
-- for it this is the same guard as before. NULLS NOT DISTINCT keeps the guard for M74's instance-routed
-- reports, whose guild_id is NULL and which plain uniqueness would let repeat without bound.
DROP INDEX reports_open_target_per_reporter_idx;
CREATE UNIQUE INDEX reports_open_target_per_reporter_idx
  ON reports (reporter_id, target_type, target_id, guild_id) NULLS NOT DISTINCT WHERE status = 0;
