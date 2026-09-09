-- Permission resolution queries (ADR 0008 layers 2 through 5).

-- name: ListGuildMemberAuthority :many
-- The guild, whether this account is in it, and every role whose permissions apply to them — in one round
-- trip rather than three.
--
-- Both queries here run before every mutating handler (rule 1) and neither result is cached: the cache
-- architecture.md describes is invalidated by a gateway dispatch, and the gateway is M18. A cache with
-- nothing to invalidate it is a demotion that takes effect five minutes late, so this runs live on every
-- check and has to be cheap. That is the constraint the shape below is chosen against.
--
-- The obvious decomposition is a guild lookup, then a membership lookup, then a role list, and it is three
-- network round trips on a path that runs before every mutation. This is the shape
-- GetActiveAPITokenByHash already established for the authentication path: resolve the whole context in
-- one indexed query rather than in a sequence of them.
--
-- The joins are both LEFT, and each one is load-bearing:
--
--   * on guild_members, because a non-member must be distinguishable from a missing guild. An INNER join
--     collapses both to zero rows, and the handler needs to tell "no such guild" from "not permitted" —
--     not to report the difference to the caller, which would be a membership oracle, but to know that a
--     guild exists at all before deciding what to write.
--   * on roles, because a member holding no roles at all still gets @everyone, and a guild whose rows are
--     mid-creation must not produce an empty result that reads as "not a member".
--
-- The role predicate is `is_default OR held`, so @everyone is included without the caller having to
-- remember it. That is ADR 0008 layer 4's floor: every member has it, it is never in guild_member_roles,
-- and a resolution that forgot it would silently deny permissions the guild grants to everyone.
--
-- is_default comes back because layer 5 needs it, not layer 4. An overwrite targeting @everyone is keyed
-- by that role's id like any other role overwrite, so applying the tiers in the ADR's order means knowing
-- which of the returned roles is the default one. Deriving it any other way — a second query, or assuming
-- the lowest position — is a lookup this query has already paid for.
--
-- position is M13's addition and it is the whole of the actor's half of the hierarchy check. Standing is
-- the highest position among the roles a member holds, and this query already returns exactly those roles
-- — so the alternative was a second query on every kick, mute, deafen, assignment and role edit, to fetch
-- a number that was one column away. M12's authorize.go argued against widening roles.Resolve's return for
-- owner_id, on the grounds that it would reshape a security-critical signature to save a lookup on two
-- cold paths. That argument does not carry here: standing is layer 4, the same layer this query already
-- serves, and the saving is on every hierarchy-checked mutation rather than on two.
--
-- owner_id and is_member repeat on every row. That is two columns times a handful of roles, and the
-- alternative is the extra round trip this query exists to avoid.
SELECT
    g.owner_id,
    (gm.user_id IS NOT NULL)::boolean AS is_member,
    r.id AS role_id,
    r.permissions AS role_permissions,
    r.position AS role_position,
    r.is_default AS role_is_default
FROM guilds g
LEFT JOIN guild_members gm
    ON gm.guild_id = g.id AND gm.user_id = $2
LEFT JOIN roles r
    ON r.guild_id = g.id
   AND (
       r.is_default
       OR EXISTS (
           SELECT 1
           FROM guild_member_roles gmr
           WHERE gmr.guild_id = g.id
             AND gmr.user_id = $2
             AND gmr.role_id = r.id
       )
   )
WHERE g.id = $1;

-- name: ListChannelPermissionOverwrites :many
-- Every overwrite on one channel: ADR 0008 layer 5, in one lookup, ordered in Go by the precedence the ADR
-- fixes rather than by SQL.
--
-- Sorting here is tempting and wrong. The precedence is @everyone, then the union of the member's role
-- overwrites, then the member's own — and the middle tier is an accumulation across roles rather than an
-- ordering among them, because two roles' overwrites are OR'd together and neither wins. An ORDER BY
-- would express a ranking that does not exist and invite a reader to apply the rows in sequence.
--
-- The join to channels is not decoration. It scopes the read to overwrites on a channel that actually
-- belongs to the guild being resolved, so an overwrite from another guild's channel cannot be applied even
-- if a caller passes a mismatched pair — rule 1's "never trust a client-supplied ID without verifying it
-- belongs to the actor's claimed context", enforced in the statement rather than in a check somebody has
-- to remember to write. The handler checks it too; this makes the resolution safe on its own.
--
-- channels.guild_id is nullable in the schema, because a DM belongs to no guild — so without the explicit
-- ::bigint the generated parameter is a *int64 and every caller has to take the address of a value it
-- knows is never nil. The cast keeps the Go signature honest about what this query actually accepts.
--
-- The primary key (channel_id, target_type, target_id) serves the lookup on its leading column: measured
-- at 0.092 ms and 6 buffers, Index Scan.
-- channel_id is selected although this query already knows it, so that both overwrite reads return the
-- same generated struct. Resolution.InChannel consumes either — the guild-wide read feeds the channel
-- listing and this one feeds a single-channel resolve — and two structurally identical row types would
-- have meant a field-by-field conversion loop written at whichever call site was built second, on the hot
-- path the guild-wide query's own plan exists to keep cheap.
SELECT po.channel_id, po.target_type, po.target_id, po.allow, po.deny
FROM permission_overwrites po
JOIN channels c ON c.id = po.channel_id
WHERE po.channel_id = sqlc.arg(channel_id) AND c.guild_id = sqlc.arg(guild_id)::bigint;

