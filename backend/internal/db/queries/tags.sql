-- Message tags (Milestone M17).

-- name: CreateMessageTag :one
-- The uniqueness guards are the two partial indexes in 000024, not a check here: a read-then-insert races
-- under READ COMMITTED the way M10's invite redemption did, where four of four concurrent racers got in.
-- A collision arrives as a unique violation and the service maps it.
INSERT INTO message_tags (id, guild_id, name, created_by, is_shared)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetMessageTag :one
SELECT * FROM message_tags WHERE id = $1;

-- One guild's tags: every shared one, plus the caller's own private ones.
--
-- **The visibility filter is what makes "private" mean anything**, and it is in the SQL rather than in a
-- Go loop for the reason rule 13's exclusion is: the next reader of this table inherits it instead of
-- having to remember it. A private tag is its creator's alone — not the guild's, not a moderator's — so
-- the predicate is `is_shared OR created_by = $2` and there is deliberately no variant without it.
--
-- Served by the two partial unique indexes bitmap-OR'd together rather than by a guild_id index, which
-- 000024 measured as unnecessary and slightly worse. Bounded at creation rather than paginated (M12's
-- rule): `tags.Service` caps a guild's shared tags and each member's private ones, so this list has a
-- ceiling and does not need a cursor.
--
-- name: ListGuildMessageTags :many
SELECT * FROM message_tags
WHERE guild_id = $1 AND (is_shared OR created_by = $2)
ORDER BY id DESC;

-- name: CountGuildSharedTags :one
SELECT count(*) FROM message_tags WHERE guild_id = $1 AND is_shared;

-- name: CountMemberPrivateTags :one
SELECT count(*) FROM message_tags WHERE guild_id = $1 AND created_by = $2 AND NOT is_shared;

-- name: DeleteMessageTag :execrows
-- Applications cascade with it (000024). The authority check is the service's; this is scoped by guild as
-- well as id so a tag id from another guild is not reachable through this guild's path — M15's
-- loadInChannel discipline, one object over.
DELETE FROM message_tags WHERE id = $1 AND guild_id = $2;

-- Apply a tag to a message, refusing a pair that crosses a guild boundary.
--
-- # The cross-guild predicate is the whole point of the statement's shape
--
-- A tag knows its guild; a message reaches one only through channels. So `c.guild_id = t.guild_id` is what
-- stops guild A's tag landing on guild B's message — written here rather than checked in Go, because a
-- guard in the statement cannot be skipped by the next writer and cannot race. That is the answer M16b's
-- security review arrived at for the recording log, and the shape M16 needed when `reports` had no
-- guild_id at all.
--
-- It is not defence against a confused client. Both ids come from an authorized request, and the service
-- checks the caller may see the message and may use the tag. It is defence against the *next* writer —
-- a bulk importer, an automation path (M22), a webhook (M60) — which will not be holding the authorize
-- result this one does.
--
-- `deleted_at IS NULL` refuses a tag on a soft-deleted message: the moderation surfaces read deleted
-- messages on purpose (000020), but adding a *new* fact to something already removed is a different act,
-- and a tag applied after deletion would surface a deleted message in a tag listing.
--
-- `m.channel_id = channel_id` binds the message to the channel the route named, which is the channel that
-- was authorized. The guild predicate alone let a member reach a message in a channel they cannot see
-- through one they can — found by M17's manual pass, answering 204 where an id naming nothing answered
-- 404. The service checks the same thing first (tags.loadMessageInChannel), because the repeat-detection
-- after a refused insert would otherwise report an application on that message as success; this copy is
-- the one the next writer inherits.
--
-- ON CONFLICT DO NOTHING makes applying twice idempotent rather than an error, which is what a client
-- retrying a request wants. The row count **cannot** tell "already applied" from "refused" — both are
-- zero — so the service reads the application back (GetMessageTagApplication) to tell them apart. This
-- comment said the row count did until M17's /code-review.
--
-- name: ApplyMessageTag :execrows
INSERT INTO message_tag_applications (tag_id, message_id, applied_by)
SELECT t.id, m.id, sqlc.arg(applied_by)::bigint
FROM message_tags t
JOIN messages m ON m.id = sqlc.arg(message_id)::bigint
  AND m.channel_id = sqlc.arg(channel_id)::bigint AND m.deleted_at IS NULL
JOIN channels c ON c.id = m.channel_id AND c.guild_id = t.guild_id
WHERE t.id = sqlc.arg(tag_id)::bigint
ON CONFLICT (tag_id, message_id) DO NOTHING;

-- name: UnapplyMessageTag :execrows
-- No guild predicate needed: the pair is the primary key, and a pair that crosses a guild could never have
-- been written by the statement above. The authority check is the service's.
DELETE FROM message_tag_applications WHERE tag_id = $1 AND message_id = $2;

-- Every tag on each of a set of messages, with the caller's visibility filter applied.
--
-- **One statement for a whole page, never one per message.** Fifty messages resolved one at a time is
-- §15.2's N+1 on the path every client hits to draw a channel; `= ANY($1)` is one round trip whatever the
-- page size. 000024 measures the index this leans on at 245 buffers against 837, and 2.1 ms against
-- 10.5 ms, because without it the scan is over every application on the instance rather than over the
-- page.
--
-- The same `is_shared OR created_by` filter as the listing, for the same reason: somebody else's private
-- tag must not appear on a message you can read, or "private" describes only who may apply it.
--
-- name: ListTagsForMessages :many
SELECT a.message_id, sqlc.embed(t), a.applied_by, a.applied_at
FROM message_tag_applications a
JOIN message_tags t ON t.id = a.tag_id
WHERE a.message_id = ANY(sqlc.arg(message_ids)::bigint[])
  AND (t.is_shared OR t.created_by = sqlc.arg(viewer_id)::bigint)
ORDER BY a.message_id DESC, t.id;

-- name: GetMessageTagApplication :one
-- Reads one application by its primary key. Two callers want it for different reasons: Unapply needs
-- `applied_by` to decide authority, and Apply uses its presence to tell "already applied" from "refused",
-- which the insert's row count cannot distinguish because ON CONFLICT DO NOTHING also reports zero.
SELECT * FROM message_tag_applications WHERE tag_id = $1 AND message_id = $2;

-- name: LockMessageTag :exec
-- Holds a tag still while its deletion decides whether to write an audit entry. FOR UPDATE conflicts with
-- the FOR KEY SHARE lock the foreign-key check on message_tag_applications takes, so an application
-- inserted concurrently waits for this transaction rather than slipping in after the count below and
-- being cascaded away unrecorded.
SELECT id FROM message_tags WHERE id = $1 FOR UPDATE;

-- name: CountMessageTagApplicationsByOthers :one
-- How many of a tag's applications somebody other than the actor made. Deleting a shared tag cascades
-- every application of it, and a deletion that takes other people's labels with it is authority over
-- them (rule 2), which is what decides whether `tag.delete` is written. Served by the primary key's
-- leading tag_id.
SELECT count(*) FROM message_tag_applications WHERE tag_id = $1 AND applied_by <> $2;
