-- Reverses 000004_oauth.up.sql. Indexes go with their tables, so they need no separate statements.
DROP TABLE IF EXISTS oauth_exchange_codes;
DROP TABLE IF EXISTS oauth_states;
DROP TABLE IF EXISTS oauth_identities;
