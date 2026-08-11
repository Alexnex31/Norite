-- Scoped API token queries.
--
-- These are the long-lived, narrow-privilege credentials bots and local automation use, as opposed to the
-- 15-minute access tokens a logged-in client holds. They are stored only as a SHA-256 hash, so the raw
-- value is recoverable exactly once — in the response that created it.

-- name: CreateAPIToken :one
INSERT INTO api_tokens (id, user_id, name, token_hash, scopes)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetAPITokenByHash :one
-- Runs on every request authenticated with an API token, which is why the hash column is indexed. Revoked
-- rows are returned rather than filtered so the caller can answer "revoked" distinctly from "unknown"
-- server-side, while still telling the client only that the credential is invalid.
SELECT * FROM api_tokens
WHERE token_hash = $1;

-- name: TouchAPIToken :exec
-- Records use. Separate from the lookup and deliberately fire-and-forget: a write on every authenticated
-- request must never be able to fail one.
UPDATE api_tokens
SET last_used_at = now()
WHERE id = $1;

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
