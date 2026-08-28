-- Password-reset token queries.
--
-- A token is single-use and short-lived, and every one of these statements is written so that property
-- holds in SQL rather than in whichever Go path remembered to check it.

-- name: CreatePasswordResetToken :one
INSERT INTO password_reset_tokens (id, user_id, token_hash, sent_to, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetPasswordResetTokenByHash :one
-- The confirm path's only lookup. Deliberately returns spent and expired rows too: the caller needs to
-- tell "no such token" from "already used" for its own logging, even though both are reported to the
-- client identically.
SELECT * FROM password_reset_tokens
WHERE token_hash = $1;

-- name: ConsumePasswordResetToken :one
-- Spends a token, and does the single-use check in the WHERE clause rather than in Go.
--
-- Two confirms racing on the same token both reach this statement; the second finds used_at already set,
-- matches zero rows, and is failed. Checking `used_at IS NULL` in the service and updating afterwards
-- would let both win and would make the "single-use" promise a comment rather than a guarantee.
UPDATE password_reset_tokens
SET used_at = now()
WHERE id = $1 AND used_at IS NULL AND expires_at > now()
RETURNING *;

-- name: InvalidateOutstandingResetTokens :execrows
-- Requesting a new reset spends every older one for that account, so the most recent link is the only one
-- that works. Without this, every request an anxious user makes leaves another live token behind, and the
-- window a leaked one is redeemable in becomes the union of all of them.
UPDATE password_reset_tokens
SET used_at = now()
WHERE user_id = $1 AND used_at IS NULL;

-- name: DeleteExpiredPasswordResetTokens :execrows
-- Removes spent and expired reset tokens. Run on a schedule by auth.RunSweeper.
--
-- Both are dead: ConsumePasswordResetToken already refuses anything expired or used, so nothing here is
-- reachable and the rows are only taking space. Served by password_reset_tokens_expires_at_idx, made
-- non-partial in 000005 for exactly this query.
DELETE FROM password_reset_tokens
WHERE expires_at < now();

-- name: SetUserPassword :one
UPDATE users
SET password_hash = $2, updated_at = now()
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;
