-- Restores M5's partial index. Reversible, though the version it returns to is the one the sweep cannot
-- use — see the up migration.
DROP INDEX password_reset_tokens_expires_at_idx;
CREATE INDEX password_reset_tokens_expires_at_idx ON password_reset_tokens (expires_at) WHERE used_at IS NULL;
