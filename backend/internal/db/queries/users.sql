-- Account queries.
--
-- Every read filters `deleted_at IS NULL`: a soft-deleted account keeps its row so authored content can
-- still render as "Deleted User", but it must never be findable for login, registration collision, or
-- profile lookup. Leaving that filter off is the way a deleted account quietly becomes usable again.

-- name: CreateUser :one
-- email_verified_at is a parameter rather than a default, because the three callers disagree about it and
-- each is right.
--
-- Registration passes NULL: the address is a claim until somebody follows a link sent to it. Bootstrap
-- passes now(), because the operator proved filesystem access to the instance's own config, which is
-- strictly more than an emailed link proves — asking them to check their mail to finish setting up a
-- server they are holding the keys to would be theatre, and would make bootstrap impossible on an
-- instance with no relay. The OAuth path has its own insert (CreateOAuthUser) because it creates an
-- account with no password at all.
INSERT INTO users (id, username, email, password_hash, display_name, email_verified_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetUserByID :one
SELECT * FROM users
WHERE id = $1 AND deleted_at IS NULL;

-- name: GetUserByEmail :one
SELECT * FROM users
WHERE email = $1 AND deleted_at IS NULL;

-- name: UserExistsByEmail :one
SELECT EXISTS (
  SELECT 1 FROM users WHERE email = $1 AND deleted_at IS NULL
) AS taken;

-- name: UsernameUnavailable :one
-- Is this username claimed, by an account or by a registration that created none?
--
-- The second half is what closes the last leg of the registration oracle, and it is not optional: a
-- registration against a *taken* address creates no account, so without a reservation it would leave the
-- username free while one against a fresh address does not — and two requests read that difference. See
-- migration 000011.
--
-- One query rather than two so the two halves cannot be checked in different places, or one of them
-- forgotten by a later caller.
SELECT EXISTS (
  SELECT 1 FROM users u WHERE u.username = $1 AND u.deleted_at IS NULL
  UNION ALL
  SELECT 1 FROM registration_reservations r WHERE r.username = $1
) AS unavailable;

-- name: ReserveUsername :exec
-- Claims a username for a registration that created no account.
--
-- ON CONFLICT DO NOTHING because the name may already be claimed by an account or by an earlier
-- reservation, and either way the caller's answer is the same: it is not available. Nothing here needs to
-- know which.
INSERT INTO registration_reservations (username)
VALUES ($1)
ON CONFLICT (username) DO NOTHING;
