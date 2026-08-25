-- Milestone M10 — verifying that an address belongs to whoever registered it.

-- One row per verification request. The same shape as password_reset_tokens (M5) and for the same
-- reasons, which is deliberate: two token tables with different rules about single-use or expiry would be
-- two sets of guards to keep right.
--
-- What this table makes possible is bigger than the feature it implements. Until now this instance could
-- not verify an address itself, and that single absence is why POST /auth/register answered 409 on a taken
-- address — there was no way to accept a registration and sort it out by mail, so it had to refuse in a
-- way that disclosed whether the address was already registered. It is also why ADR 0024 had to merge its
-- two unverified-address refusals into one message, and why an account whose provider will not vouch for
-- its address could not sign in at all.
CREATE TABLE email_verification_tokens (
  id         bigint PRIMARY KEY,                                  -- snowflake (ADR 0003)
  user_id    bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,

  -- SHA-256 of the opaque token, exactly as for refresh, API and reset tokens (CLAUDE.md rule 8). The raw
  -- value exists only in the email that carried it.
  token_hash bytea NOT NULL,

  -- The address the mail went to, recorded for the same reason password_reset_tokens records it: a later
  -- change of address must not let a verification already in flight confirm the new one. Confirming
  -- compares this against the account's current email and refuses if they differ — otherwise registering
  -- with an address you control, changing it to one you do not, and then following your own link would
  -- mark somebody else's address verified.
  sent_to    text NOT NULL,

  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,

  -- Set when the link is followed. Single-use, so a leaked mail — forwarded, backed up, or read from a
  -- shared mailbox — cannot be replayed.
  consumed_at timestamptz NULL
);

-- The confirm path's only lookup. Unique because two rows sharing a hash would make it ambiguous, and a
-- duplicate can only mean a generator failure worth failing loudly on.
CREATE UNIQUE INDEX email_verification_tokens_token_hash_idx
  ON email_verification_tokens (token_hash);

-- Requesting verification again spends the account's outstanding tokens, so the newest link is the only
-- one that works. That query filters on (user_id, consumed_at IS NULL), and partial is right here for the
-- reason it is right on password_reset_tokens_user_idx: user_id is not unique on this table, so the
-- predicate genuinely narrows a scan rather than duplicating a constraint.
--
-- It also serves the ON DELETE CASCADE from users, though only for unconsumed rows; consumed ones fall
-- back to a scan. Acceptable because the sweep removes them quickly and an account deletion is rare —
-- unlike instance_invites in 000009, where the referencing table is long-lived and the index is total.
CREATE INDEX email_verification_tokens_user_idx
  ON email_verification_tokens (user_id) WHERE consumed_at IS NULL;

-- The sweep's index (auth.RunSweeper). Non-partial, which is the whole point of 000005: the sweep deletes
-- every expired row regardless of whether it was consumed, so a predicate on consumed_at would not imply
-- this index's and the planner would ignore it entirely. M5 made exactly that mistake on the reset table.
CREATE INDEX email_verification_tokens_expires_at_idx
  ON email_verification_tokens (expires_at);

-- Every account that already exists is treated as verified.
--
-- Not a convenience. From this migration on, an unverified account cannot log in, so leaving these NULL
-- would lock every existing user out of an instance that was working a minute earlier — an outage
-- delivered as a hardening. Nobody could recover on their own either: the resend path needs an account to
-- send to, and password reset needs a mail relay the instance may not have.
--
-- Safe here because email_verified_at has existed since M4 and only the OAuth path ever set it, so a NULL
-- means "nobody asked" rather than "asked and failed". An instance with real users should read this as the
-- statement it is: accounts created before verification existed are grandfathered, and only accounts
-- created afterwards are held to it.
UPDATE users SET email_verified_at = now() WHERE email_verified_at IS NULL AND deleted_at IS NULL;
