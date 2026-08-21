-- OAuth sign-in queries.
--
-- Two tables with very different lifetimes: oauth_identities is a permanent link between an account and a
-- provider, oauth_states is a single-use row that exists for the minutes between /authorize and /callback.

-- name: CreateOAuthState :one
-- client_redirect_uri is '' for a flow with nowhere to return to — a browser, and the device-code path.
-- It is written once here and only ever read back out of the row ConsumeOAuthState spends, which is what
-- keeps the destination a property of the flow rather than of whoever presents the callback.
INSERT INTO oauth_states (
  id, state_hash, provider, code_verifier, flow_challenge, expires_at, client_redirect_uri
)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ConsumeOAuthState :one
-- Spends a state, with single-use and expiry both in the WHERE clause rather than in Go.
--
-- A callback replayed — by a user refreshing the page, or by someone who captured the redirect — matches
-- zero rows the second time and is failed before any token exchange happens. Reading the row, checking
-- consumed_at in the service, then updating would let two exchanges run against one verifier, which is the
-- single-use property PKCE depends on.
UPDATE oauth_states
SET consumed_at = now()
WHERE state_hash = $1 AND consumed_at IS NULL AND expires_at > now()
RETURNING *;

-- name: DeleteExpiredOAuthStates :execrows
-- Abandoned flows are the common case: opening the provider page and closing the tab leaves a row behind.
-- Called by auth.RunSweeper. Without it the table grows for the life of the instance, and it is written
-- by an unauthenticated endpoint, so nothing but the rate limiter bounds that.
DELETE FROM oauth_states
WHERE expires_at < now();

-- name: GetOAuthIdentity :one
-- The sign-in lookup: has this provider account been linked before, to an account that still exists?
--
-- The join is the load-bearing part, and its absence was a real hole. A soft-deleted account keeps its
-- rows so authored content still renders as "Deleted User" — including its oauth_identities row — so a
-- lookup on the identity alone let a deleted account sign straight back in and collect a token pair, while
-- password login and API tokens both refused it. Same reasoning and same shape as
-- GetActiveAPITokenByHash's join.
--
-- Served by the UNIQUE constraint on (provider, provider_user_id) plus the users primary key, so neither
-- needs a separate index.
SELECT i.* FROM oauth_identities i
JOIN users u ON u.id = i.user_id
WHERE i.provider = $1 AND i.provider_user_id = $2
  AND u.deleted_at IS NULL;

-- name: GetOAuthIdentityIncludingDeleted :one
-- The same lookup as GetOAuthIdentity, deliberately without the liveness join.
--
-- Used only after a unique violation, to find out which of oauth_identities' two constraints was hit and
-- what it means. GetOAuthIdentity hides rows belonging to soft-deleted accounts, which is correct for
-- signing in and exactly wrong here: a hidden row is still a row, and it is the reason the INSERT failed.
SELECT * FROM oauth_identities
WHERE provider = $1 AND provider_user_id = $2;

-- name: CreateOAuthIdentity :one
INSERT INTO oauth_identities (id, user_id, provider, provider_user_id, email)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: CreateOAuthUser :one
-- An account created by an OAuth sign-in, with no password.
--
-- password_hash is left NULL rather than set to an empty string, so an account that can only sign in
-- through a provider is distinguishable from one with a password — the distinction VerifyPassword and the
-- reset path both already depend on.
INSERT INTO users (id, username, email, display_name, email_verified_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: CreateOAuthExchangeCode :one
INSERT INTO oauth_exchange_codes (id, code_hash, user_id, flow_challenge, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ConsumeOAuthExchangeCode :one
-- Single-use and expiry in the WHERE clause, so a code seen in an address bar and replayed matches zero
-- rows the second time rather than issuing a second token pair.
UPDATE oauth_exchange_codes
SET consumed_at = now()
WHERE code_hash = $1 AND consumed_at IS NULL AND expires_at > now()
RETURNING *;

-- name: RevokeOAuthExchangeCodesForUser :execrows
-- Part of revoking everything a compromised credential could reach (CLAUDE.md rule 17).
--
-- An outstanding exchange code is not a session, so revoking sessions and API tokens leaves it redeemable
-- — and it is the one credential in this flow that gets rendered on screen. Without this, resetting a
-- password to lock an intruder out still leaves them a code they can trade for a fresh token pair.
--
-- No index on user_id, on purpose: the table only ever holds sign-ins from the last couple of minutes that
-- nobody has redeemed yet, so a scan here is cheaper than the write this would add to every sign-in.
UPDATE oauth_exchange_codes
SET consumed_at = now()
WHERE user_id = $1 AND consumed_at IS NULL;

-- name: DeleteExpiredOAuthExchangeCodes :execrows
DELETE FROM oauth_exchange_codes
WHERE expires_at < now();
