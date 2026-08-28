-- Second-factor queries: one TOTP enrollment per account, and its recovery codes.
--
-- Single-use lives in the statement here as it does everywhere else in this package — see
-- ConsumeRecoveryCode below, and password_reset_tokens.sql for the reasoning it shares.

-- name: UpsertTOTPEnrollment :one
-- Begin (or restart) enrollment. Unconfirmed until a code proves the authenticator works.
--
-- ON CONFLICT replaces rather than refusing, because the case it serves is somebody who lost the QR code
-- half-way and started again. Confirming is what makes a factor real, so replacing an *unconfirmed* row
-- costs nothing — and replacing a confirmed one is prevented by the caller, which requires the current
-- factor before it will touch an account that already has one.
INSERT INTO user_totp (user_id, secret_encrypted)
VALUES ($1, $2)
ON CONFLICT (user_id) DO UPDATE
SET secret_encrypted = EXCLUDED.secret_encrypted, confirmed_at = NULL, created_at = now()
RETURNING *;

-- name: GetTOTPForUser :one
-- Deliberately returns unconfirmed rows. "Is a factor owed" and "is an enrollment in progress" are
-- different questions and the caller asks both; a query that hid unconfirmed rows would make the second
-- unanswerable and silently let a second enrollment overwrite one in flight.
SELECT * FROM user_totp WHERE user_id = $1;

-- name: ConfirmTOTP :one
-- The moment the factor becomes required. Guarded on still being unconfirmed so a replayed confirmation
-- cannot move the timestamp, which is the same single-transition discipline the device flow uses.
UPDATE user_totp
SET confirmed_at = now()
WHERE user_id = $1 AND confirmed_at IS NULL
RETURNING *;

-- name: DeleteTOTPForUser :execrows
DELETE FROM user_totp WHERE user_id = $1;

-- name: CreateRecoveryCode :one
INSERT INTO user_recovery_codes (id, user_id, code_hash)
VALUES ($1, $2, $3)
RETURNING *;

-- name: ConsumeRecoveryCode :one
-- Spend one code, exactly once.
--
-- Every guard is in the WHERE: the code must belong to this account, and must not already be spent. Two
-- requests presenting the same code both reach this statement and Postgres serializes them on the row, so
-- the second re-evaluates `used_at IS NULL` against the first's committed value and matches nothing. A
-- read-then-update in Go would let both through, which is the shape RedeemInstanceInvite was rewritten out
-- of at M10 after four of four concurrent racers got in.
--
-- Scoped by user_id as well as the hash even though the hash is unique, for the reason every revoking
-- query in sessions.sql is scoped by device: a lookup that can only ever match this account's rows cannot
-- be made to act on another's by a collision or a future schema change.
UPDATE user_recovery_codes
SET used_at = now()
WHERE user_id = $1 AND code_hash = $2 AND used_at IS NULL
RETURNING *;

-- name: DeleteRecoveryCodesForUser :execrows
-- Used when the whole set is replaced or the factor is disabled. Deletes rather than marking spent: these
-- are not evidence of anything once the factor they belonged to is gone.
DELETE FROM user_recovery_codes WHERE user_id = $1;

-- name: CountLiveRecoveryCodes :one
-- Served by user_recovery_codes_live_idx (000014). Read by the profile response and by the regenerate
-- path, so it scales with the codes an account has left rather than with every set it has ever had.
SELECT count(*) FROM user_recovery_codes WHERE user_id = $1 AND used_at IS NULL;
