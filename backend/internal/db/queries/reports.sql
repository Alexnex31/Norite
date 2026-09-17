-- Milestone M16 — filing a report, and the guild-moderator triage queue that reads it.

-- name: CreateReport :one
INSERT INTO reports (id, reporter_id, target_type, target_id, guild_id, reason_category, detail, routed_to)
VALUES ($1, $2, $3, $4, sqlc.narg(guild_id)::bigint, $5, sqlc.narg(detail)::text, $6)
RETURNING *;

-- The message a report is being filed against, deleted rows included.
--
-- Deliberately not GetMessage, which filters `deleted_at IS NULL`. A message deleted seconds after it was
-- posted is exactly what somebody reports, and refusing to file against one would make deleting fast the
-- way to dodge a report. 000020 made the delete soft for this reason in as many words.
--
-- Lives in this file rather than messages.sql because `reports` is its only caller and the difference from
-- GetMessage is a reports decision, not a messages one — sqlc generates one package either way.
--
-- name: GetMessageForReport :one
SELECT * FROM messages WHERE id = $1;

-- One page of a guild's triage queue, filtered to a status.
--
-- # The listing carries no content, and that is a decision rather than an omission
--
-- It returns the report and two facts *about* the target — whether it is encrypted, and whether it has
-- been deleted — so a queue can badge a row without disclosing anything. The reported text lives behind
-- [GetGuildReport] alone.
--
-- Three reasons, in the order they matter. It gives rule 13's exclusion exactly one place to be right
-- instead of two, which is the M15 lesson about a bound enforced twice measuring two different things.
-- It bounds the page: fifty reports carrying a 4,000-character message each is a 200 kB response on a
-- surface somebody refreshes, which is rule 21 as much as §15. And no screen requires it — `6c` shows an
-- excerpt in its table and `6c` is M74's instance queue, not this one.
--
-- The earlier draft returned `left(content, $n)` here. Besides the above it would not type: an expression
-- through a LEFT JOIN defeats sqlc's nullability inference, so the column came back as a non-nullable
-- `string` that a missing target would fail to scan into at runtime.
--
-- # Two queries rather than one with an optional filter
--
-- The obvious shape is a single query with `status = $n OR $n IS NULL`. M14 measured the OR form on the
-- audit log's cursor and it cannot become an index qual under a generic plan — and it recorded that
-- COALESCE, which fixes that for a *range* bound, buys nothing for an equality filter, because the
-- rewrite names the column on both sides. So an optional equality has no single-query answer that keeps
-- the plan.
--
-- 000021's measurement makes the split the honest one anyway: the two cases do not want the same plan.
-- Filtered to a status the index serves the equality and the ordering together (0.101 ms, 21 buffers);
-- unfiltered it falls to a bitmap heap scan and a quicksort (0.409 ms, 109 buffers), which is fine and is
-- a different plan. One query that pretended otherwise would get the worse of the two for both.
--
-- # The cursor
--
-- `before` on the id, newest-first, the COALESCE spelling M14 measured at 9 buffers against 1,859 for the
-- OR form. An id rather than created_at: a snowflake is time-ordered *and* unique, while nothing
-- constrains created_at, so a page boundary inside a group of equal timestamps would skip or repeat a row.
--
-- # The join
--
-- LEFT, so a report whose target no longer resolves still appears — a moderator must be able to dismiss a
-- report about something that is gone. It does **not** filter `deleted_at`: 000020 says the moderation
-- surface reads deleted messages on purpose, and this is that surface.
--
-- name: ListGuildReportsByStatus :many
SELECT
  sqlc.embed(r),
  m.is_e2e AS target_is_e2e,
  m.deleted_at AS target_deleted_at
FROM reports r
LEFT JOIN messages m ON m.id = r.target_id AND r.target_type = 0
WHERE r.guild_id = $1
  AND r.status = $2
  AND r.id < COALESCE(sqlc.narg(before)::bigint, 9223372036854775807)
ORDER BY r.id DESC
LIMIT $3;

-- The same page across every status. See above for why this is a second query rather than a filter.
--
-- name: ListGuildReports :many
SELECT
  sqlc.embed(r),
  m.is_e2e AS target_is_e2e,
  m.deleted_at AS target_deleted_at
FROM reports r
LEFT JOIN messages m ON m.id = r.target_id AND r.target_type = 0
WHERE r.guild_id = $1
  AND r.id < COALESCE(sqlc.narg(before)::bigint, 9223372036854775807)
ORDER BY r.id DESC
LIMIT $2;

-- One report in full: the only place this milestone returns reported content.
--
-- Scoped by guild_id as well as id, so a report id from another guild is not reachable through this
-- guild's path — the permission check covers the guild in the route, and M15's loadInChannel established
-- that a mismatch answers 404 rather than disclosing that the id exists elsewhere.
--
-- # Rule 13, and why it is two joins rather than a CASE
--
-- `m` resolves the target and supplies the two facts about it. `mv` is the same row again behind
-- `NOT mv.is_e2e`, and it is the only thing content is read from — so an encrypted message contributes a
-- NULL content and a true `target_is_e2e`, which are different answers the service can tell apart.
--
-- The exclusion is a **join predicate rather than a CASE expression** for two reasons that happen to
-- agree. Rule 13 asks for it to be explicit and in the query, where a later reader of this join inherits
-- it instead of having to remember it — the same argument that puts the @everyone delete guard in the
-- statement. And an expression defeats sqlc's nullability inference: `CASE WHEN m.is_e2e THEN NULL ELSE
-- m.content END` generated an `interface{}`, and casting it to text generated a non-nullable `string`
-- that a NULL fails to scan into. A plain column through a LEFT JOIN types as `*string` correctly, which
-- ListGuildMemberAuthority has relied on since M12.
--
-- Today `is_e2e` is never true for a guild channel — E2E is DM-only, a DM has no guild, and this surface
-- is guild-scoped. That is precisely why the exclusion is written rather than argued: M11a's lesson is
-- that "closed by construction" stops being true quietly, and it was given a test anyway.
--
-- name: GetGuildReport :one
SELECT
  sqlc.embed(r),
  mv.content AS target_content,
  m.is_e2e AS target_is_e2e,
  m.deleted_at AS target_deleted_at,
  m.channel_id AS target_channel_id,
  m.author_id AS target_author_id
FROM reports r
LEFT JOIN messages m ON m.id = r.target_id AND r.target_type = 0
LEFT JOIN messages mv ON mv.id = m.id AND NOT mv.is_e2e
WHERE r.id = $1 AND r.guild_id = $2;

-- Close a report, with every guard in the statement.
--
-- `status = 0` is the terminal-state guard: resolved and dismissed are terminal, so a second close finds
-- no row and the service answers 409. Written here rather than as a read-then-write for the reason M10's
-- invite redemption was rewritten this way — as a check-then-update, four of four concurrent racers got
-- in. `guild_id` is in the WHERE for the cross-scope reason GetGuildReport states.
--
-- name: ResolveReport :one
UPDATE reports
SET status = $3, resolved_at = now(), resolved_by = $4
WHERE id = $1 AND guild_id = $2 AND status = 0
RETURNING *;
