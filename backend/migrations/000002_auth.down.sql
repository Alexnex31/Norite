-- Reverses 000002_auth.up.sql.
--
-- Dropped in reverse dependency order: api_tokens and sessions both reference users, so users goes last.
-- Indexes go with their tables, so they need no separate statements.
DROP TABLE IF EXISTS api_tokens;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;

-- citext is deliberately NOT dropped. Extensions are database-wide rather than owned by this migration, and
-- another schema in the same database may be using it; dropping it would break them. Leaving it costs
-- nothing, and re-running the up migration is a no-op thanks to IF NOT EXISTS.
