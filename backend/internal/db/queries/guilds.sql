-- Guild, channel, role and membership queries (Milestone M12).

-- name: CreateGuild :one
-- The guild row. Its @everyone role and the owner's membership are written in the same transaction — see
-- guilds.Service.Create, which is the only caller and does all three in one RunInTx.
INSERT INTO guilds (id, name, owner_id, description)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetGuild :one
SELECT * FROM guilds WHERE id = $1;

-- name: UpdateGuild :one
-- Partial update through COALESCE, so a caller sends only the fields it means to change.
--
-- The alternative — one statement per field, or a query builder — is what rule 3 forbids and what sqlc
-- exists to avoid. NULL means "leave alone" rather than "set to NULL", which is why description clears
-- through a separate flag: without it there would be no way to remove a description at all, since the
-- value that means "clear this" and the value that means "do not touch this" would be the same.
UPDATE guilds
SET name        = COALESCE(sqlc.narg(name), name),
    description = CASE WHEN sqlc.arg(clear_description)::boolean THEN NULL
                       ELSE COALESCE(sqlc.narg(description), description) END,
    updated_at  = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: DeleteGuild :execrows
-- Everything below a guild is ON DELETE CASCADE, so this one statement takes the channels, roles,
-- memberships, role grants, overwrites and audit entries with it.
--
-- :execrows rather than :exec because the handler needs to tell "deleted" from "was not there" — not to
-- report the difference, which would be the membership oracle authorize exists to close, but to avoid
-- answering 204 for a guild that never existed.
DELETE FROM guilds WHERE id = $1;

-- name: CreateRole :one
INSERT INTO roles (id, guild_id, name, color, permissions, position, hoist, mentionable, is_default)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: ListGuildRoles :many
-- Ordered by position, which the (guild_id, position) index serves.
SELECT * FROM roles WHERE guild_id = $1 ORDER BY position, id;

-- name: GetRole :one
-- Scoped by guild as well as by id, so a role id from another guild resolves to nothing rather than to
-- somebody else's role. Rule 1: never trust a client-supplied ID without verifying it belongs to the
-- actor's claimed context — enforced in the statement, not in a check a handler has to remember.
SELECT * FROM roles WHERE id = $1 AND guild_id = $2;

-- name: UpdateRole :one
-- Same COALESCE shape as UpdateGuild, and the same guild scoping as GetRole.
--
-- position is not updatable here. Reordering roles is a multi-row swap and role *hierarchy* — who may
-- manage whom — is M13's, so this milestone stores the column, orders by it, and does not let a single-row
-- update reshuffle it.
UPDATE roles
SET name        = COALESCE(sqlc.narg(name), name),
    color       = COALESCE(sqlc.narg(color), color),
    permissions = COALESCE(sqlc.narg(permissions), permissions),
    hoist       = COALESCE(sqlc.narg(hoist), hoist),
    mentionable = COALESCE(sqlc.narg(mentionable), mentionable),
    updated_at  = now()
WHERE id = sqlc.arg(id) AND guild_id = sqlc.arg(guild_id)
RETURNING *;

-- name: DeleteRole :execrows
-- The is_default guard is in the WHERE rather than in Go, so a concurrent request cannot slip between a
-- check and a delete. Same discipline as ConsumePasswordResetToken and RedeemInstanceInvite: a guard that
-- lives in the statement cannot be raced, and one that lives in a Go `if` can.
--
-- A guild without @everyone has no permission floor for resolution to start from, so this is not a policy
-- nicety — it is the invariant guild creation establishes in its transaction.
DELETE FROM roles WHERE id = $1 AND guild_id = $2 AND NOT is_default;

-- name: CountGuildChannels :one
-- How many channels a guild has, for the creation cap.
--
-- Served by the leading column of channels_guild_id_position_idx: a bitmap index scan into the heap, 15
-- buffers on a 50,000-channel instance, rather than a sequential scan of every channel on it. Not an
-- *index-only* scan — the plan visits the heap for visibility — which is worth saying because the obvious
-- shorthand for "an index serves this" is the wrong one here.
SELECT count(*) FROM channels WHERE guild_id = $1;

