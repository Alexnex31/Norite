-- name: CreateMessage :one
INSERT INTO messages (id, channel_id, author_id, content, type, reply_to_id)
VALUES ($1, $2, $3, $4, $5, sqlc.narg(reply_to_id)::bigint)
RETURNING *;

-- One page of a channel's backlog (Milestone M15).
--
-- # The cursor, and why it is COALESCE rather than the obvious form
--
-- `before` and `after` are both optional, and the obvious spelling for an optional bound is
-- `$n IS NULL OR id < $n`. M14 measured that on audit_log_entries and it cannot become an index qual
-- under a generic plan — 1859 buffers against 9 — because the planner has to keep the OR for the case
-- where the parameter is NULL. COALESCE against the type's extreme collapses to a plain comparison the
-- index serves. The same trick buys nothing for an equality filter, where the rewrite would name the
-- column on both sides.
--
-- Ordered by id rather than created_at, for the reason M14's cursor is: a snowflake is time-ordered *and*
-- unique, while nothing constrains created_at, so a page boundary landing inside a group of equal
-- timestamps would skip or repeat a row.
--
-- Deleted messages are excluded here and not by a partial index. 000020 says why: M16's moderation
-- surface reads them on purpose, so an index that could not serve that reader would be the wrong shape.
--
-- name: ListChannelMessages :many
SELECT * FROM messages
WHERE channel_id = $1
  AND deleted_at IS NULL
  AND id < COALESCE(sqlc.narg(before)::bigint, 9223372036854775807)
  AND id > COALESCE(sqlc.narg(after)::bigint, 0)
ORDER BY id DESC
LIMIT $2;

-- name: GetMessage :one
SELECT * FROM messages WHERE id = $1 AND deleted_at IS NULL;

-- Read for update, so an edit's prior-state read and its write are one atomic step.
--
-- M14 learned this on audit_log_entries the hard way: a diff that reads prior state and then writes has a
-- window under READ COMMITTED where a concurrent commit lands in between, and the record then claims a
-- transition that never happened. An edit here writes the *previous* content to message_edit_history, so
-- the same window would file a history row against content that was already gone.
--
-- name: GetMessageForUpdate :one
SELECT * FROM messages WHERE id = $1 AND deleted_at IS NULL FOR UPDATE;

-- name: UpdateMessageContent :one
UPDATE messages SET content = $2, edited_at = now() WHERE id = $1 AND deleted_at IS NULL RETURNING *;

-- Soft delete, so the row survives for M16 to carry a report against and for M16a to resolve an edit
-- history back to its message. A hard delete would make a reported message vanish from the queue it was
-- reported into.
--
-- name: SoftDeleteMessage :exec
UPDATE messages SET deleted_at = now() WHERE id = $1 AND deleted_at IS NULL;

-- name: AppendMessageEditHistory :exec
INSERT INTO message_edit_history (id, message_id, content) VALUES ($1, $2, $3);

-- The denormalized pointer 000015 created with no foreign key, maintained here.
--
-- Written in the send's own transaction rather than derived on read: the alternative is max(id) per
-- channel on every channel listing, which is the N+1 §15.2 names. The column carries no REFERENCES
-- clause on purpose — a foreign key would make deleting a message rewrite the channel row on the hottest
-- write path in the product.
--
-- # GREATEST, and why it is what lets the send stop locking the channel
--
-- A bare assignment is only monotonic if something serializes the writers, and until M15's optimization
-- pass that something was the channel row lock Send took through AuthorizeChannel — which cost about 3x
-- the throughput of the product's hottest write, measured, because every send in a channel queued behind
-- every other for its whole transaction. Two concurrent sends committing out of order would otherwise
-- walk this pointer backwards and break unread state.
--
-- GREATEST makes the statement monotonic by itself, so the ordering no longer has to be bought with a
-- lock. It ignores NULLs, so the first message in a channel still sets the pointer.
--
-- name: SetChannelLastMessage :exec
UPDATE channels SET last_message_id = GREATEST(last_message_id, $2) WHERE id = $1;

-- The message an edit history belongs to, deleted rows included (Milestone M16a).
--
-- GetMessageForReport's twin, and deliberately a second query rather than a shared one: they differ in
-- what they mean, not only in what they select. That one answers "may this be reported"; this one answers
-- "what does this message say now", which the history envelope carries because there is no single-message
-- GET anywhere in the API — /channels/{id}/messages/{id} serves PATCH and DELETE only. Without the
-- current version a moderator reads every prior version and has no route that will tell them what the
-- message says today, which is the question a report is about.
--
-- Deleted rows included for GetMessageForReport's reason, stated in 000020 in as many words: the soft
-- delete exists so a reported message still resolves, and a message deleted after it was reported is
-- exactly the one a moderator is reading the history of.
--
-- Rule 13 is a LEFT JOIN predicate rather than a CASE, which is M16's shape for both its reasons. The
-- exclusion is inherited by whoever writes the next reader of this join, and sqlc cannot type an
-- expression through a LEFT JOIN — the CASE form generates interface{}, and casting it to text generates
-- a non-nullable string that a NULL fails to scan into at runtime. `mv` is this row again behind
-- NOT is_e2e, so an encrypted message yields NULL content beside a true flag rather than the same NULL a
-- vanished row would give.
--
-- name: GetMessageWithHistoryTarget :one
SELECT
  m.id,
  m.channel_id,
  m.author_id,
  m.is_e2e,
  m.deleted_at,
  m.edited_at,
  mv.content AS current_content
FROM messages m
LEFT JOIN messages mv ON mv.id = m.id AND NOT mv.is_e2e
WHERE m.id = $1;

