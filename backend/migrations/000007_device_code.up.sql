-- Milestone M9 — the device-code flow, for a machine with no browser of its own.

-- An in-flight device authorization: a CLI asks for a code, a person completes the sign-in in a browser
-- on some *other* machine, and the CLI polls until there is something to collect.
--
-- The shape here deliberately differs from docs/architecture.md §2's sketch of this table in three ways,
-- each of which that sketch got wrong against a rule this repository does not bend. The doc is corrected
-- in the same milestone; the reasons are recorded here because this file is what a future reader will
-- diff against it.
--
--   1. The device code is stored as a hash, not in plaintext. It is redeemable for a token pair, which
--      makes it a credential, and every credential in this codebase is stored only as its SHA-256
--      (CLAUDE.md rule 8). The sketch had `code varchar(16) PRIMARY KEY` holding the raw value.
--   2. There is no status column. A smallint carrying "pending/completed/expired" duplicates facts the
--      timestamps already hold — expiry is `expires_at`, and a second copy of it can disagree with the
--      first. The rest of this schema states outcomes as nullable timestamps and so does this.
--   3. There is no session_id. The sketch had approval create the session, which cannot work: a session
--      is scoped to one device_id (docs/architecture.md §2) and the browser doing the approving has no
--      idea what the waiting CLI's is, and the raw refresh token could not be kept here to hand over
--      later anyway. The row records *who* approved; the session is minted at poll time, by the request
--      that carries the device identity, exactly as ExchangeOAuthCode does.
CREATE TABLE device_codes (
  id               bigint PRIMARY KEY,                                  -- snowflake (ADR 0003)

  -- SHA-256 of the `nod_…` value only the waiting client holds. This is the credential the poll spends.
  device_code_hash bytea NOT NULL,
  -- The short code a person reads off their terminal and types into a phone. In plaintext, which is the
  -- one credential-shaped value in this schema that is not hashed, and the exception is deliberate.
  --
  -- It is not a bearer credential: holding it authorizes nothing. Whoever enters it must then authenticate
  -- as themselves and press Approve, and what that authorizes is somebody else's machine to act as *their*
  -- account — so a stolen user code buys an attacker the ability to give their own account away. The
  -- device code beside it is the credential, and that one is hashed.
  --
  -- And it has to be readable back: the approval page shows it so a person can compare it against the
  -- screen that produced it, which is the only comparison standing between them and authorizing a device
  -- they were talked into. A hash cannot serve that, and the page is where this flow's real defense is.
  user_code        varchar(9) NOT NULL,

  -- Captured when the code is issued, because the client that will hold the resulting session is the one
  -- that asked for the code — not the browser that approves it. device_name is shown on the approval
  -- page so a person can see what they are authorizing, which is the only defense this flow has against
  -- being talked into approving somebody else's device.
  device_id        text NOT NULL,
  device_name      text NOT NULL,

  -- Set when somebody signs in on the verification page and approves. NULL means still waiting, which is
  -- what the poll reports as authorization_pending.
  user_id          bigint NULL REFERENCES users(id) ON DELETE CASCADE,
  -- Set when they press Deny instead. Distinct from an expiry, so a waiting client can stop at once
  -- rather than polling out its full life for an answer that already exists.
  denied_at        timestamptz NULL,
  -- Set when the code is spent for a token pair. Single-use lives in ConsumeDeviceCode's WHERE clause.
  consumed_at      timestamptz NULL,
  -- The last poll, so a client polling faster than the documented interval can be told to slow down
  -- (RFC 8628 §3.5). Advisory rather than a security boundary — the rate limiter is what actually bounds
  -- this endpoint — so it is written on every poll and compared in Go.
  last_polled_at   timestamptz NULL,

  created_at       timestamptz NOT NULL DEFAULT now(),
  expires_at       timestamptz NOT NULL
);

-- The poll's only lookup. Unique for the same reason as oauth_states_state_hash_idx: two rows sharing a
-- hash would make it ambiguous, and a duplicate can only mean a generator failure.
CREATE UNIQUE INDEX device_codes_device_code_hash_idx ON device_codes (device_code_hash);

-- The verification page's only lookup, and the constraint that makes collision handling correct rather
-- than hopeful. A user code is short by construction — a person types it — so two live codes colliding is
-- rare but not negligible, and the generator retries on the unique violation this raises rather than
-- checking first and racing.
CREATE UNIQUE INDEX device_codes_user_code_idx ON device_codes (user_code);

-- The sweep's index (auth.RunSweeper). Abandoned authorizations are the common case here, more so than
-- for oauth_states: a person who runs `norite login` on a server and gets distracted leaves a row behind,
-- and the endpoint that creates it is unauthenticated.
--
-- Non-partial, for the reason 000005 exists: the sweep deletes every expired row regardless of whether it
-- was approved, denied or spent, so a predicate on any of those columns would not imply this index's and
-- the planner would ignore it entirely.
CREATE INDEX device_codes_expires_at_idx ON device_codes (expires_at);

-- Which device authorization an in-flight provider sign-in belongs to, or NULL for the flows that existed
-- before this milestone.
--
-- The verification page offers Google and GitHub as well as a password, and the provider round trip is a
-- redirect out to a third party and back — so the only thing that can carry the destination across it is
-- this row, the same problem client_redirect_uri solved in 000006 and the same answer. The callback reads
-- it out of the state it consumes and never from its own URL, which is what stops whoever presents the
-- callback from choosing which device gets authorized.
--
-- No query in this codebase filters on it — it is read out of the row ConsumeOAuthState already found by
-- state_hash — but it still needs an index, and the reason is the one an unindexed foreign key always
-- has: ON DELETE CASCADE. Deleting a device_codes row makes Postgres look for referencing rows here, and
-- with no index that is a sequential scan of oauth_states *per deleted row*. The sweep deletes in
-- batches, so it is the pathological shape for it.
--
-- Measured rather than assumed, on 20,000 rows in each table: without this index the sweep's own scan
-- takes 0.17 ms and the cascade trigger takes 279 ms for 400 deletions. With it, the trigger drops to
-- 4 ms. The first version of this comment claimed no index was needed and was wrong.
--
-- Partial, and here that is correct where 000005 says it usually is not. The sweep's predicate is on
-- expires_at and implies nothing about consumed_at, which is why *that* index cannot be partial; this
-- one's only reader is the cascade, whose predicate is `device_code_id = X` and therefore implies
-- `IS NOT NULL`. Nearly every row in this table has a NULL here — only a device flow sets it — so the
-- partial version indexes a small fraction of them and stays out of the way of every other write.
ALTER TABLE oauth_states ADD COLUMN device_code_id bigint NULL REFERENCES device_codes(id) ON DELETE CASCADE;
CREATE INDEX oauth_states_device_code_id_idx ON oauth_states (device_code_id)
  WHERE device_code_id IS NOT NULL;
