-- Milestone M12 — the audit log, pulled forward from M14 because rule 2 needs it now.
--
-- Rule 2: every guild-scoped mutation writes an audit log entry, in the same DB transaction as the
-- mutation. M12 is the first milestone that *has* guild-scoped mutations, and the table was scheduled for
-- M14 — so the rule would have been false for two milestones and then retrofitted across every handler
-- written in between.
--
-- Reordering the two milestones was not an option: audit_log_entries references guilds(id), so guilds has
-- to exist first. Pulling the table forward is the only ordering that makes rule 2 true from the first
-- POST /guilds. M14 keeps what makes it a milestone — GET /guilds/{guild_id}/audit-log, the `changes`
-- diffing, and the coverage test that every mutation type produces exactly one entry — and stops being the
-- milestone that introduces the table. Its roadmap entry is rewritten in the same pull request as this
-- migration, so the next reader does not find a milestone whose first half is already built.
--
-- Nothing reads this table until M14. Everything writes to it from the first handler in this milestone.

CREATE TABLE audit_log_entries (
  id         bigint PRIMARY KEY,                            -- snowflake (ADR 0003), so entries sort by time
  -- Nullable, and that is what makes this table usable for more than guilds later: an instance-scoped
  -- action has no guild. Rule 14 gives those their own table (instance_audit_log, M69) rather than sharing
  -- this one, so today every row written here carries a guild — but the column stays nullable because §2
  -- specifies it and because narrowing it later is a rewrite of the table.
  guild_id   bigint NULL REFERENCES guilds(id) ON DELETE CASCADE,
  -- No ON DELETE. An audit entry naming a deleted actor is still evidence, and an entry whose actor went
  -- NULL is evidence with the answer removed. The FK refuses the delete, and the account-deletion path
  -- (M66) has to decide what to do about it in the open.
  actor_id   bigint NOT NULL REFERENCES users(id),
  -- A stable string, not an enum: this vocabulary grows with every milestone that adds a mutation, and an
  -- enum type would make each addition a migration. varchar(64) because the values are written by this
  -- codebase and read by clients, so an unbounded column buys nothing.
  action     varchar(64) NOT NULL,
  -- What was acted on — a channel, a role, a member. Deliberately not a foreign key: the target is
  -- polymorphic, and an FK would also delete the record of a deletion along with the thing deleted, which
  -- is precisely the entry an operator goes looking for.
  target_id  bigint NULL,
  -- The before/after diff. M14 owns its shape and the diffing that produces it; M12 writes NULL or a flat
  -- object of changed fields. jsonb rather than json so a later query can index into it.
  changes    jsonb NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- The one read M14 makes: a guild's log, newest first, paginated. Measured on 300,000 entries across
-- 5,000 guilds:
--
--   with the index    0.356 ms,   55 buffers   Index Scan, no sort node
--   without           9.457 ms, 3167 buffers   Parallel Seq Scan, then a sort per worker
--
-- The DESC is in the index rather than only in the query, and it earns that on the paginated read
-- specifically — the plan above walks the index backwards and stops at the LIMIT, with no sort at all.
-- Drop the LIMIT and Postgres goes back to a bitmap heap scan and a quicksort even with this index
-- present, exactly as it does for channels_guild_id_position_idx in 000015. Both are the same lesson:
-- a composite index's second column pays on the query the endpoint actually makes, and the unbounded
-- version of that query is not evidence about it.
--
-- The index ships with the table rather than with M14's endpoint (rule 7), because by then the table will
-- have a milestone's worth of rows in it and building an index on a populated table is a different
-- operation from building one on an empty one.
CREATE INDEX audit_log_entries_guild_id_created_at_idx ON audit_log_entries (guild_id, created_at DESC);

-- Deleting a user is refused while any entry names them as actor, and that check needs an index for the
-- same reason guilds_owner_id_idx does — otherwise it is a sequential scan of the instance's entire audit
-- history, inside the transaction rule 17's revoke-everything already holds open.
--
--   with the index    0.304 ms,  125 buffers   Bitmap Index Scan
--   without          18.509 ms, 3153 buffers   Seq Scan
--
-- Sixty times, and it is the audit log rather than guilds that makes this the expensive one: this table
-- grows with every mutation on the instance, so the scan it replaces gets longer forever.
CREATE INDEX audit_log_entries_actor_id_idx ON audit_log_entries (actor_id);

-- **Not swept, and this one is load-bearing rather than merely absent.**
--
-- Every other table auth/ has added carries a TTL and a delete in SweepExpired. This one must not. An
-- audit log with a retention window is an audit log an attacker waits out: the whole value of the record
-- is that it outlives the incident that made somebody want it gone. If an instance ever needs a retention
-- policy it is an operator decision with an operator-visible setting, not a sweeper the schema imposes.
