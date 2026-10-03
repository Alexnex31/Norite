-- Guild invites (Milestone M20a).
--
-- "Live" means not expired and not used up, and every statement that answers for a code asks both. The
-- redemption that spends an invite's last use deletes it and the sweep removes expired ones, but neither
-- is what makes a dead code answer as an unknown one: a deletion that fails or a sweep that has not run
-- must not turn tidiness into a disclosure.

-- name: CreateGuildInvite :one
INSERT INTO invites (id, code, guild_id, channel_id, inviter_id, max_uses, expires_at)
VALUES ($1, $2, $3, $4, $5, sqlc.narg(max_uses)::integer, sqlc.narg(expires_at)::timestamptz)
RETURNING *;

-- How many live invites a guild has, for the creation ceiling. invites_guild_id_idx serves it.
--
-- name: CountLiveGuildInvites :one
SELECT count(*) FROM invites
WHERE guild_id = $1
  AND (expires_at IS NULL OR expires_at > now())
  AND (max_uses IS NULL OR uses < max_uses);

-- A guild's live invites with each inviter's name, newest first. Unpaginated, like the channel and role
-- lists, and bounded the same way: at creation, by [limits].invites_per_guild. The inviter is joined by
-- primary key and dropped for a deleted account, as a message's author is (M20a).
--
-- name: ListGuildInvites :many
SELECT sqlc.embed(i), u.username AS inviter_username, u.display_name AS inviter_display_name
FROM invites i
LEFT JOIN users u ON u.id = i.inviter_id AND u.deleted_at IS NULL
WHERE i.guild_id = $1
  AND (i.expires_at IS NULL OR i.expires_at > now())
  AND (i.max_uses IS NULL OR i.uses < i.max_uses)
ORDER BY i.id DESC;

-- A live invite by its code: what revoking reads before deciding who may.
--
-- name: GetLiveGuildInvite :one
SELECT * FROM invites
WHERE code = $1
  AND (expires_at IS NULL OR expires_at > now())
  AND (max_uses IS NULL OR uses < max_uses);

-- What a preview shows: where a live code leads, and who issued it.
--
-- Every join is inner except the inviter's, so a code whose guild or channel is mid-deletion answers as an
-- unknown one. Nothing here is gated on the caller beyond being signed in, deliberately: an invite is the
-- guild's own decision to show itself to whoever holds the code, and the preview is how the person given
-- it decides whether to use it.
--
-- name: GetGuildInvitePreview :one
SELECT
  i.code,
  i.expires_at,
  g.id          AS guild_id,
  g.name        AS guild_name,
  g.description AS guild_description,
  g.icon_hash   AS guild_icon_hash,
  c.id          AS channel_id,
  c.name        AS channel_name,
  i.inviter_id,
  u.username     AS inviter_username,
  u.display_name AS inviter_display_name
FROM invites i
JOIN guilds g   ON g.id = i.guild_id
JOIN channels c ON c.id = i.channel_id
LEFT JOIN users u ON u.id = i.inviter_id AND u.deleted_at IS NULL
WHERE i.code = $1
  AND (i.expires_at IS NULL OR i.expires_at > now())
  AND (i.max_uses IS NULL OR i.uses < i.max_uses);

-- Spend one use of a live code, M10's RedeemInstanceInvite shape.
--
-- Every guard is in the WHERE, so concurrent redemptions are decided by the row lock this UPDATE takes and
-- the re-check READ COMMITTED does under it: exactly max_uses of them match. Written as a read and then an
-- update, four of four of M10's concurrent racers got in. `max_uses IS NULL` is its own branch because
-- `uses < NULL` is NULL, which would make an unlimited invite match nothing.
--
-- name: RedeemGuildInvite :one
UPDATE invites SET uses = uses + 1
WHERE code = $1
  AND (expires_at IS NULL OR expires_at > now())
  AND (max_uses IS NULL OR uses < max_uses)
RETURNING *;

-- name: DeleteGuildInvite :execrows
DELETE FROM invites WHERE id = $1;

-- How many guilds an account is in, owned ones included, for the joined ceiling (M20a). Counted under
-- LockAccountOwnership; guild_members_user_id_idx serves it.
--
-- name: CountGuildsJoinedBy :one
SELECT count(*) FROM guild_members WHERE user_id = $1;

-- The sweep (auth.SweepExpired), served by invites_expires_at_idx.
--
-- name: DeleteExpiredGuildInvites :execrows
DELETE FROM invites WHERE expires_at < now();
