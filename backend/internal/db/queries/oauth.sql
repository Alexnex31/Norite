-- OAuth sign-in queries.
--
-- Two tables with very different lifetimes: oauth_identities is a permanent link between an account and a
-- provider, oauth_states is a single-use row that exists for the minutes between /authorize and /callback.

-- name: CreateOAuthState :one
INSERT INTO oauth_states (id, state_hash, provider, code_verifier, expires_at)
VALUES ($1, $2, $3, $4, $5)
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
-- Called by the cleanup job (M11); until then the table's growth is bounded only by traffic.
DELETE FROM oauth_states
WHERE expires_at < now();

-- name: GetOAuthIdentity :one
-- The sign-in lookup: has this provider account been linked before? Served by the UNIQUE constraint on
-- (provider, provider_user_id), which is why that pair needs no separate index.
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
INSERT INTO oauth_exchange_codes (id, code_hash, user_id, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ConsumeOAuthExchangeCode :one
-- Single-use and expiry in the WHERE clause, so a code seen in an address bar and replayed matches zero
-- rows the second time rather than issuing a second token pair.
UPDATE oauth_exchange_codes
SET consumed_at = now()
WHERE code_hash = $1 AND consumed_at IS NULL AND expires_at > now()
RETURNING *;

-- name: DeleteExpiredOAuthExchangeCodes :execrows
DELETE FROM oauth_exchange_codes
WHERE expires_at < now();
