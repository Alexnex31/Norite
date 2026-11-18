-- name: InsertVoiceState :one
-- A connection the SFU has answered, as the channel is told about it.
INSERT INTO voice_states (
  connection_id, guild_id, channel_id, user_id, session_id, device_id, mute, deaf, suppress, supports_video
) VALUES (
  $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
)
RETURNING *;

-- name: DeleteVoiceState :one
-- A connection's end. Returning the row, so the caller can tell the channel it was in, and no row when
-- something else ended it first: a cascade, a replacement, or another end racing this one.
DELETE FROM voice_states WHERE connection_id = $1 RETURNING *;

-- name: GetVoiceState :one
SELECT * FROM voice_states WHERE connection_id = $1;

-- name: ListVoiceStatesInChannel :many
-- A channel's participants, oldest first. Bounded by the SFU's participants per room, which is what a
-- row stands for.
SELECT * FROM voice_states WHERE channel_id = $1 ORDER BY connection_id;

-- name: ListVoiceStatesInGuild :many
-- The guild's connections, for the voice re-check. Bounded by its voice channels times the SFU's
-- participants per room.
SELECT * FROM voice_states WHERE guild_id = $1 ORDER BY connection_id;

-- name: ListVoiceStatesOfDevice :many
-- The connections an account holds from one device, which a join from that device replaces.
SELECT * FROM voice_states WHERE user_id = $1 AND device_id = $2;

-- name: CountVoiceChannelUsers :one
-- How many accounts other than user_id are in a channel, for user_limit: an account on two connections is
-- one person, and one already there takes no new place.
SELECT count(DISTINCT user_id) FROM voice_states WHERE channel_id = $1 AND user_id <> $2;

-- name: LockVoiceChannel :exec
-- Serializes the joins of one channel for the calling transaction, so two joins cannot both read one
-- place left under user_limit and both take it.
--
-- Slot 5 of the advisory namespace ("NOR" plus a slot, as slots 3 and 4 are), keyed per channel. Two
-- channels whose low 31 bits collide serialize unnecessarily and stay correct.
SELECT pg_advisory_xact_lock(1313033477, (sqlc.arg(channel_id)::bigint & 2147483647)::int);

-- name: UpdateVoiceStateRights :one
-- The rights the re-check found, when they changed.
UPDATE voice_states SET mute = $2, deaf = $3, suppress = $4 WHERE connection_id = $1 RETURNING *;

-- name: DeleteAllVoiceStates :exec
-- At start, on a single process: every call it held ended with the process that held it, and no SFU
-- survives a restart. Phase P's flagship, with several replicas, sweeps per replica instead.
DELETE FROM voice_states;

-- name: GetMembersVoiceFlags :many
-- The server mute and deafen a moderator set, for the accounts given; an account that is not a member is
-- absent.
--
-- Driven from the account list, one probe of guild_members' primary key each, for ListMemberRolesForUsers'
-- reason: `guild_id = $1 AND user_id = ANY($2)` has a cached generic plan that scans the whole guild's
-- members to keep a few (M13a).
SELECT gm.user_id, gm.mute, gm.deaf
FROM unnest(sqlc.arg(user_ids)::bigint[]) AS u(user_id)
JOIN guild_members gm ON gm.guild_id = sqlc.arg(guild_id)::bigint AND gm.user_id = u.user_id;
