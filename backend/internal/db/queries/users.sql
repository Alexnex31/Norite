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

-- name: UserExistsByUsername :one
SELECT EXISTS (
  SELECT 1 FROM users WHERE username = $1 AND deleted_at IS NULL
) AS taken;
