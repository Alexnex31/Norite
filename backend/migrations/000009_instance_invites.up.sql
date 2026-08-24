-- Milestone M10 — gating account creation on an invite.

-- An instance invite: the thing that makes registration_mode = "invite" mean something.
--
-- Distinct from the per-guild `invites` table, which admits an existing account to a guild. This one
-- gates account creation itself, so it is the only thing standing between a private instance and anybody
-- who can reach it. M4 wired the mode and, with no table to redeem against, made it a hard refusal —
-- deliberately, so an operator who gated registration got gating rather than silence. This is the other
-- half, four milestones late.
--
-- The shape is docs/architecture.md §2's, with two deviations, each recorded here because this file is
-- what a future reader will diff against it.
--
--   1. created_by is nullable. The doc has it NOT NULL, which cannot hold: an invite may be created by
--      the instance operator, who is not an account and has no id to record. See the column.
--   2. The code is stored in plaintext. Every other credential-shaped value in this schema is a SHA-256,
--      and the exception is deliberate — see the column for why, and why the blast radius is small
--      enough to make it the right call rather than a convenient one.
CREATE TABLE instance_invites (
  -- The code itself, in plaintext, and the primary key.
  --
  -- The reasoning is M9's user_code exception applied to a different value, and it has two halves.
  --
  -- It has to be readable back. An invite exists to be handed to somebody, and an administrator who made
  -- three of them last week and wants to re-send one has nowhere else to get it — a list that cannot show
  -- its own contents is not a list. Hashing would make this a show-once credential like an api_token,
  -- which is right for a value its owner *holds* and wrong for one they *distribute*.
  --
  -- And what it authorizes is bounded in a way a token is not: presenting one lets somebody create an
  -- account subject to every other rule registration enforces. It reaches no existing account, no guild,
  -- and no data. A leaked invite costs an unwanted signup on a private instance, which is recoverable by
  -- deleting the row; a leaked refresh token costs the account, which is not.
  --
  -- varchar(16) holds the generated length exactly. Sixteen characters of the twenty-letter alphabet
  -- auth.GenerateUserCode draws from is about 69 bits — far more than the 34.6 a device code carries,
  -- because this one has no twenty-minute life bounding how long it can be guessed at.
  code       varchar(16) PRIMARY KEY,

  -- Who issued it, or NULL when the instance operator did.
  --
  -- The doc has this NOT NULL and it cannot be: instance administration is reachable by an operator token,
  -- which proves possession of the instance's config file and names no account (see auth/operator.go).
  -- The first invite on a new instance is very often exactly that case — an operator setting up a private
  -- instance before anybody else has an account to invite from.
  --
  -- No ON DELETE, so this defaults to NO ACTION and an account cannot be hard-deleted while the record of
  -- what it issued survives. Same reasoning as instance_admins.granted_by: accounts are soft-deleted here,
  -- so nothing in the product hits it, and durable provenance is the behaviour to have if anything does.
  created_by bigint NULL REFERENCES users(id),

  -- NULL means unlimited. An open-ended invite is a real thing to want — a link in a group chat that
  -- everybody in it may use — and expressing it as a very large number would be a lie a query has to
  -- remember to interpret.
  max_uses   integer NULL,
  uses       integer NOT NULL DEFAULT 0,

  -- NULL means it never expires. The sweep below deletes on this column, so it deliberately skips these.
  expires_at timestamptz NULL,
  created_at timestamptz NOT NULL DEFAULT now(),

  -- The invariant the redemption UPDATE relies on, stated where the database can hold it rather than only
  -- in the WHERE clause that maintains it. A negative max_uses would make an invite that can never be
  -- redeemed while looking like one that can; a uses count above its own ceiling can only mean the
  -- redemption query stopped being atomic, and it is worth finding that out at the write rather than
  -- discovering it as an over-subscribed instance.
  CONSTRAINT instance_invites_uses_sane CHECK (
    uses >= 0 AND (max_uses IS NULL OR (max_uses > 0 AND uses <= max_uses))
  )
);

-- The sweep's index (auth.RunSweeper), and non-partial for the reason 000005 records: the sweep deletes
-- every expired row regardless of how used it was, so a predicate on uses or max_uses would not imply
-- this index's and the planner would ignore it entirely.
--
-- Note the sweep must not touch NULL expires_at, which is what "never expires" is stored as. A NULL sorts
-- into neither side of the `expires_at < now()` comparison, so that falls out of SQL's own semantics
-- rather than needing a guard — but it is the kind of thing a later "tidy up the sweep" rewrite breaks,
-- so the query says it explicitly too.
CREATE INDEX instance_invites_expires_at_idx ON instance_invites (expires_at);

-- The foreign key's index, and the reason an unindexed one is a problem even without ON DELETE CASCADE:
-- NO ACTION still makes Postgres look for referencing rows when a users row is deleted, and with no index
-- that is a sequential scan of this table per deleted row.
--
-- Measured on this table rather than inherited from 000007's note, with 20,000 users and 20,000 invites,
-- deleting 400 unreferenced users:
--
--   without this index   instance_invites_created_by_fkey trigger  284 ms   (total 312 ms)
--   with it                                                         17 ms   (total  69 ms)
--
-- The comparison also settles 000008's opposite call: instance_admins.granted_by is deliberately
-- unindexed, and its trigger costs 11 ms across the same 400 deletions, because an instance has a handful
-- of administrators where it may have thousands of invites.
--
-- Partial, which 000005 says is usually wrong and here is right. That index's reader is a sweep whose
-- predicate is on a different column; this one's is the foreign-key check, whose predicate is
-- `created_by = X` and therefore implies IS NOT NULL. Operator-created invites carry NULL here and stay
-- out of the index entirely.
CREATE INDEX instance_invites_created_by_idx ON instance_invites (created_by)
  WHERE created_by IS NOT NULL;