-- One page of a message's prior versions, newest first (Milestone M16a).
--
-- # It pages, because nothing bounds how many versions a message has
--
-- M12 settled that "a list that cannot be paginated is bounded at creation instead" — the channel and
-- role ceilings exist because of it. This table has neither half: Update carries no edit counter and no
-- cooldown, so a message accumulates versions without limit. Bounding *edits* was the alternative and it
-- is the wrong one, because a message that stops being editable after N corrections is a worse product
-- and a worse privacy story on the one table whose entries can never be erased.
--
-- The cursor is an id and the COALESCE spelling is M14's, measured there at 9 buffers against 1,859.
-- Ordering by id rather than edited_at is what 000022's index replacement is for: nothing constrains
-- edited_at, which is transaction time, so two rows committing in the same microsecond would make a page
-- boundary skip or repeat.
--
-- Rule 13 is an inner join here rather than the LEFT JOIN above, and the difference is deliberate: an
-- encrypted message has no readable versions at all, so the right answer is an empty page, while the
-- envelope's own is_e2e flag is what says why. Returning rows with NULL content would put the exclusion
-- in two places, which is the M15 lesson about a bound enforced twice.
--
-- name: ListMessageEditHistory :many
SELECT h.id, h.content, h.edited_at
FROM message_edit_history h
JOIN messages m ON m.id = h.message_id AND NOT m.is_e2e
WHERE h.message_id = $1
  AND h.id < COALESCE(sqlc.narg(before)::bigint, 9223372036854775807)
ORDER BY h.id DESC
LIMIT $2;

-- The opt-in message audit (Milestone M16b).
--
-- These three live in messages.sql rather than guilds.sql because `messages` is the only package that
-- calls them — the writer is Send/Update/Delete and the reader is mounted from the same handler — and the
-- decisions they carry are message decisions rather than guild ones. Same call reports.sql makes for
-- GetMessageForReport; sqlc generates one package either way.

-- Whether this guild records its messages.
--
-- Read once per message mutation, inside the mutation's own transaction, and the branch is in Go. The
-- whole-thing-in-one-statement alternative was measured and rejected — see 000023, which has the numbers:
-- it costs 62-90% of the message insert for a feature that is off in every guild until somebody turns it
-- on, and the unraceability it was supposed to buy is illusory, because either shape reads the flag after
-- the message is already written.
--
-- A primary-key lookup on a table holding one row per guild. Deliberately *not* folded into
-- ListGuildMemberAuthority, which already reads this row and could carry the column for free: that query
-- feeds roles.Resolve, and a recording policy is not one of ADR 0008's layers. See 000023.
--
-- name: GuildRecordsMessages :one
SELECT message_audit_enabled FROM guilds WHERE id = $1;

-- Record one action against one message.
--
-- # Three predicates, none of which trusts the caller
--
-- Deliberate redundancy, and each one turns a property of the *callers* into a property of the schema.
-- Together they cost something only on a guild that opted in, and nothing at all on one that did not,
-- because this statement does not run there — see 000023 for the numbers.
--
--   * `g.message_audit_enabled` is the milestone's first done-when clause: no row can reach this table
--     for a guild whose flag is false, whatever the Go branch that just read it believes.
--   * `g.id = c.guild_id` ties the message's own channel to the guild being written. **Unreachable
--     today** — all three callers take `guild_id` and `message_id` from the same guildauth call, and
--     `loadInChannel` refuses a message from another channel — which is exactly why it is written rather
--     than argued. A fourth writer passing an inconsistent pair would otherwise file one guild's content
--     into another guild's log, silently; M60's webhook ingest is the named candidate, and it will not be
--     holding the same authorize result these three do.
--   * `NOT m.is_e2e` is rule 13, on the row rather than in Go, so the next writer inherits it.
--
-- # Content and channel come off the message row, never from parameters
--
-- Two reasons that agree. What is recorded cannot disagree with what was stored, which is the whole value
-- of an audit row. And rule 13's exclusion is `NOT m.is_e2e` on that same row — a join predicate rather
-- than a Go check, so whoever writes the next writer inherits it instead of having to remember it, which
-- is the form M16 settled on and M16a reused.
--
-- An E2E message therefore records *nothing* rather than a row with NULL content. That is the right
-- answer for the same reason ListMessageEditHistory returns an empty page: a row saying "something was
-- said here and we cannot tell you what" is an exclusion written twice.
--
-- name: RecordMessageAudit :exec
INSERT INTO message_audit_entries (id, guild_id, message_id, channel_id, actor_id, action, content)
SELECT
  sqlc.arg(id)::bigint,
  g.id,
  m.id,
  m.channel_id,
  sqlc.arg(actor_id)::bigint,
  sqlc.arg(action)::varchar,
  m.content
FROM messages m
JOIN channels c ON c.id = m.channel_id
JOIN guilds   g ON g.id = c.guild_id
               AND g.id = sqlc.arg(guild_id)::bigint
               AND g.message_audit_enabled
WHERE m.id = sqlc.arg(message_id)::bigint AND NOT m.is_e2e;

-- One page of a guild's recording log, newest first.
--
-- No rule-13 predicate here and that is not an omission: the exclusion happened at write time, so there
-- is nothing in this table to exclude. Repeating it on the read would be the bound-enforced-twice mistake
-- M15 recorded, where the two copies measured different things — and the structural half is pinned by a
-- test asserting guild_id is NOT NULL, since a DM has no guild and could never have produced a row.
--
-- The cursor is an id and the COALESCE spelling is M14's. 000023 measures this query at three densities
-- and the index it needs turns on how many guilds record rather than on how large any one of them is.
--
-- name: ListGuildMessageAudit :many
SELECT * FROM message_audit_entries
WHERE guild_id = $1
  AND id < COALESCE(sqlc.narg(before)::bigint, 9223372036854775807)
ORDER BY id DESC
LIMIT $2;
