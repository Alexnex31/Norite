-- Account queries.
--
-- Every read filters `deleted_at IS NULL`: a soft-deleted account keeps its row so authored content can
-- still render as "Deleted User", but it must never be findable for login, registration collision, or
-- profile lookup. Leaving that filter off is the way a deleted account quietly becomes usable again.

-- name: CreateUser :one
INSERT INTO users (id, username, email, password_hash, display_name)
VALUES ($1, $2, $3, $4, $5)
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
