-- Milestone M15 — the messages a channel holds, and the history of their edits.
--
-- **This is half of `docs/architecture.md` §2's block, deliberately.** That block spans two milestones and
-- says so per line since M15's planning: the table and its lookup index are M15's, while everything
-- supporting search — the `content_search` generated column and both GIN indexes — is M65's, and one of
-- those needs the `pg_trgm` extension. Building them here would pull an extension dependency into a
-- milestone that has no query using it, and `content_search` would be a generated column maintained on
-- every insert on the hottest write path in the product for a reader that does not exist for fifty
-- milestones. M65's roadmap entry now states what that costs it: the column arrives as an ALTER over real
-- data, which is a full table rewrite under ACCESS EXCLUSIVE. That is a deployment question, and the
-- answer is still not to build it early.

CREATE TABLE messages (
  id          bigint PRIMARY KEY,                               -- snowflake (ADR 0003): time-ordered *and*
                                                                --   unique, which is what lets the cursor
                                                                --   below page without skipping a row
  channel_id  bigint NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  -- Nullable, and no ON DELETE, matching audit_log_entries.actor_id and for the same reason: a message
  -- whose author went NULL is a message with the answer removed.
  --
  -- **Settled 2026-09-16: a deleted account's messages survive, attributed to "Deleted User."** So the
  -- refusing FK is not a problem M76a has to work around — it is the guarantee that makes that the only
  -- reachable outcome. Account deletion is a soft delete with a placeholder rename, so `DELETE FROM users`
  -- never runs; if a hard delete is ever attempted, this constraint stops it rather than letting a
  -- milestone quietly NULL out authorship and call it deletion. The nullable column exists for a message
  -- that genuinely has no author — a system or webhook message (`type` below) — not as somewhere to put a
  -- person who left.
  --
  -- The cost is stated where it belongs, in M76a: erasure requests cannot be satisfied by deleting the
  -- account, because the content is what survives. Editing a message before deleting the account does not
  -- help either — message_edit_history keeps the prior text and has no deletion path of its own.
  author_id   bigint NULL REFERENCES users(id),
  content     text NOT NULL,
  -- 0 DEFAULT, 1 SENT_VIA_AUTOMATION (webhooks at M60, bot automation at M22). Reserved system values
  -- above those. smallint rather than an enum type for audit_log_entries.action's reason: the vocabulary
  -- grows with milestones and an enum makes each addition a migration.
  type        smallint NOT NULL DEFAULT 0,
  -- ON DELETE SET NULL, not CASCADE: deleting a message must not delete the replies to it, or removing
  -- one post silently removes a conversation that continued past it.
  reply_to_id bigint NULL REFERENCES messages(id) ON DELETE SET NULL,
  -- **M15's, though nothing sets it true until M97**, and it is not dead weight. It is the column M65's
  -- generated `content_search` keys its exclusion off (`CASE WHEN is_e2e THEN NULL ELSE …`), so rule 13
  -- is designed into the schema rather than retrofitted onto a populated table later. True only for
  -- DM-channel-type messages sent under E2E, where `content` holds ciphertext the instance cannot read.
  is_e2e      boolean NOT NULL DEFAULT false,
  -- NULL until the first edit. The *previous* content goes to message_edit_history in the same
  -- transaction as the update, so this timestamp and that row are written together or not at all.
  edited_at   timestamptz NULL,
  -- Soft delete. The row survives so M16 can carry a report against it and M16a can still resolve the
  -- message an edit-history entry belongs to — a hard delete would make a reported message vanish from
  -- the moderation queue it was reported into.
  deleted_at  timestamptz NULL,
  created_at  timestamptz NOT NULL DEFAULT now()
);

-- The channel backlog: newest first, paginated, deleted rows excluded. This is rule 7's index and the one
-- query shape M15 adds that runs at load.
--
-- Not partial on `deleted_at IS NULL`, and that is deliberate after 000005's and 000012's lesson in the
-- other direction. A partial index is right when the excluded rows are the majority and nothing ever asks
-- for them; here the excluded rows are a small minority that grows slowly, and M16's moderation surface
-- reads deleted messages *on purpose*. A partial index would serve the listing and silently not serve the
-- one reader that most needs an index.
--
-- Measured on 400,000 messages across 2,000 channels, one page of 50 newest-first:
--
--   with     0.416 ms,     56 buffers   Index Scan, no sort node
--   without 12.094 ms, 22,090 buffers   Index Scan Backward on messages_pkey, filtering
--
-- The buffer ratio is the interesting half, not the 29x: without this index Postgres walks the primary
-- key backwards from the newest message *on the instance* discarding everything not in this channel, so
-- the work grows with total instance traffic while the indexed form grows with the channel's. That is
-- 000015's argument for roles_guild_id_position_idx, on the table where it matters most.
CREATE INDEX messages_channel_id_id_idx ON messages (channel_id, id DESC);

