-- Instance-administration queries.
--
-- The Instance Admin tier, which is instance-wide and sits outside roles.Resolve entirely (ADR 0013).
-- M10 creates the first row and reads it; M71 adds granting, revoking, and the last-admin safety rail.

-- name: CountInstanceAdmins :one
-- The bootstrap guard, and the reason it is a count rather than an existence check.
--
-- POST /instance/bootstrap is authorized by an operator token, which is minted from the instance signing
-- key by anyone holding the config file. That is the right authority for creating the *first* admin and
-- the wrong one for creating a second — an operator token replayed later must not be able to add an
-- account to this table quietly. So the endpoint refuses unless this answers 0, which makes bootstrap
-- self-disabling rather than needing a flag somewhere that says whether it already ran.
--
-- Read inside the same transaction as the insert, so two simultaneous bootstraps cannot both see zero.
SELECT count(*) FROM instance_admins;

-- name: IsInstanceAdmin :one
-- The tier check, on every request to an instance-administration endpoint.
--
-- Joined against users for liveness, the same shape and the same reasoning as GetActiveAPITokenByHash
-- and GetOAuthIdentity: a soft-deleted account keeps its rows so its authored content still renders as
-- "Deleted User", and this row is one of them. Without the join, deleting an admin's account would leave
-- their tier intact and usable by any credential still outstanding on it.
SELECT EXISTS (
  SELECT 1 FROM instance_admins a
  JOIN users u ON u.id = a.user_id
  WHERE a.user_id = $1 AND u.deleted_at IS NULL
) AS is_admin;

-- name: CreateInstanceAdmin :one
-- granted_by is NULL for the bootstrap admin: nobody in this table granted it. See 000008.
INSERT INTO instance_admins (user_id, granted_by)
VALUES ($1, $2)
RETURNING *;
