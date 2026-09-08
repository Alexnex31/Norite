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
-- owner_id and is_member repeat on every row. That is two columns times a handful of roles, and the
-- alternative is the extra round trip this query exists to avoid.
SELECT
    g.owner_id,
    (gm.user_id IS NOT NULL)::boolean AS is_member,
    r.id AS role_id,
    r.permissions AS role_permissions,
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
SELECT po.target_type, po.target_id, po.allow, po.deny
FROM permission_overwrites po
JOIN channels c ON c.id = po.channel_id
WHERE po.channel_id = sqlc.arg(channel_id) AND c.guild_id = sqlc.arg(guild_id)::bigint;
