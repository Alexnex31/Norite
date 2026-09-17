-- Milestone M16 — the reports a guild has to triage.
--
-- **This is the guild half of a unified table, and the split is per column rather than per milestone.**
-- `docs/architecture.md` §2 draws `reports` as one table serving four routings — guild, instance-level,
-- DM/Group-DM and whisper — and M16 builds only the first. So the columns are all here (a later routing
-- must not be an ALTER over a populated table) while the *vocabulary* in three of them is deliberately
-- narrower than what the comments reserve. §2's block now carries the same per-line annotation M15 gave
-- `messages`, because that block is what somebody copies.
--
-- **One column §2 does not draw: `guild_id`.** Found at M16's planning by reading that DDL against this
-- milestone's own done-when, which is "a PermManageMessages holder can see and resolve it" — a query this
-- table as drawn cannot answer. The reasoning is below, on the column.

CREATE TABLE reports (
  id          bigint PRIMARY KEY,                     -- snowflake (ADR 0003), and the triage cursor
  -- No ON DELETE, matching messages.author_id: a report whose reporter was hard-deleted is a report with
  -- its provenance removed, and account deletion is a soft delete with a placeholder rename (M76a), so
  -- `DELETE FROM users` never runs. If one is ever attempted this constraint stops it rather than letting
  -- a milestone quietly orphan the moderation record.
  --
  -- **Never sent to a guild moderator.** The column exists for M74's instance triage, for the dedupe
  -- index below, and for the account export — not for the guild-scoped read surface M16 builds. See the
  -- security ledger: a guild moderator is not a vetted actor, and the export asymmetry in §2 already
  -- encodes that a reporter is protected from the person they reported.
  reporter_id bigint NOT NULL REFERENCES users(id),
  -- 0 message, and **only** 0 at M16. 1 whisper (M61), 2 channel, 3 user are reserved here and refused at
  -- the boundary rather than accepted and stranded — M14's lesson about an unknown filter value, applied
  -- to a write: a target type nothing can triage is a report filed into a queue that will never show it.
  --
  -- smallint rather than an enum type, for audit_log_entries.action's reason: the vocabulary grows with
  -- milestones and an enum makes each addition a migration.
  target_type smallint NOT NULL,
  -- Polymorphic, so it cannot be a foreign key — M12 already paid for this shape on
  -- permission_overwrites.target_id. Nothing cleans up after it and nothing needs to while the target is
  -- a soft-deleted message: M15 made message deletion soft precisely so a reported message still resolves
  -- after its author removes it. A target type whose rows are hard-deleted would owe a cleanup path.
  target_id   bigint NOT NULL,
  -- **M16's addition to §2's block, and the milestone does not work without it.**
  --
  -- The done-when is a per-guild triage queue. As §2 draws this table there is no guild anywhere in it,
  -- so that query would have to reach one by joining reports → messages → channels, which is wrong three
  -- ways: no index serves it, it breaks outright for three of the four target types above (a whisper, a
  -- channel and a user do not resolve through `messages`), and it would make a report's routing depend on
  -- a row a channel deletion can cascade away — the opposite of why the message delete is soft.
  --
  -- Measured on the same 220,000-report set the index below describes, one page of 50:
  --
  --   this column, with its index      0.098 ms      21 buffers
  --   the join §2 as drawn forces      6.593 ms   3,617 buffers   (a small guild)
  --   the same join, heavy guild      13.526 ms   6,389 buffers
  --
  -- 67x and 138x, and as with 000020's backlog index the buffer ratio is the half that matters: the join
  -- walks reports the instance-wide way and discards what belongs to other guilds, so its work grows with
  -- total instance traffic while the indexed form grows with this guild's.
  --
  -- So it is denormalized at filing time from the resolved target. NULL is the instance-routed case,
  -- which is M74's: a whisper or a plain DM has no guild to escalate to, which is the whole reason that
  -- milestone exists.
  --
  -- CASCADE, so a deleted guild takes its reports with it. That is the same state M12 asserted for
  -- audit_log_entries rather than left to look like a bug: rule 2 is satisfied by the entry being written
  -- in the transaction, and the durable record of an instance-level action is rule 14's instance_audit_log
  -- (M74), never this table.
  guild_id    bigint NULL REFERENCES guilds(id) ON DELETE CASCADE,
  -- A closed vocabulary the service validates, not free text: an unrecognised category is a report that
  -- sorts into no bucket in any triage view. varchar(32) matches audit_log_entries.action's width.
  reason_category varchar(32) NOT NULL,
  -- The reporter's own words, optional. **Untrusted free text**, and the first this backend stores that a
  -- moderator reads rather than a renderer: rule 19 applies to every client that prints it (M17a now says
  -- so), and rule 9 to every one that renders it.
  detail      text NULL,
  -- 0 open, 2 resolved, 3 dismissed. **1 under_review is reserved and unreachable at M16**: the done-when
  -- is "see and resolve", a third state exercises authority over nobody, and it would add an audit verb
  -- with nothing to say. M74 owns the richer queue and the state that goes with it.
  status      smallint NOT NULL DEFAULT 0,
  -- 0 guild moderators, 1 instance admins. **Computed server-side from the resolved target, never taken
  -- from the client.** A client-settable routing lets a member push a report past their own guild's
  -- moderators into the instance queue — and, in the direction that actually matters, lets a report
  -- *about* a moderator be routed to that same moderator.
  routed_to   smallint NOT NULL,
  -- Who resolved it. **Not redundant with the audit entry**, which is the obvious objection: the audit log
  -- is gated by PermViewAuditLog, which is not implied by PermManageMessages and is not in the default
  -- grant — so without this column a moderator cannot see who closed a report without holding a permission
  -- the triage view itself does not require.
  resolved_by bigint NULL REFERENCES users(id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  resolved_at timestamptz NULL
);

-- The triage page: one guild's reports, newest first, filtered to a status.
--
-- This is rule 7's index and the one query shape M16 adds that runs at load. Measured on PostgreSQL 16
-- against 220,000 reports over 2,000 guilds, 91% of them closed, ids interleaved across guilds — one page
-- of 50, newest first.
--
-- **The interleaving is load-bearing and the first seed got it wrong.** Clustering one guild's reports at
-- the top of the id range let every plan walk the primary key backwards and stop almost immediately,
-- which made every index here look unnecessary. A snowflake is minted at filing time, so a guild's
-- reports are scattered through the instance's range; seeding them contiguously measures a table no
-- instance has.
--
--   status-filtered, a small guild (~110 reports — most guilds)
--     (guild_id, status, id DESC)    0.101 ms      21 buffers   Index Scan
--     (guild_id, id DESC)            0.644 ms     109 buffers   Bitmap Heap Scan → quicksort
--     neither                        9.268 ms   3,509 buffers   Bitmap Heap Scan → quicksort
--
--   status-filtered, the heavy guild (22,099 reports, 2,009 open)
--     (guild_id, status, id DESC)    0.123 ms      58 buffers   Index Scan
--     (guild_id, id DESC)            0.615 ms     115 buffers   planner declines it, walks the pkey
--     neither                        0.625 ms     115 buffers
--
-- So the status column earns its place in the middle, and that is the whole choice: the default triage
-- view is one status at a time, which makes it an equality the index can serve before ordering by id.
--
-- **What it costs, stated because M12 got this axis wrong in the other direction.** With status between
-- guild_id and id, the index cannot produce id-order for a guild *without* a status equality — so the
-- "every status" view falls to a bitmap heap scan and a quicksort, 0.409 ms and 109 buffers against
-- 0.289 ms and 56 for (guild_id, id DESC). That is the secondary view paying 1.4x so the primary one can
-- pay 6x less, it is still 21x better than no index at all, and it is bounded by the page LIMIT either
-- way. A second index to recover it would cost every insert — M15 measured three indexes on `messages` at
-- 26% of insert time — for a view nobody opens to triage.
CREATE INDEX reports_guild_id_status_id_idx ON reports (guild_id, status, id DESC);

-- The foreign-key index this project has now paid for three times by not having one: M11's replaced_by_id
-- at 3,757 ms in a trigger, M12's guild_member_roles.role_id at 4,566 ms on 500 deletions, and M15
-- measuring messages.author_id at 35x for a single account with no posts at all.
--
-- Both of these are the *refusing* direction rather than the cascading one, which is the case that hides:
-- the FK has no ON DELETE, so deleting a user requires Postgres to prove no report references them, and
-- proving a negative over an unindexed column is a full scan — inside the transaction rule 17's
-- revoke-everything is already holding open.
--
-- Measured, and quoted for the cheapest possible case as M15 did — deleting one account that has filed
-- nothing and resolved nothing, so every millisecond below is spent proving a negative:
--
--                              reports_reporter_id_fkey   reports_resolved_by_fkey   whole DELETE
--   with both indexes                       0.122 ms                   0.044 ms         1.886 ms
--   without                                 7.296 ms                   7.411 ms        16.149 ms
--
-- 89x across the two triggers, and the number to watch is not that one: the unindexed form scans every
-- report on the instance, so it grows without bound while the indexed form stays flat. It is also paid
-- per referencing table per delete, which is why each one of these gets checked rather than assumed.
--
-- reporter_id is also the account export's read (M76a) and M74's reporter-history triage.
CREATE INDEX reports_reporter_id_idx ON reports (reporter_id);
CREATE INDEX reports_resolved_by_idx ON reports (resolved_by);

-- One open report per reporter per target, enforced here rather than by a check-then-insert.
--
-- **Guards that can be raced live in the statement.** M10 wrote invite redemption as a check-then-update
-- and four of four concurrent racers got in; the same discipline is why @everyone is undeletable by
-- `AND NOT is_default` in the DELETE rather than by an `if` above it. A duplicate-report check in Go has
-- exactly that shape, and report filing is the one write on this table an unprivileged member can drive.
--
-- It is also the per-user abuse bound the base rate limiter cannot supply: that limiter groups by IP, and
-- §14.14 asks for a per-user limit on filing. This bounds one reporter to one open report per target,
-- which is the half that matters. A ceiling on a reporter's *total* open reports is M74's, with the
-- reporter-history triage that would act on it.
--
-- Partial, and it does not subsume reports_reporter_id_idx above — the predicate excludes every resolved
-- and dismissed row, so it cannot serve the FK check or the export, which read regardless of status. That
-- is 000005's and 000012's lesson stated in the direction that makes a partial index correct rather than
-- the one that made it wrong: the excluded rows here are the ones this guard has nothing to say about,
-- and a second index covers the readers that need them.
CREATE UNIQUE INDEX reports_open_target_per_reporter_idx
  ON reports (reporter_id, target_type, target_id) WHERE status = 0;

-- **§2's (status, routed_to) index is deliberately not here.** It serves the Instance Admin queue — every
-- open report routed to tier 1, across the whole instance — and M16 builds no query with that shape. M15
-- measured three indexes on `messages` at 26% of insert time and said the next one added should be made
-- to argue against those numbers; an index whose only reader is fifty-eight milestones away cannot.
-- M74 ships it with the query that uses it, which is rule 7 read forwards.
