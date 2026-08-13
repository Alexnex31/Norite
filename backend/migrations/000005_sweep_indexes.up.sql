-- Milestone M6 — make the expiry sweep's index usable on password_reset_tokens.

-- The index M5 added is partial on `used_at IS NULL`, and the sweep deletes every expired row regardless of
-- whether it was spent. A partial index only serves a query whose predicate implies the index's, so the
-- planner ignored this one entirely and the sweep would have scanned the table — the exact outcome the
-- index's own comment says it exists to prevent. The same mistake was made on both OAuth tables in
-- 000004 and is corrected there in place, that migration being unreleased; this one is not, so it is fixed
-- forward.
--
-- Dropping the partial predicate makes the index cover spent rows too. That costs a little more space and
-- serves both the sweep and any future lookup by expiry, where the partial version served neither.
DROP INDEX password_reset_tokens_expires_at_idx;
CREATE INDEX password_reset_tokens_expires_at_idx ON password_reset_tokens (expires_at);
