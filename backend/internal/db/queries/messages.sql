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
