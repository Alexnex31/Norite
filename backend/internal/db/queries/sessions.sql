-- Refresh-session queries.
--
-- A session is one device's refresh-token family. Rotation replaces a row with a successor and links the
-- two; reuse detection walks that link. Every one of these queries is scoped by user_id AND device_id
-- wherever it revokes, because revoking across devices is precisely the bug this schema exists to prevent
-- (docs/architecture.md §2, ADR 0011).

-- name: CreateSession :one
-- first_seen is the family's start and is carried forward by rotation, never recomputed — see 000013. A
-- fresh sign-in passes its own created_at; a rotation passes the predecessor's value along.
INSERT INTO sessions (id, user_id, device_id, refresh_token_hash, device_name, ip_address, expires_at,
                      first_seen)
VALUES ($1, $2, $3, $4, $5, $6, $7, coalesce(sqlc.narg('first_seen')::timestamptz, now()))
RETURNING *;

-- name: GetSessionByRefreshTokenHash :one
-- The hot path: every refresh looks a session up by hash. Deliberately returns revoked and rotated rows
-- too — the caller must be able to tell "no such token" from "a token that was already used", since only
-- the second is a replay worth revoking a family over.
SELECT * FROM sessions
WHERE refresh_token_hash = $1;

-- name: RotateSession :one
-- Marks a session as replaced by its successor. Revoking at the same moment is what makes the old token
-- single-use: a second presentation finds revoked_at set and replaced_by_id populated, which is the replay
-- signature.
UPDATE sessions
SET revoked_at = now(), replaced_by_id = $2, last_used_at = now()
WHERE id = $1 AND revoked_at IS NULL
RETURNING *;

-- name: RevokeSession :one
UPDATE sessions
SET revoked_at = now()
WHERE id = $1 AND revoked_at IS NULL
RETURNING *;

-- name: RevokeSessionsForDevice :execrows
-- Reuse detection: revoke every live session in one device's family. Scoped by device_id as well as
-- user_id so another machine's family is untouched — a user with daemons on two computers must not be
-- logged out of one because the other replayed a token.
UPDATE sessions
SET revoked_at = now()
WHERE user_id = $1 AND device_id = $2 AND revoked_at IS NULL;

-- name: CountLiveSessionsForDevice :one
SELECT count(*) FROM sessions
WHERE user_id = $1 AND device_id = $2 AND revoked_at IS NULL AND expires_at > now();

-- name: GetSessionByID :one
-- Deliberately returns revoked and rotated rows, exactly as GetSessionByRefreshTokenHash does.
--
-- The caller that needs this is "which device is this request coming from", answered from the sid claim in
-- the access token. That claim names the session the token was minted from, and a rotation inside the
-- token's fifteen-minute life revokes that row while the token stays valid. Filtering revoked rows here
-- would make every recently-refreshed client look like it had no current device — and POST /auth/logout/all
-- would then spare nothing and log the caller out of itself.
SELECT * FROM sessions
WHERE id = $1;

-- name: RevokeAllSessionsForUser :execrows
-- Every live session for an account, across every device.
--
-- Moved here from password_reset_tokens.sql at M11, where M5 had put it because reset was its only caller.
-- It belongs to sessions now: auth.revokeEverything is what calls it, and reset is one of that primitive's
-- callers rather than its owner (CLAUDE.md rule 17).
UPDATE sessions
SET revoked_at = now()
WHERE user_id = $1 AND revoked_at IS NULL;

-- name: RevokeAllSessionsForUserExceptDevice :execrows
-- The same, sparing one device — what "sign out everywhere else" means.
--
-- The spared device is named rather than the spared session, because a session is one row of a rotating
-- family: sparing a row would leave the caller signed in only until its next refresh, which is at most
-- fifteen minutes away.
UPDATE sessions
SET revoked_at = now()
WHERE user_id = $1 AND device_id <> $2 AND revoked_at IS NULL;

-- name: ListSessionDevicesForUser :many
-- The devices signed in to an account: one row per device family, not one per session row.
--
-- A session row is one generation of a rotating family, replaced every time the client refreshes. Listing
-- rows would show somebody a new "session" every fifteen minutes and hand out ids that are stale before
-- they can be acted on, so DISTINCT ON collapses each family to its newest live row — whose id is what the
-- revoke endpoint takes.
--
-- first_seen is read straight off that row rather than aggregated over the family. It used to be
-- min(created_at) across every row including revoked ones, which was correct until 000012 started deleting
-- them: after a sweep no row older than the refresh TTL survives, so the aggregate reported a rolling
-- thirty days for a device somebody had used for a year. 000013 carries the value forward instead.
--
-- Sorted by last use in the outer query because DISTINCT ON dictates the inner ORDER BY, and "which of
-- these is the one I am still using" is the question somebody scanning this list is asking.
-- Served by sessions_live_by_device_idx (000013).
SELECT * FROM (
  SELECT DISTINCT ON (s.device_id)
    s.id,
    s.device_id,
    s.device_name,
    s.ip_address,
    s.last_used_at,
    s.expires_at,
    s.first_seen
  FROM sessions s
  WHERE s.user_id = $1 AND s.revoked_at IS NULL AND s.expires_at > now()
  ORDER BY s.device_id, s.created_at DESC
) d
ORDER BY d.last_used_at DESC;

-- name: DeleteExpiredSessions :execrows
-- Called by auth.RunSweeper, in batches. Non-partial index behind it and an index on replaced_by_id — see
-- 000012, and 000005 for why a partial one cannot serve this.
--
-- Expired only, never merely revoked, and the distinction is load-bearing. A revoked row is still
-- evidence: replaced_by_id is what lets a presented token be told apart as *replay* rather than as merely
-- unknown, and that is the signal reuse detection revokes a device family on. Deleting revoked rows while
-- their tokens could still be presented would turn a stolen token into an unrecognized one and quietly
-- disable the detection. Past expires_at nothing can be presented, so the evidence has nothing left to
-- prove.
--
-- One consequence worth naming, because it is a behavior change rather than a neutral cleanup. Refresh
-- tests revoked_at and replaced_by_id *before* it tests expiry, so today presenting a long-expired rotated
-- token is detected as replay and revokes that device's whole family. Once the row is swept the same
-- presentation is merely an unknown token and the family survives. No access is granted either way — the
-- token is unredeemable in both — but a detection that fires now stops firing. Accepted: acting on a
-- month-old unredeemable token means logging a legitimate user out of every device on that machine on the
-- strength of a signal that cannot lead anywhere.
--
-- Bounded, unlike the other six sweeps, because this table is unlike them. Theirs hold rows with TTLs of
-- minutes to an hour, so a ten-minute tick keeps them small. This one has never been swept at all: the
-- first pass on an instance running since M4 faces the entire backlog, every deleted row fires the
-- replaced_by_id RI trigger, and RunSweeper passes the process context with no deadline. Unbounded, that
-- is one statement holding a connection from a deliberately small pool (§15.3) for as long as it takes,
-- uncancellable short of shutdown. The loop in SweepExpired calls this until it returns less than the
-- limit, so a large backlog is drained across several bounded statements instead of one unbounded one.
DELETE FROM sessions
WHERE id IN (
  SELECT id FROM sessions WHERE expires_at < now() LIMIT $1
);
