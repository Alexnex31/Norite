-- Back to the pre-sweep indexes.
--
-- The partial predicate is restored exactly as 000002 wrote it, so a down-then-up round trip lands on the
-- same schema rather than a subtly different one.
DROP INDEX sessions_replaced_by_id_idx;
DROP INDEX sessions_expires_at_idx;
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at) WHERE revoked_at IS NULL;
