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

-- name: CountGuildsOwnedBy :one
-- How many guilds an account owns, for the creation cap.
--
-- Served by guilds_owner_id_idx, which 000015 added for the account-deletion FK check and which answers
-- this for free.
SELECT count(*) FROM guilds WHERE owner_id = $1;

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

-- name: LockGuildRolePositions :exec
-- Serializes role creation within one guild, for the whole of the calling transaction.
--
-- NextRolePosition below is read and then acted on, which under READ COMMITTED — Postgres's default and
-- this pool's, since RunInTx sets no isolation level — is a read-modify-write with a gap in it. Two
-- concurrent creates both read the same max, neither sees the other's uncommitted INSERT, and both land on
-- the same position. Migration 000015 deliberately declines a unique constraint on (guild_id, position),
-- because reordering is a multi-row swap, so nothing downstream catches it.
--
-- The consequence is not cosmetic: position is the hierarchy M13 enforces over, and two roles at the same
-- position are neither above nor below each other. NextRolePosition's own comment calls a collision "a
-- correctness bug rather than a style preference" — that comment was written about the count-versus-max
-- cause and this is the other one.
--
-- An advisory lock rather than row locking, and the two-argument form so it lives in its own namespace:
-- the first key is "NOR" plus a slot number (slot 1 is the migration lock, slot 2 the instance bootstrap,
-- this is slot 3), the second is the guild. Two guilds whose low 31 bits collide serialize against each
-- other unnecessarily and stay correct, which is the right direction for an operation that happens a
-- handful of times in a guild's life.
SELECT pg_advisory_xact_lock(1313033475, (sqlc.arg(guild_id)::bigint & 2147483647)::int);

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

--
-- Role assignment, role positions and permission overwrites (Milestone M13) follow. This separator is a
-- lone comment line rather than prose, because sqlc attaches any comment block immediately above a query
-- to that query's godoc — a section heading here becomes AssignRoleToMember's first documented sentence,
-- and the Querier interface's.
--

-- name: AssignRoleToMember :execrows
-- Give a member a role.
--
-- # Why there is no ON CONFLICT, and why the row comes from a SELECT
--
-- Deliberately conflicting rather than swallowing, for AddGuildMember's reason one table over: ON CONFLICT
-- DO NOTHING would make a second assignment return zero rows, which is indistinguishable from an insert
-- that matched nothing. Those two need different answers — the first is an idempotent PUT succeeding, the
-- second is a 404 — so the unique violation is left to surface and the caller translates it.
--
-- The SELECT is what verifies both ids belong together before anything is written: the role must be in
-- this guild, and the (guild, user) pair must be a real membership. Rule 1 in the statement rather than in
-- a check a handler has to remember. The composite foreign key would refuse a non-member anyway, but it
-- would do it as a constraint violation the caller has to reverse-engineer, and a 500 is not an answer.
--
-- # Why NOT is_default
--
-- @everyone is held by every member by virtue of membership and is never stored here — ListGuildMemberAuthority
-- reads it through `is_default OR EXISTS(...)` precisely so that it does not have to be, and
-- GetMemberHighestRolePosition's explanation rests on the same premise. Without this predicate the premise
-- is merely a convention, and granting @everyone explicitly writes a row that permission resolution
-- ignores, that the member's role list reports and nobody else's does, and that DeleteRole cannot remove
-- because it refuses to delete the default role at all. Every other role-mutating statement here carries
-- the same guard; this one was the exception until a review found it.
INSERT INTO guild_member_roles (guild_id, user_id, role_id)
SELECT gm.guild_id, gm.user_id, r.id
FROM guild_members gm
JOIN roles r ON r.guild_id = gm.guild_id
WHERE gm.guild_id = sqlc.arg(guild_id)
  AND gm.user_id = sqlc.arg(user_id)
  AND r.id = sqlc.arg(role_id)
  AND NOT r.is_default;

-- name: UnassignRoleFromMember :execrows
-- Take a role away. Zero rows means the member did not hold it, which is an idempotent DELETE succeeding
-- rather than an error — the caller has already established that the member exists, because the hierarchy
-- check reads their standing first and GetMemberHighestRolePosition returns no row for a non-member.
--
-- guild_id in the WHERE is not redundant with role_id: it scopes the delete to this guild's grant even
-- though the pair could only ever appear together, which is the same belt-and-braces GetRole applies.
DELETE FROM guild_member_roles
WHERE guild_id = $1 AND user_id = $2 AND role_id = $3;

