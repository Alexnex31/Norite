-- Carry the device's first sign-in forward, so the sweep cannot quietly cap it.
--
-- first_seen was derived: min(created_at) over the family's rows. That is correct until something deletes
-- rows, and 000012 — one migration earlier, in the same milestone — started deleting them. Since every
-- row's expires_at is its created_at plus RefreshTokenTTL, no row older than thirty days survives a sweep,
-- so min(created_at) can never report earlier than thirty days ago. A laptop signed in for a year reported
-- "first seen" as a rolling month.
--
-- Measured before this migration: a family seeded 395 days back reported 2025-07-28 correctly, and
-- 2026-08-26 — that same day — immediately after one sweep.
--
-- The irony is the point. ListSessionDevicesForUser's own comment justifies its correlated subquery by
-- saying the newest row's created_at "would tell a user they signed in fifteen minutes ago on a machine
-- they have used for a month". The sweep reintroduced exactly that lie at a coarser scale, and the two
-- halves shipped in adjacent parts of one milestone.
--
-- Denormalized rather than derived, because the value has to outlive the rows it came from. It is copied
-- onto each successor at rotation, exactly as device_id, device_name and ip_address already are.
ALTER TABLE sessions ADD COLUMN first_seen timestamptz NOT NULL DEFAULT now();

-- Backfill from what each family still knows. On an instance that has never swept this is exact; on one
-- that has, it is the best available answer and no worse than what the old query returned.
UPDATE sessions s
SET first_seen = f.started
FROM (
  SELECT user_id, device_id, min(created_at) AS started
  FROM sessions
  GROUP BY user_id, device_id
) f
WHERE f.user_id = s.user_id AND f.device_id = s.device_id;

-- Serves ListSessionDevicesForUser, which is the one query M11 added and did not measure — rule 7 says
-- the index ships in the same migration as the query, and 000012 measured the sweep and the FK trigger
-- while this one went out on whatever (user_id, device_id) happened to offer.
--
-- Measured against three devices with thirty days of rotations behind them, 8,640 rows, 3 live:
--
--                                without          with
--   ListSessionDevicesForUser    0.703 ms         0.033 ms
--   buffers                      164              8
--
-- Small in absolute terms, and the shape is the point: without it the scan is bounded by the account's
-- retained history, with it by the number of devices. History is what this table accumulates between
-- sweeps.
--
-- Partial on live rows because that is exactly what the listing selects, and on a rotation chain the live
-- rows are a vanishing fraction of the whole: roughly one per device against ninety-six a day.
CREATE INDEX sessions_live_by_device_idx ON sessions (user_id, device_id, created_at DESC)
  WHERE revoked_at IS NULL;
