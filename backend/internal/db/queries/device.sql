-- Device-code flow queries (Milestone M9).
--
-- One table with a short life and a small state machine: issued, then approved or denied by a browser
-- somewhere else, then spent exactly once by the client that has been polling for it. Every transition
-- that must happen at most once is expressed as a WHERE clause here rather than as a check in Go, so two
-- concurrent attempts produce one winner without anything in the service having to remember to look.

-- name: CreateDeviceCode :one
-- device_id and device_name come from the client asking for the code, which is the client that will hold
-- the resulting session. The browser that approves never supplies either.
INSERT INTO device_codes (
  id, device_code_hash, user_code, device_id, device_name, expires_at
)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetDeviceCodeByUserCode :one
-- The verification page's lookup, for a code that can still be acted on.
--
-- Approved, denied, spent and expired rows are all excluded, so all four produce the same "no such code"
-- as a code that never existed. That is worth having beyond tidiness: a code entered a second time gets
-- told the authorization is over, instead of being walked through a sign-in that would then fail at the
-- approval step for a reason nobody could see.
SELECT * FROM device_codes
WHERE user_code = $1
  AND user_id IS NULL
  AND denied_at IS NULL
  AND consumed_at IS NULL
  AND expires_at > now();

-- name: ApproveDeviceCode :one
-- Records who authorized this device.
--
-- `user_id IS NULL` is what makes an approval single-use, and it is the reason the approval token needs
-- no store of its own: replaying one matches zero rows. The remaining conditions mean an approval racing
-- a denial or an expiry loses rather than overwriting it.
UPDATE device_codes
SET user_id = $2
WHERE id = $1
  AND user_id IS NULL
  AND denied_at IS NULL
  AND consumed_at IS NULL
  AND expires_at > now()
RETURNING *;

-- name: DenyDeviceCode :one
-- The other half of the approval page, and the reason it is worth a column rather than letting the code
-- expire: a person who realizes they were sent a code by someone else can end the authorization now, and
-- the waiting client stops immediately instead of polling for another twenty minutes.
UPDATE device_codes
SET denied_at = now()
WHERE id = $1
  AND user_id IS NULL
  AND denied_at IS NULL
  AND consumed_at IS NULL
  AND expires_at > now()
RETURNING *;

-- name: PollDeviceCode :one
-- One poll: records that it happened and reports the state as it was *before* it did.
--
-- The CTE is what makes the previous poll time readable at all. RETURNING sees the updated row, so
-- last_polled_at there is always now() and always useless; `previous` is evaluated against the same
-- statement snapshot and therefore holds the value from the poll before this one. That is what the
-- slow_down decision is made against.
--
-- The row is touched even when the poll came too soon, deliberately: a client hammering every second must
-- keep being told to slow down, and only updating on well-behaved polls would let the first one reset the
-- clock forever.
--
-- Expired and already-spent rows match nothing, which the service reports as one answer.
WITH previous AS (
  SELECT dc.id, dc.last_polled_at
  FROM device_codes dc
  WHERE dc.device_code_hash = $1
    AND dc.consumed_at IS NULL
    AND dc.expires_at > now()
)
UPDATE device_codes d
SET last_polled_at = now()
FROM previous
WHERE d.id = previous.id
RETURNING d.*, previous.last_polled_at AS previous_polled_at;

-- name: ConsumeDeviceCode :one
-- Spends an approved code for the one token pair it is worth.
--
-- Single-use in the WHERE clause, for the reason ConsumeOAuthExchangeCode gives: two processes holding
-- the same device code both reach here, and exactly one matches a row. Reading, checking consumed_at in
-- Go, then updating would issue two sessions for one authorization.
UPDATE device_codes
SET consumed_at = now()
WHERE device_code_hash = $1
  AND user_id IS NOT NULL
  AND denied_at IS NULL
  AND consumed_at IS NULL
  AND expires_at > now()
RETURNING *;

-- name: RevokeDeviceCodesForUser :execrows
-- Part of revoking everything a compromised credential could reach (CLAUDE.md rule 17).
--
-- An approved-but-unpolled device code is exactly the same kind of outstanding claim on an account as an
-- OAuth exchange code, and RevokeOAuthExchangeCodesForUser exists for that reason — this one closes the
-- same hole on the path M9 adds. Without it, a password reset performed to lock an intruder out leaves
-- them a code that still trades for a fresh token pair.
--
-- Rows still waiting for approval have a NULL user_id and belong to nobody yet, so they are correctly
-- outside this. No index on user_id, for the reason RevokeOAuthExchangeCodesForUser gives: the table
-- holds minutes of unfinished sign-ins, so a scan is cheaper than a write on every issuance.
UPDATE device_codes
SET consumed_at = now()
WHERE user_id = $1 AND consumed_at IS NULL;

-- name: DeleteExpiredDeviceCodes :execrows
-- Called by auth.RunSweeper. Abandoned authorizations are the common case, and the endpoint that creates
-- them is unauthenticated, so nothing but the rate limiter bounds how fast this table grows.
DELETE FROM device_codes
WHERE expires_at < now();

-- name: GetDeviceCodeByID :one
-- The verification page's re-read between steps.
--
-- By id because that is what the signed continuation carries: the device code never reaches the browser at
-- all, and the user code is what a person types, so putting either back into a value the browser holds
-- would give a page something to replay. Same liveness conditions as the lookup by user code, so an
-- authorization that expired or was decided while somebody was typing is caught at every step rather than
-- only at the first.
SELECT * FROM device_codes
WHERE id = $1
  AND user_id IS NULL
  AND denied_at IS NULL
  AND consumed_at IS NULL
  AND expires_at > now();

-- name: RevokeApprovedDeviceCode :one
-- Takes back an approval that has not been collected yet.
--
-- This is what makes Deny a real recovery path rather than a promise. Somebody who approves and realizes a
-- second later — the likeliest way anybody escapes the phishing this flow is vulnerable to — presses Deny
-- and lands here, because DenyDeviceCode by then matches nothing. Spending the code is the strongest thing
-- still available: the waiting client's next poll gets expired_token and no session is ever created.
--
-- Scoped to the account the approval token names, so an approval for one account cannot revoke another's.
-- Matches nothing once the code has been redeemed, which is the one case where this is too late and the
-- caller has to say so.
UPDATE device_codes
SET consumed_at = now()
WHERE id = $1 AND user_id = $2 AND consumed_at IS NULL
RETURNING *;