-- name: ShiftRolePositionsUp :exec
-- Make room at the bottom of the hierarchy for a new role (Milestone M13).
--
-- A new role is created immediately above @everyone rather than above every existing role, which is what
-- Discord does and what stops a non-owner creating a role they then cannot edit, delete, assign or
-- reposition. That means everything else moves up one, and @everyone stays on the floor every layer-4
-- resolution starts from.
--
-- # Renumbering rather than shifting, so positions stay bounded by the ceiling
--
-- The obvious implementation is `position = position + 1`, and it makes positions grow with the guild's
-- *lifetime* creation count rather than with the roles it currently has. A guild that keeps one long-lived
-- role and churns ten thousand others leaves that role near position 10,000 against a ceiling of 250 — so
-- any service-side bound derived from the ceiling would reject a legitimate reorder of the guild's own
-- current hierarchy, and nothing anywhere would bring the numbers back down.
--
-- Renumbering from row_number() closes it: after every create, the live non-default roles occupy 2..N+1
-- with position 1 free for the new one, so no position ever exceeds the role ceiling plus one. Relative
-- order is preserved, which is the only property the hierarchy depends on — `ORDER BY position, id`
-- matches ListGuildRoles, so two roles that tie today keep the order the listing already shows.
--
-- At most 250 rows on a path that runs when somebody clicks "create role". It runs inside the same
-- advisory lock CreateRole already takes, because two concurrent creates that both renumber and both
-- insert at 1 would collide — and migration 000015 deliberately declines the unique constraint that would
-- catch it.
UPDATE roles r
SET position = ranked.position, updated_at = now()
FROM (
    SELECT inner_r.id, (row_number() OVER (ORDER BY inner_r.position, inner_r.id) + 1)::integer AS position
    FROM roles inner_r
    WHERE inner_r.guild_id = sqlc.arg(guild_id) AND NOT inner_r.is_default
) ranked
WHERE r.id = ranked.id AND r.position IS DISTINCT FROM ranked.position;

-- name: SetRolePosition :execrows
-- One row of a reorder. The whole reorder is several of these in one transaction under the same advisory
-- lock, because N separate requests would leave two roles sharing a position between them — and two roles
-- at one position are neither above nor below each other, which dissolves the ordering every hierarchy
-- check rests on.
--
-- Two guards, and both are in the statement rather than in a check before it — the same discipline
-- DeleteRole applies to the same role.
--
-- `NOT is_default` refuses to move @everyone off 0. The position bound is the other half and is the one
-- that is easy to leave out, because @everyone staying at 0 sounds like it already implies the floor is
-- reserved. It does not: nothing stopped a *real* role being moved onto 0, or to a negative position below
-- it. Both are corruptions rather than curiosities. A role sharing @everyone's position is neither above
-- nor below it, which dissolves the strictly-greater comparison the whole hierarchy rests on; and
-- GetMemberHighestRolePosition coalesces an absent maximum to 0, so a member whose only role sits at 0
-- becomes indistinguishable from a member holding no roles at all — losing exactly the protection the
-- position was granted to give them. A client sending zero-based positions is the ordinary way in, since
-- role lists render 0-indexed.
--
-- The *upper* bound lives in the service rather than here, because it derives from the configurable role
-- ceiling: positions only ever grow through role creation, which shifts every role up by one, and the
-- bound exists so that shift can never overflow `position`'s integer. A magic number in this file would be
-- a policy constant in the wrong place, and the ceilings it follows from are already config.
UPDATE roles
SET position = sqlc.arg(position), updated_at = now()
WHERE id = sqlc.arg(id)
  AND guild_id = sqlc.arg(guild_id)
  AND NOT is_default
  AND sqlc.arg(position)::integer > 0;

-- name: GetPermissionOverwrite :one
-- One overwrite, scoped to the guild through its channel.
--
-- Read before every write and before every delete, because the escalation check covers the union of what
-- the request changes: the bits the existing row allows or denies, or'd with the bits the new one does.
-- Checking only the value being written leaves deletion checked by nothing at all — and deleting an
-- overwrite that denies you something grants you that thing, which is a self-escalation on the endpoint
-- whose whole subject is per-channel permissions.
SELECT po.channel_id, po.target_type, po.target_id, po.allow, po.deny
FROM permission_overwrites po
JOIN channels c ON c.id = po.channel_id
WHERE po.channel_id = sqlc.arg(channel_id)
  AND po.target_type = sqlc.arg(target_type)
  AND po.target_id = sqlc.arg(target_id)
  AND c.guild_id = sqlc.arg(guild_id)::bigint;

