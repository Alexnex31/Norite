-- Health-check queries.
--
-- These exist so the readiness endpoint validates the *whole* data path — pool checkout, the
-- sqlc-generated call, the round trip to Postgres — rather than only the socket. A pool that has a live
-- TCP connection but cannot actually execute a statement (exhausted, wedged on a failover, blocked by a
-- broken search_path) is not ready, and a bare ping would report it as healthy.

-- name: Ping :one
SELECT 1::int AS ok;
