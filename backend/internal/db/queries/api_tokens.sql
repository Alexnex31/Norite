-- Scoped API token queries.
--
-- These are the long-lived, narrow-privilege credentials bots and local automation use, as opposed to the
-- 15-minute access tokens a logged-in client holds. They are stored only as a SHA-256 hash, so the raw
-- value is recoverable exactly once — in the response that created it.

-- name: CreateAPIToken :one
INSERT INTO api_tokens (id, user_id, name, token_hash, scopes)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetActiveAPITokenByHash :one
-- Runs on every request authenticated with an API token, which is why the hash column is indexed.
--
-- One statement, not three: the owning account's liveness is joined in rather than fetched separately, and
-- the revocation and soft-delete filters are applied here rather than in Go. An unusable token therefore
-- returns no rows whatever the reason — which is also what the client is told, so nothing is lost by not
-- distinguishing them.
SELECT t.* FROM api_tokens t
JOIN users u ON u.id = t.user_id
WHERE t.token_hash = $1
  AND t.revoked_at IS NULL
  AND u.deleted_at IS NULL;

-- name: TouchAPIToken :exec
-- Records use, at most once every few minutes per token.
--
-- Writing on every authenticated request would put a row update — and its WAL traffic, and its dead tuple
-- for autovacuum — on the hottest read path in the API, to keep a timestamp accurate to the second that
-- nothing needs to the second. The staleness window is the whole optimization: "last used" is for an
-- operator auditing an account's tokens, and five minutes is well inside what that reader cares about.
--
-- Still fire-and-forget: bookkeeping must never be able to fail an otherwise-valid request.
UPDATE api_tokens
SET last_used_at = now()
WHERE id = $1
  AND (last_used_at IS NULL OR last_used_at < now() - interval '5 minutes');

-- name: ListAPITokensForUser :many
SELECT * FROM api_tokens
WHERE user_id = $1 AND revoked_at IS NULL
ORDER BY id DESC;

-- name: RevokeAPIToken :one
-- Scoped by user_id as well as id: an actor may only revoke their own tokens, and enforcing that in the
-- statement means a handler cannot forget to check ownership (CLAUDE.md rule 1).
UPDATE api_tokens
SET revoked_at = now()
WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL
RETURNING *;

-- name: RevokeAllAPITokensForUser :execrows
-- Every API token the account holds.
--
-- Moved here from password_reset_tokens.sql at M11, for the reason RevokeAllSessionsForUser gives. The
-- argument for revoking these alongside sessions moved with it, onto auth.revokeEverything.
UPDATE api_tokens
SET revoked_at = now()
WHERE user_id = $1 AND revoked_at IS NULL;
