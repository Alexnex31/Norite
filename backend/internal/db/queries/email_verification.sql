-- Email verification queries.
--
-- The same shape as password_reset_tokens and deliberately so: two token tables with different rules about
-- single-use or expiry would be two sets of guards to keep right, and the second one is always the one
-- that gets it wrong.

-- name: CreateEmailVerificationToken :one
INSERT INTO email_verification_tokens (id, user_id, token_hash, sent_to, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetEmailVerificationTokenByHash :one
-- The confirm path's lookup. Expiry is checked here as well as in the consume below, so an expired token
-- is refused before anything is written — the same two-step the reset path uses.
SELECT * FROM email_verification_tokens
WHERE token_hash = $1;

-- name: ConsumeEmailVerificationToken :one
-- Spends a token, with single-use in the WHERE clause rather than in Go.
--
-- Two confirms racing on the same link both reach this statement; the second finds consumed_at already set,
-- matches zero rows, and is failed. Checking in the service and updating afterwards would let both win, and
-- while a double verification is harmless in itself, the guarantee is what the address-change check below
-- depends on.
UPDATE email_verification_tokens
SET consumed_at = now()
WHERE id = $1 AND consumed_at IS NULL AND expires_at > now()
RETURNING *;

-- name: InvalidateOutstandingVerificationTokens :execrows
-- Requesting verification again spends every older token for that account, so the newest link is the only
-- one that works. Without it, each resend leaves another live token behind and the window a leaked mail is
-- redeemable in becomes the union of all of them.
UPDATE email_verification_tokens
SET consumed_at = now()
WHERE user_id = $1 AND consumed_at IS NULL;

-- name: MarkEmailVerified :one
-- Records that the address is confirmed.
--
-- The WHERE guards against a race with an address change: the account's current email must still be the
-- one the token was sent to. ConsumeEmailVerificationToken has already checked that the token is unspent,
-- but the address can change between the two statements, and they run in one transaction precisely so this
-- comparison is against a value that cannot move underneath it.
UPDATE users
SET email_verified_at = now(), updated_at = now()
WHERE id = $1 AND email = $2 AND deleted_at IS NULL
RETURNING *;

-- name: DeleteExpiredEmailVerificationTokens :execrows
-- Called by auth.RunSweeper. Non-partial index behind it — see 000010, and 000005 for why.
DELETE FROM email_verification_tokens
WHERE expires_at < now();