-- name: CountGuildRoles :one
-- How many roles a guild has, for the creation cap.
--
-- A count, not NextRolePosition's max(position)+1 — deleting a role leaves a gap, so the highest position
-- and the number of roles are different numbers and only one of them is the thing being capped.
--
-- Same access path as the channel count above: bitmap index scan on roles_guild_id_position_idx, 10
-- buffers on a 25,000-role instance.
SELECT count(*) FROM roles WHERE guild_id = $1;

-- name: NextRolePosition :one
-- The position a new role goes in: one above the highest that exists.
--
-- `max(position) + 1`, not `count(*)`, and the difference is a correctness bug rather than a style
-- preference. Deleting a role leaves a gap, so after removing the middle of @everyone(0)/mods(1)/admins(2)
-- the count is 2 and the next role would be created *at* position 2 — a tie with admins, which nothing
-- prevents since there is deliberately no unique constraint on (guild_id, position). Delete more and the
-- new role lands below existing ones, contradicting "positioned above every existing role" in the contract
-- and silently corrupting the ordering M13's hierarchy rules will be enforcing.
--
-- coalesce for the empty case, which cannot happen through the API — guild creation writes @everyone in
-- the same transaction — but which would otherwise make a NULL the caller has to handle.
--
-- Reads one value out of roles_guild_id_position_idx rather than the whole role list. The previous
-- implementation selected every column of every role in the guild to take len() of the slice.
SELECT coalesce(max(position), -1) + 1 FROM roles WHERE guild_id = $1;

-- name: DeleteOverwritesForTarget :exec
-- Removes the permission overwrites that named a role or a member, across the whole guild.
--
-- target_id is polymorphic — it names a role or a user depending on target_type — so it cannot be a
-- foreign key, and nothing cascades. Without this, deleting a role leaves its overwrites behind forever on
-- every channel that had one, and removing a member leaves theirs: rejoin the guild later and
-- roles.applyOverwrites matches the member tier again and silently reapplies a deny nobody can see in the
-- UI or explain from the audit log.
--
-- Scoped through channels to the guild, so this cannot reach another guild's rows even though target_id
-- alone would match them — a snowflake is unique in practice, but "in practice" is not the guarantee
-- rule 1 asks for.
DELETE FROM permission_overwrites po
USING channels c
WHERE po.channel_id = c.id
  AND c.guild_id = sqlc.arg(guild_id)::bigint
  AND po.target_type = sqlc.arg(target_type)
  AND po.target_id = sqlc.arg(target_id);

-- name: CreateChannel :one
INSERT INTO channels (id, guild_id, type, parent_id, name, topic, position, nsfw, bitrate, user_limit)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: ListGuildChannels :many
-- Columns enumerated rather than `SELECT *`, and topic_search is the reason.
--
-- It is a generated tsvector that nothing in this codebase reads until M63's channel search, and pgx
-- decodes it into a full pgtype.TSVector — a parsed lexeme list with positions and weights — on every row.
-- Confirmed by probe: `SELECT *` scans fine, so this is not a correctness fix. It is that the channel list
-- is one of rule 7's three named hot paths, and paying to transfer and parse a search index nobody reads,
-- once per channel per listing, is exactly the over-fetching §15 asks not to ship.
--
-- The three single-row channel queries keep `SELECT *`, deliberately. One row's tsvector costs nothing,
-- and enumerating there too would give sqlc four near-identical row structs where one db.Channel does —
-- four conversions to keep in step for no measurable gain. The cost this avoids is the one that multiplies
-- by the number of channels, and that is this query alone.
SELECT id, guild_id, type, parent_id, name, topic, position, nsfw, last_message_id,
       bitrate, user_limit, created_at, updated_at
FROM channels WHERE guild_id = $1 ORDER BY position, id;

