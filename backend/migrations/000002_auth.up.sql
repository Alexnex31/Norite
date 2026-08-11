-- Milestone M4 — the auth core: accounts, device-scoped refresh sessions, scoped API tokens.
--
-- Schema follows docs/architecture.md §2 ("Full DDL") exactly. Tables the auth surface will need later
-- (oauth_identities at M6, password_reset_tokens at M5, instance_invites at M10) are deliberately NOT
-- created here — each arrives with the milestone that reads it, so a migration never adds a table nothing
-- can yet use.

-- citext gives case-insensitive uniqueness for usernames and emails without every query having to remember
-- lower(). Doing that in application code instead is the classic way two accounts end up differing only by
-- capitalisation, which is both a login-confusion and an impersonation problem.
CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE users (
  id                bigint PRIMARY KEY,           -- snowflake (ADR 0003), never a serial
  username          citext UNIQUE NOT NULL,
  email             citext UNIQUE NOT NULL,
  -- Nullable: an account created through OAuth alone (M6) has no password, and must not be given an empty
  -- string that some later comparison could treat as one.
  password_hash     text NULL,
  display_name      text NOT NULL,
  avatar_hash       text NULL,
  email_verified_at timestamptz NULL,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  -- Soft delete: authored content stays and renders as "Deleted User" (docs/architecture.md, account
  -- lifecycle). Every lookup must therefore filter on this being null.
  deleted_at        timestamptz NULL
);

-- Login and registration both look accounts up by email, and every such lookup excludes soft-deleted rows.
-- The UNIQUE constraints above cover the full column, but not the "and not deleted" shape these queries
-- actually use, so the partial indexes are what those hit (CLAUDE.md rule 7).
CREATE INDEX users_active_email_idx ON users (email) WHERE deleted_at IS NULL;
CREATE INDEX users_active_username_idx ON users (username) WHERE deleted_at IS NULL;

CREATE TABLE sessions (
  id                 bigint PRIMARY KEY,
  user_id            bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  -- Stable per daemon install. This is what scopes a refresh-token family: rotation and reuse-detection
  -- both act within one device_id and never across them, so a user running daemons on two machines under
  -- one account cannot have one machine's activity log the other out (ADR 0011).
  device_id          text NOT NULL,
  -- SHA-256 of the opaque 256-bit refresh token. The raw value exists only in the response that issued it
  -- and in the client's keychain — never here, never in a log (CLAUDE.md rule 8).
  refresh_token_hash bytea NOT NULL,
  device_name        text NULL,
  ip_address         inet NULL,
  created_at         timestamptz NOT NULL DEFAULT now(),
  last_used_at       timestamptz NOT NULL DEFAULT now(),
  expires_at         timestamptz NOT NULL,
  revoked_at         timestamptz NULL,
  -- The rotation chain. Set when this session is rotated away from, so presenting an already-rotated token
  -- is detectable as replay rather than merely failing to match.
  replaced_by_id     bigint NULL REFERENCES sessions(id) ON DELETE SET NULL
);

-- The refresh path looks a session up by token hash on every rotation, which is the hottest query this
-- table has. Unique because two sessions sharing a hash would make that lookup ambiguous, and a duplicate
-- can only mean a generator failure worth failing loudly on.
CREATE UNIQUE INDEX sessions_refresh_token_hash_idx ON sessions (refresh_token_hash);
-- Reuse detection revokes one device's whole chain, and "list my sessions" (M11) reads per user.
CREATE INDEX sessions_user_device_idx ON sessions (user_id, device_id);
-- Expired-session cleanup scans by expiry.
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at) WHERE revoked_at IS NULL;

CREATE TABLE api_tokens (
  id           bigint PRIMARY KEY,
  user_id      bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         text NOT NULL,
  -- SHA-256, exactly as for refresh tokens above, and for the same reason.
  token_hash   bytea NOT NULL,
  -- Named scopes, e.g. {identify,messages:send}. An empty array is a token that can do nothing, which is
  -- valid and safe; NULL would be ambiguous, so the column is NOT NULL.
  scopes       text[] NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz NULL,
  revoked_at   timestamptz NULL
);

-- Authenticating a request with an API token is a lookup by hash on every single request, so it gets a
-- unique index for the same reasons as the session one above.
CREATE UNIQUE INDEX api_tokens_token_hash_idx ON api_tokens (token_hash);
-- Listing and revoking a user's own tokens.
CREATE INDEX api_tokens_user_idx ON api_tokens (user_id) WHERE revoked_at IS NULL;