-- name: GetMemberHighestRolePosition :one
-- The target's standing: the highest position among the roles one member holds (Milestone M13).
--
-- The actor's standing comes free from ListGuildMemberAuthority above. This is the other side of every
-- hierarchy check — the person being kicked, muted, renamed, or given a role — and it is a separate read
-- because the target is not the account whose permissions were just resolved.
--
-- # Why the join to guild_members, and why GROUP BY
--
-- Both exist to keep two different answers from collapsing into the same number, which is the trap this
-- query is shaped around.
--
-- @everyone is never stored in guild_member_roles, so a member holding no other role has zero rows there
-- and MAX(position) over them is NULL. Coalescing that to 0 is correct — 0 is @everyone's position and the
-- floor is where such a member belongs. But an aggregate with no GROUP BY returns exactly one row even
-- over no input at all, so somebody who is not in the guild would also coalesce to 0: a non-member
-- evaluating as a member at the floor, which every actor above the floor then outranks. The mutation
-- itself would fail further down on a row count, so the result is safe by accident rather than by design.
--
-- The join makes membership the thing that produces a row and GROUP BY makes "no member" produce no rows,
-- so the caller gets pgx.ErrNoRows for a non-member and a real 0 for a member at the floor.
-- GREATEST as well as COALESCE, and the two cover different holes. COALESCE handles a member with no
-- rows here at all, whose maximum is NULL and whose standing is @everyone's 0. GREATEST handles a role
-- sitting at or below 0, which nothing in the schema prevents: roles.position carries no CHECK, and
-- SetRolePosition's lower bound only guards the reorder path, so a direct insert or a future import can
-- still place one there.
--
-- Without it the two halves of every hierarchy check disagree at exactly that boundary. Resolve computes
-- the actor's standing by taking the maximum over a floor of zero, so it reads such a member as standing
-- at 0; this query would read the same member as standing at -5. One comparison, two readers, two answers
-- — and the strictly-greater rule then gives a different verdict depending on which side of it the member
-- is on.
SELECT GREATEST(COALESCE(MAX(r.position), 0), 0)::integer AS highest_position
FROM guild_members gm
LEFT JOIN guild_member_roles gmr
    ON gmr.guild_id = gm.guild_id AND gmr.user_id = gm.user_id
-- The guild predicate on this join is redundant against every writer that exists — AssignRoleToMember
-- scopes the role to the guild and is the only one — and it is here anyway, for the reason
-- CopyChannelOverwrites and ListGuildPermissionOverwrites carry theirs: a rule that holds because of who
-- happens to call it is not the rule. guild_member_roles.role_id references roles(id) alone, only the
-- (guild_id, user_id) foreign key is composite, so nothing in the schema refuses a row naming another
-- guild's role — and this query would then report that role's position as the member's standing here.
-- ListGuildMemberAuthority scopes the actor's side the same way, and the two halves of one comparison
-- reading different rule sets is exactly what this query's comment above exists to prevent.
LEFT JOIN roles r ON r.id = gmr.role_id AND r.guild_id = gm.guild_id
WHERE gm.guild_id = $1 AND gm.user_id = $2
GROUP BY gm.guild_id, gm.user_id;

-- name: ListGuildPermissionOverwrites :many
-- Every overwrite on a set of channels, for the channel listing's per-channel view filter (Milestone M13).
--
-- # This must not be written as a join on guild_id, and that is measured rather than argued
--
-- The obvious shape — JOIN channels c ON c.id = po.channel_id WHERE c.guild_id = $1 — is fine on a small
-- guild and falls off a cliff at the channel ceiling, because the planner stops choosing the nested loop
-- and sequentially scans the whole overwrite table. On PostgreSQL 16 against 175,000 overwrite rows:
--
--                                            time      buffers
--   guild-join form, 10-channel guild      0.319 ms         75   Nested Loop, pkey lookups
--   guild-join form, 500-channel guild    13.682 ms      1,523   Seq Scan, 176,500 rows for 1,500 back
--   this form, 500-channel guild           0.536 ms      1,523   Bitmap Index Scan on pkey
--
-- The last figure is the query as it stands, with the guild join a security review added afterwards. The
-- ids alone measured 0.388 ms; scoping costs ~0.15 ms and a hash join over the guild's channels, and is
-- kept for the reason that review gives. Re-measured rather than left quoting the pre-scoping number,
-- because a comment citing a figure the code no longer produces is worse than one citing none.
--
-- The planner is not wrong by its own cost model — it weighs one sequential scan against 500 index
-- descents — but the cost it minimises grows with the whole instance while the alternative grows with one
-- guild, so the gap widens for the life of the instance. Handing it the ids removes the choice.
--
-- # Scoped to the guild as well, even though the caller supplies the ids
--
-- The first draft left the guild out, reasoning that the ids come from ListGuildChannels and are therefore
-- server-derived. That is the argument CopyChannelOverwrites made about its destination one file over, and
-- this milestone rejected it there after reproducing four rows crossing between guilds: a rule that holds
-- because of who happens to call it is not the rule. The same applies here and the consequence is the same
-- shape — a member-tier target_id is a global user id, so another guild's deny genuinely matches and
-- silently removes a permission from this guild's resolution.
--
-- The join costs nothing measurable: the ids already restrict the scan to one guild's channels, so this
-- adds a primary-key lookup per channel to a query that was already reading those rows.
SELECT po.channel_id, po.target_type, po.target_id, po.allow, po.deny
FROM permission_overwrites po
JOIN channels c ON c.id = po.channel_id
WHERE po.channel_id = ANY(sqlc.arg(channel_ids)::bigint[])
  AND c.guild_id = sqlc.arg(guild_id)::bigint;
