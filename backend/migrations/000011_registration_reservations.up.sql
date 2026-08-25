-- Milestone M10 — closing the last leg of the registration oracle.

-- A username claimed by a registration attempt that created no account.
--
-- # Why this table exists at all
--
-- POST /auth/register answers 202 identically whether or not the address already has an account, which is
-- what M10 set out to deliver. That holds for one request. It did not hold for two, and the reason is that
-- the two branches left *different state* rather than different responses:
--
--   * a free address commits a users row, which permanently occupies the submitted username;
--   * a taken address rolls its transaction back and occupies nothing.
--
-- A taken username is still reported plainly — 409, deliberately, because a username is a public @handle.
-- So the username namespace was a read of "did the previous request create a row?", which is the address
-- answer. Two unauthenticated requests, no race, no timing:
--
--   register(U, victim@example.com) -> 202 always
--   register(U, attacker@evil.test) -> 409 means the first call created an account, so the victim's
--                                      address was free; 202 means it created nothing, so it was taken.
--
-- Found by a security review of this milestone, and reproduced against a real instance in both
-- registration modes. Note it cost nothing on a gated instance either: the rollback leaves the invite
-- unspent, so probing addresses that are already registered was free and unlimited.
--
-- The fix has to equalize the *state*, because no ordering of the checks helps — whichever branch creates
-- a row is the branch that occupies the name. So the branch that creates nothing writes here instead, and
-- both leave the username unavailable.
CREATE TABLE registration_reservations (
  -- citext, matching users.username, so a reservation and an account cannot disagree about what "the same
  -- username" means. Without it `Ada` could be reserved while `ada` stayed free, and the oracle would come
  -- back through the difference.
  username   citext PRIMARY KEY,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- # Why there is no expires_at, and what has to change if that ever stops being true
--
-- A reservation must last exactly as long as the thing it stands in for. What it stands in for is an
-- unverified users row, and nothing in this system reaps one of those — an account registered and never
-- confirmed sits there forever. So a reservation that expired would reopen the oracle on the far side of
-- its own TTL: probe again after it lapses, and 202-versus-409 answers the same question it did before.
--
-- Giving unverified accounts a lifetime is the better end state, and it would let this table have one too.
-- **They move together or not at all.** A future migration that sweeps unverified accounts must give this
-- table the same clock in the same commit, and one that adds a TTL here without touching accounts would
-- silently undo this whole table.
--
-- Unbounded growth from an unauthenticated endpoint is the obvious objection, and it is already true of
-- what this mirrors: an attacker could always occupy usernames without limit by registering with fresh
-- addresses, which writes a heavier row than this one. This adds no capability; it removes an asymmetry.
CREATE INDEX registration_reservations_created_at_idx ON registration_reservations (created_at);
