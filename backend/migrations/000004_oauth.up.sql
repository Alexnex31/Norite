-- Milestone M6 — OAuth sign-in with Google and GitHub.

-- Which provider accounts are linked to which Norite account. DDL per docs/architecture.md §2.
CREATE TABLE oauth_identities (
  id               bigint PRIMARY KEY,                                  -- snowflake (ADR 0003)
  user_id          bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider         varchar(32) NOT NULL,
  provider_user_id varchar(255) NOT NULL,
  -- The address the provider reported at link time. Recorded rather than relied on: it is what the
  -- account was linked *by*, which is worth being able to see later, but the account's own email is the
  -- one that matters and either can change independently afterwards.
  email            text NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),

  -- One Norite account per provider identity: without this, two accounts could both claim the same Google
  -- user, and a sign-in would have no defensible way to choose between them. This is also the index the
  -- sign-in path looks up by, so it needs no companion.
  UNIQUE (provider, provider_user_id),
  -- ...and one identity per provider per account, so "link Google" is idempotent rather than accumulating
  -- rows nothing distinguishes.
  UNIQUE (user_id, provider)
);

-- In-flight authorization requests: the bridge between /authorize and /callback.
--
-- This table is NOT in architecture.md's DDL and is a deliberate addition, for one reason: PKCE requires
-- the code verifier to be known to this server and to nobody else. The obvious stateless alternative —
-- packing it into the `state` parameter — sends it through the user's browser and the provider, which is
-- exactly the disclosure PKCE exists to prevent, so it would leave the mechanism in place and its value at
-- zero. The verifier therefore has to live server-side, and that means a row.
--
-- Rows are short-lived and single-use. Nothing here survives a completed sign-in.
CREATE TABLE oauth_states (
  id            bigint PRIMARY KEY,
  -- SHA-256 of the opaque state value, exactly as for every other credential this codebase hands out and
  -- takes back (CLAUDE.md rule 8). The raw value exists only in the redirect URL and in whatever the
  -- provider echoes back.
  state_hash    bytea NOT NULL,
  provider      varchar(32) NOT NULL,
  -- The PKCE code verifier, necessarily in plaintext: it is sent to the provider at exchange time, so a
  -- hash would be useless. It is the one value here that is genuinely secret, and the reason this table's
  -- rows are deleted rather than kept for history.
  code_verifier text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  -- Set when the state is spent, which is what makes a callback replay fail rather than start a second
  -- exchange with the same verifier.
  consumed_at   timestamptz NULL
);

-- The callback's only lookup. Unique for the same reason as sessions_refresh_token_hash_idx: two rows
-- sharing a hash would make it ambiguous, and a duplicate can only mean a generator failure.
CREATE UNIQUE INDEX oauth_states_state_hash_idx ON oauth_states (state_hash);

-- Abandoned flows — a user who opens the provider page and closes the tab — are the common case, so the
-- sweep that clears them (M11's cleanup job) needs to find them without scanning the table.
--
-- Deliberately not partial on `consumed_at IS NULL`, which is what it was first written as. The sweep
-- deletes every expired row regardless of whether it was spent, so its predicate does not imply a partial
-- index's, and the planner would have ignored the index entirely — leaving a sequential scan behind an
-- index whose stated purpose was to prevent one.
CREATE INDEX oauth_states_expires_at_idx ON oauth_states (expires_at);

-- The bridge between a completed callback and a client that wants tokens.
--
-- The callback cannot hand a token pair to a browser: a redirect would put credentials in a URL, and so
-- in history, in a Referer, and in every log between here and there. So it issues a one-time code
-- instead, and the client trades it at POST /auth/oauth/exchange for the pair — which is what that
-- endpoint, documented in architecture.md §2 long before this milestone, exists for.
--
-- The same shape serves the CLI's loopback flow at M8: the code is the only thing that crosses the
-- browser, and it is worthless without a second request.
CREATE TABLE oauth_exchange_codes (
  id          bigint PRIMARY KEY,
  -- SHA-256, like every other value handed out and taken back.
  code_hash   bytea NOT NULL,
  user_id     bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at  timestamptz NOT NULL DEFAULT now(),
  -- Minutes, not hours. The client redeems this immediately; anything longer is a window for whoever saw
  -- the address bar over someone's shoulder.
  expires_at  timestamptz NOT NULL,
  consumed_at timestamptz NULL
);

CREATE UNIQUE INDEX oauth_exchange_codes_code_hash_idx ON oauth_exchange_codes (code_hash);
-- Full, not partial, for the same reason as oauth_states_expires_at_idx above.
CREATE INDEX oauth_exchange_codes_expires_at_idx ON oauth_exchange_codes (expires_at);