-- name: UpsertPermissionOverwrite :one
-- Write an overwrite, creating or replacing.
--
-- PUT rather than POST/PATCH on the wire, so the same statement has to serve both — ON CONFLICT on the
-- primary key is what makes the endpoint idempotent. The row is selected from channels rather than
-- supplied directly, so a channel in another guild inserts nothing and the caller sees pgx.ErrNoRows
-- rather than writing an overwrite onto somebody else's channel (rule 1, in the statement).
INSERT INTO permission_overwrites (channel_id, target_type, target_id, allow, deny)
SELECT c.id, sqlc.arg(target_type), sqlc.arg(target_id), sqlc.arg(allow), sqlc.arg(deny)
FROM channels c
WHERE c.id = sqlc.arg(channel_id) AND c.guild_id = sqlc.arg(guild_id)::bigint
ON CONFLICT (channel_id, target_type, target_id)
DO UPDATE SET allow = EXCLUDED.allow, deny = EXCLUDED.deny
RETURNING channel_id, target_type, target_id, allow, deny;

-- name: DeletePermissionOverwrite :execrows
-- Remove an overwrite. Scoped through channels for UpsertPermissionOverwrite's reason.
DELETE FROM permission_overwrites po
USING channels c
WHERE po.channel_id = c.id
  AND c.guild_id = sqlc.arg(guild_id)::bigint
  AND po.channel_id = sqlc.arg(channel_id)
  AND po.target_type = sqlc.arg(target_type)
  AND po.target_id = sqlc.arg(target_id);

-- name: CopyChannelOverwrites :exec
-- Copy a category's overwrites onto a channel being created under it (Milestone M13).
--
-- Inheritance is a copy at creation and not a lookup at resolution time, which is what Discord does: a
-- channel does not follow its category's later permission changes, and "sync permissions with category" is
-- a client re-copying through the overwrite endpoints rather than a flag anything stores. roles.Resolve
-- therefore keeps reading exactly one channel's rows and never walks to a parent — resolution-time
-- inheritance would put a second query on the check that runs before every mutation.
--
-- Without this, a channel created inside a locked-down category is readable by everyone the moment it
-- exists. That was unreachable while nothing could write an overwrite, which is why M12 does not do it.
--
-- Not escalation-checked, deliberately: the copy replicates a configuration the guild already authored
-- rather than authoring a new one, so refusing bits the creator does not hold would make inheritance fail
-- exactly under the locked-down category it is most wanted under. What is checked is the creator's
-- authority over the parent, in CreateChannel.
--
-- # Both channels are scoped, not just the source
--
-- The first draft constrained `guild_id` only on the join to the source channel and took the destination
-- as a bare parameter, so it would happily copy one guild's overwrites onto another guild's channel —
-- reproduced, four rows crossing. Today's only caller passes an id it minted a few lines earlier, which is
-- what made it survive review of the statement in isolation; the sibling UpsertPermissionOverwrite claims
-- exactly this property for itself in its own comment, and a rule that holds because of who happens to
-- call it is not the rule that comment describes.
--
-- It would also not have stayed harmless. target_id for a member overwrite is a global user id rather than
-- a guild-scoped one, so a copied member row genuinely applies in the guild it landed in — applyOverwrites
-- matches on user id — and DeleteOverwritesForTarget is guild-scoped, so nothing would ever clean it up.
INSERT INTO permission_overwrites (channel_id, target_type, target_id, allow, deny)
SELECT dest.id, po.target_type, po.target_id, po.allow, po.deny
FROM permission_overwrites po
JOIN channels src ON src.id = po.channel_id
JOIN channels dest ON dest.id = sqlc.arg(channel_id)
WHERE po.channel_id = sqlc.arg(source_channel_id)
  AND src.guild_id = sqlc.arg(guild_id)::bigint
  AND dest.guild_id = sqlc.arg(guild_id)::bigint
ON CONFLICT DO NOTHING;

-- name: CountChannelPermissionOverwrites :one
-- How many overwrites a channel carries, for the creation ceiling.
--
-- The ceiling exists for the reason CLAUDE.md settles for channels and roles: a list that cannot be
-- paginated is bounded at creation instead. Overwrites are read whole — once per channel before every
-- channel-scoped mutation, and once per guild on the channel listing — so an unbounded count is work on
-- two hot paths, done on behalf of rows a caller wrote and nobody can see. target_id is polymorphic and
-- cannot be a foreign key, so a fabricated one persists with nothing to clean it up.
--
-- Served by the primary key's leading column.
SELECT count(*) FROM permission_overwrites WHERE channel_id = $1;