-- **What the three indexes on this table cost, measured rather than asserted.** 50,000 inserts each way:
--
--   with all three    17.3 us/row
--   primary key only  13.7 us/row
--
-- 3.6 us, or 26%, on the highest-volume insert path in the product. Recorded because 000019 asserted an
-- index's cost without checking and M14 had to go back and measure it — and because the saving is the
-- only thing that makes the cost defensible, so the two belong next to each other: 12.094 ms to 0.416 ms
-- on the backlog read, and 6,934 ms to 5.651 ms on a 500-message delete. No index here is optional at
-- that ratio, but the next one added to this table should be made to argue against these numbers.
--
-- Both of these are foreign keys, and this project has now paid for an unindexed one twice — M11's
-- replaced_by_id at 3,757 ms in a trigger, M12's guild_member_roles.role_id at 4,566 ms on 500 deletions
-- — and cleared a third suspicion at M13 by measuring rather than assuming. So both were measured here
-- before being added; the numbers are below the table they defend.
--
-- reply_to_id is the self-reference, and it is exactly M11's shape: ON DELETE SET NULL means deleting one
-- message makes Postgres find every row referencing it, and without an index that is a sequential scan of
-- every message on the instance, inside the transaction the delete already holds open.
--
-- Deleting 500 messages, 400,000 in the table, 39,800 of them replies — the FK trigger's own time, which
-- is where this hides rather than in the DELETE's plan:
--
--   with        5.651 ms
--   without 6,934.104 ms
--
-- 1,227x, and larger than the case that taught this project the lesson: M11's replaced_by_id was 3,757 ms.
-- It is worse here for the reason it will keep getting worse — the scan is over the messages table, which
-- is the one table in the product with no ceiling on its growth.
CREATE INDEX messages_reply_to_id_idx ON messages (reply_to_id);

-- author_id is the refusing direction rather than the cascading one, and it still needs the index for
-- audit_log_entries_actor_id_idx's reason: the FK has no ON DELETE, so deleting a user requires Postgres
-- to prove no message references them, and proving a negative over an unindexed column is a full scan of
-- the largest table in the product — inside the transaction rule 17's revoke-everything is already
-- holding open.
--
-- Deleting one account that has posted nothing, which is the cheapest possible case and therefore the
-- honest one to quote — the FK trigger's time:
--
--   with     0.408 ms
--   without 14.439 ms
--
-- Thirty-five times for a single row, and the cost is paid per referencing table per delete. The number
-- to watch is not this one: it is that the unindexed form scans every message on the instance, so it
-- grows without bound while the indexed form stays flat.
CREATE INDEX messages_author_id_idx ON messages (author_id);

-- M15's, with the edit endpoint that writes it. It had no milestone at all until M15's planning read §2's
-- DDL against the roadmap and found nothing owned it — the same gap M14 found with account deletion and
-- closed as M76a, found the same way. It belongs to the milestone whose endpoint writes it: assigning it
-- later means a second migration and an edit path that recorded nothing in between.
--
-- **Nothing reads this table until M16a**, which is the shape audit_log_entries had from M12 to M14 —
-- written under a rule, read once a milestone owned the surface. M16a also owns the disclosure decision
-- the reader needs, and inherits rule 13 with it.
CREATE TABLE message_edit_history (
  id         bigint PRIMARY KEY,
  message_id bigint NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  -- The content *before* the edit that created this row. Appended in the edit's own transaction, so a
  -- successful edit that recorded no history is not a state the code can reach.
  content    text NOT NULL,
  edited_at  timestamptz NOT NULL DEFAULT now()
);

-- Serves both readers, which is why it is composite rather than two indexes. M16a reads one message's
-- versions in order; the CASCADE above needs the leading column to find them when a message is deleted.
--
-- **Superseded by 000022, which replaces this index with (message_id, id DESC).** The measurement below
-- assumed M16a would order by edited_at and it does not — a cursor is an id (M14), so the ordering this
-- index serves is not the one the shipped query asks for. The cascade figure is unaffected and was
-- re-measured there; read 000022 for the comparison. Left in place rather than rewritten because an
-- applied migration is not a document to edit, and the reasoning here is still why the column is indexed
-- at all.
--
-- Measured against 200,000 history rows over 400,000 messages. Nothing writes this table until Part C of
-- this milestone, so the rows were seeded to measure it — an index defended by an argument alone is the
-- thing M13 disproved by checking, and an empty table cannot disprove anything.
--
--   M16a's read, one message's versions:       0.047 ms,     7 buffers   Index Scan
--     without                                                1,907 buffers, Sort → quicksort
--   the CASCADE, deleting 500 messages:        3.301 ms      (FK trigger time)
--     without                                  2,304.611 ms
--
-- 698x on the cascade, and that path runs on every message deletion rather than on a moderation read, so
-- it is the half that justifies shipping the index now rather than with M16a's endpoint.
CREATE INDEX message_edit_history_message_id_edited_at_idx
  ON message_edit_history (message_id, edited_at DESC);

-- **Not swept, like audit_log_entries and for a weaker version of its reason.** An edit history with a
-- retention window is one that forgets exactly the edit somebody wants to look at. Unlike the audit log
-- this is content rather than an accountability record, so a future retention policy is arguable here
-- where it is not there — but it is M16a's or M125's decision to argue, not something the schema should
-- impose before anything can even read the table.
