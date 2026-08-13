-- Milestone M5 — password reset tokens.
--
-- One row per reset request. Single-use and short-lived: the token is the entire proof of identity for
-- changing a password, so the window in which a leaked one is worth anything has to be small.

CREATE TABLE password_reset_tokens (
  id         bigint PRIMARY KEY,                                  -- snowflake (ADR 0003)
  user_id    bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  -- SHA-256 of the opaque 256-bit token, exactly as for refresh and API tokens (CLAUDE.md rule 8). The
  -- raw value exists only in the email that carried it.
  token_hash bytea NOT NULL,
  -- The address the email was sent to, recorded so a later change of address cannot silently redirect a
  -- reset already in flight: confirm compares this against the account's current email and refuses if
  -- they differ.
  sent_to    text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  -- Set when the token is spent. Single-use is what stops a leaked email — forwarded, backed up, or read
  -- from a shared mailbox weeks later — from being redeemable a second time.
  used_at    timestamptz NULL
);

-- Confirm looks a token up by hash, which is the only lookup on the request path. Unique because two rows
-- sharing a hash would make that lookup ambiguous, and a duplicate can only mean a generator failure worth
-- failing loudly on — the same reasoning as sessions_refresh_token_hash_idx.
CREATE UNIQUE INDEX password_reset_tokens_token_hash_idx ON password_reset_tokens (token_hash);

-- Requesting a reset invalidates the account's outstanding tokens, so the newest link is the only one that
-- works. That query filters on (user_id, used_at IS NULL).
--
-- Partial here, unlike the pair removed from `users` in M4: user_id is *not* unique on this table, so the
-- index genuinely narrows a scan rather than duplicating a constraint that already resolves to one row.
CREATE INDEX password_reset_tokens_user_idx ON password_reset_tokens (user_id) WHERE used_at IS NULL;

-- Expired rows are pruned on a schedule by auth.RunSweeper, which this index exists to keep off a
-- sequential scan. NOTE: as written here the index is partial and the sweep's predicate does not imply it,
-- so the planner ignored it entirely — corrected in 000005, which this comment is left beside deliberately
-- rather than rewritten, since editing a released migration would not change any database that ran it.
CREATE INDEX password_reset_tokens_expires_at_idx ON password_reset_tokens (expires_at) WHERE used_at IS NULL;