-- name: GetChannel :one
-- By id alone, with no guild parameter, and that is deliberate rather than an oversight.
--
-- PATCH /channels/{id} and DELETE /channels/{id} carry no guild in their path, so there is no guild id to
-- scope by that did not come from the caller. The handler loads the channel, reads *its* guild_id, and
-- authorizes against that — which is the only ordering rule 1 permits, since scoping the read by a
-- caller-supplied guild would be trusting the value the check exists to verify.
SELECT * FROM channels WHERE id = $1;

-- name: UpdateChannel :one
-- Scoped by guild, because by the time this runs the handler has resolved the channel's own guild from
-- GetChannel and authorized against it. Passing it back in is a second assertion that the row being
-- written is the row that was checked.
UPDATE channels
SET name       = COALESCE(sqlc.narg(name), name),
    topic      = CASE WHEN sqlc.arg(clear_topic)::boolean THEN NULL
                      ELSE COALESCE(sqlc.narg(topic), topic) END,
    position   = COALESCE(sqlc.narg(position), position),
    nsfw       = COALESCE(sqlc.narg(nsfw), nsfw),
    bitrate    = COALESCE(sqlc.narg(bitrate), bitrate),
    user_limit = COALESCE(sqlc.narg(user_limit), user_limit),
    updated_at = now()
WHERE id = sqlc.arg(id) AND guild_id = sqlc.arg(guild_id)
RETURNING *;

-- name: DeleteChannel :execrows
DELETE FROM channels WHERE id = $1 AND guild_id = $2;

-- name: AddGuildMember :one
-- ON CONFLICT DO NOTHING would make a second join silently succeed and return no row, which the caller
-- cannot distinguish from a failed insert. Left to conflict instead, so the unique violation is the
-- answer.
INSERT INTO guild_members (guild_id, user_id, nickname)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetGuildMember :one
SELECT * FROM guild_members WHERE guild_id = $1 AND user_id = $2;

-- name: ListGuildMembers :many
-- Cursor pagination on user_id, never offset.
--
-- §2 specifies cursor-only everywhere, and the member list is one of rule 7's named hot paths. An OFFSET
-- makes page N cost N pages of scanning and shifts every row when somebody joins or leaves mid-read; a
-- cursor costs one index descent regardless of depth and is stable under concurrent writes. The primary
-- key (guild_id, user_id) serves it directly — measured as an Index Scan with no sort node on a
-- 15,000-member guild.
SELECT * FROM guild_members
WHERE guild_id = $1 AND user_id > $2
ORDER BY user_id
LIMIT $3;

-- name: UpdateGuildMember :one
UPDATE guild_members
SET nickname = CASE WHEN sqlc.arg(clear_nickname)::boolean THEN NULL
                    ELSE COALESCE(sqlc.narg(nickname), nickname) END,
    deaf     = COALESCE(sqlc.narg(deaf), deaf),
    mute     = COALESCE(sqlc.narg(mute), mute)
WHERE guild_id = sqlc.arg(guild_id) AND user_id = sqlc.arg(user_id)
RETURNING *;

-- name: RemoveGuildMember :execrows
DELETE FROM guild_members WHERE guild_id = $1 AND user_id = $2;

-- name: WriteAuditLogEntry :exec
-- Rule 2, and it takes a querier rather than a pool for the reason the rule states: the entry is written
-- in the same transaction as the mutation it records, so a mutation that commits without its entry is not
-- a state this schema can reach.
--
-- Nothing reads this until M14. `changes` is jsonb and M12 writes either NULL or a flat object of changed
-- fields; M14 owns the diffing that produces a richer one.
INSERT INTO audit_log_entries (id, guild_id, actor_id, action, target_id, changes)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListMemberRoleIDs :many
-- The role ids held by each of a set of members, in one query rather than one per member.
--
-- The member listing returns up to 100 members and each carries its roles, so the obvious shape — a query
-- per member inside the loop — is the N+1 §15.2 names as the canonical risk. `= ANY($2)` resolves the
-- whole page in one round trip, and the primary key's (guild_id, user_id) prefix serves it.
SELECT guild_id, user_id, role_id
FROM guild_member_roles
WHERE guild_id = $1 AND user_id = ANY(sqlc.arg(user_ids)::bigint[]);
