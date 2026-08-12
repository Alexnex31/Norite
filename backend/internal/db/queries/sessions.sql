-- Refresh-session queries.
--
-- A session is one device's refresh-token family. Rotation replaces a row with a successor and links the
-- two; reuse detection walks that link. Every one of these queries is scoped by user_id AND device_id
-- wherever it revokes, because revoking across devices is precisely the bug this schema exists to prevent
-- (docs/architecture.md §2, ADR 0011).

-- name: CreateSession :one
INSERT INTO sessions (id, user_id, device_id, refresh_token_hash, device_name, ip_address, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
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
