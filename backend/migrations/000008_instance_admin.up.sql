-- Milestone M10 — who administers this instance.

-- The Instance Admin tier (ADR 0013): a flat, instance-wide capability that sits entirely outside
-- roles.Resolve. A guild role grants power inside one guild; this grants power over the instance itself,
-- and the two must never be resolvable through the same call — a permission bitfield that could carry
-- "may ban across the whole instance" is one bad Resolve away from granting it.
--
-- # Why this table lands 61 milestones before the one that owns it
--
-- docs/roadmap.md gives this table to M71, and M71 keeps everything that makes the tier *manageable*:
-- granting, revoking, more than one admin, and the safety rail that refuses to remove the last one. What
-- lands here is the fact itself, because M10 cannot do its own job without it. The wizard's whole purpose
-- is to end with a working administrator, and "administrator" has to mean something recorded rather than
-- implied; the invite endpoints beside it need something to gate on. A boolean smuggled onto `users`, or
-- an implicit "whoever registered first", would both have to be migrated away at M71.
--
-- The shape is docs/architecture.md §2's, unchanged.
CREATE TABLE instance_admins (
  -- The account, and the primary key. One row per admin, so the tier cannot be held twice, and the
  -- only read this table has ("is this user an admin?") is served by the key itself.
  user_id    bigint PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,

  -- Who granted it, or NULL for the bootstrap admin, who was granted it by whoever holds the instance
  -- config file rather than by another account. Nullable for that case specifically: a self-referencing
  -- row would claim the first admin granted the tier to themselves, which is a different and less honest
  -- statement than "nobody in this table did".
  --
  -- ON DELETE is deliberately absent, so this defaults to NO ACTION and a grantor's account cannot be
  -- hard-deleted while the record of what they did survives. Accounts are soft-deleted here (users
  -- carries deleted_at), so nothing in the product hits that today; it is the durable-provenance
  -- behaviour to have if anything ever does.
  granted_by bigint NULL REFERENCES users(id),
  granted_at timestamptz NOT NULL DEFAULT now()
);

-- granted_by is a foreign key with no index, and unlike oauth_states.device_code_id in 000007 that is
-- correct here. That column needed one because ON DELETE CASCADE makes Postgres scan the referencing
-- table once per deleted row, and the sweep deletes in batches. Nothing cascades from here — NO ACTION
-- still checks, but an instance has a handful of admins, so the scan is over a handful of rows, and no
-- milestone in this roadmap ever deletes a users row at volume.
