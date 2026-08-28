-- The second factor: one TOTP authenticator per account, and the recovery codes that outlive losing it.
--
-- Decisions in docs/adr/0031-two-factor-authentication.md. The shape below is the one §2 sketched when the
-- milestone was scheduled; what this migration adds is the reasoning that belongs next to the columns.

CREATE TABLE user_totp (
  -- The account, and the primary key. One authenticator per account by construction rather than by a
  -- uniqueness check somebody has to remember: a second enrollment replaces the first, and "which of my two
  -- authenticators is live" is a question nobody should have to answer.
  user_id          bigint PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  -- Encrypted, not hashed, and it is the only credential in this schema that is.
  --
  -- Everything else here — passwords, refresh tokens, API tokens, reset tokens, recovery codes below — is
  -- stored as a hash because nothing ever needs the original back. Verifying a TOTP code requires
  -- recomputing HMAC over the shared secret, so the secret has to survive in recoverable form. Storing it
  -- bare would make a single database read a permanent bypass of the factor, so it is sealed with
  -- AES-256-GCM under a key derived from the instance signing key (auth.sealTOTPSecret).
  --
  -- The consequence is stated rather than smoothed over: a database compromise that *also* yields
  -- instance.toml yields every enrolled secret. The alternative is an HSM or a KMS, which is infrastructure
  -- a self-hosted instance does not have.
  secret_encrypted bytea NOT NULL,
  -- NULL until the first correct code proves the authenticator actually works.
  --
  -- This is what stops an abandoned enrollment locking somebody out. Between "show me a QR code" and "here
  -- is a code from it" a person can close the tab, mistype the secret, or find their phone's clock is
  -- wrong — and an account that demanded a factor from that moment would be unreachable. An unconfirmed
  -- row is not a factor; auth.factorSatisfied ignores it.
  confirmed_at     timestamptz NULL,
  created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE user_recovery_codes (
  id         bigint PRIMARY KEY,                                  -- snowflake (ADR 0003)
  user_id    bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  -- SHA-256 of a high-entropy code, exactly as for every other opaque credential here (rule 8). The raw
  -- values exist only in the response that generated them, printed once.
  --
  -- UNIQUE because the hash is what a redemption looks up, and because two identical codes across two
  -- accounts would be a generator that had stopped being random.
  code_hash  text NOT NULL UNIQUE,
  -- Spent rather than deleted. A used code stays as evidence, the way a revoked session row does: it is
  -- the difference between "that code was already used" and "that code never existed", and only the first
  -- is worth telling an operator reading a log.
  used_at    timestamptz NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- Serves the "how many codes has this account got left" read, which the profile response and the
-- regenerate path both make. Partial on unused, because that is the only count anybody asks for and spent
-- codes accumulate ten at a time for the life of the account.
--
-- Measured with EXPLAIN (ANALYZE, BUFFERS) on Postgres 16, and the first measurement was wrong in a way
-- worth recording. Against one account with 10 live codes among 200 spent, the index looked pointless:
-- 0.028 ms against 0.032 ms, two buffers either way. That is because this table holds *every* account's
-- codes, so what a sequential scan crosses is the whole instance's history and a single-account fixture
-- cannot see it. Re-seeded with 10,000 accounts and 100,000 codes:
--
--                              with the index            without
--   count for one account      0.025 ms, 3 buffers       2.631 ms, 836 buffers
--                              Index Only Scan           Seq Scan, 100,200 rows removed by filter
--
-- A hundredfold, on a read GET /users/@me makes. The lesson generalizes past this index: a per-account
-- query benchmarked against a database containing one account measures nothing.
CREATE INDEX user_recovery_codes_live_idx ON user_recovery_codes (user_id) WHERE used_at IS NULL;

-- Neither table is swept, and that is deliberate rather than an omission.
--
-- Every other table this package has added carries a TTL and a delete in auth.SweepExpired: reset tokens,
-- OAuth states, exchange codes, device codes, instance invites, verification tokens, and sessions since
-- M11. These two do not. A factor is as durable as the account it protects, and a spent recovery code is
-- evidence with no expiry — there is nothing for a sweep to decide. Written down because "which tables the
-- sweeper knows about" is precisely the question M11 discovered nobody had been asking, and the answer for
-- a new table should be a decision rather than a silence.
