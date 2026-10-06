-- Milestone M20a — guild invites, moved here from M57 so the first client can be shared.
--
-- Until this table nothing could add a guild member except creating the guild, so two people could not
-- share one. §2 drew this table at the start and this migration is now its authority; where the two
-- differ, §2 says why beside the line.

CREATE TABLE invites (
  -- A snowflake as well as the code, which §2 did not have. The audit log names an invite by target_id,
  -- a bigint, and a reader has to be able to match `invite.create` to the `invite.revoke` and the
  -- `member.join` entries that followed it without the log carrying the code: an audit reader holds
  -- PermViewAuditLog, which is not the permission that lists live codes, and a live code is a way in.
  id         bigint PRIMARY KEY,
  -- Plaintext, for 000009's reasons: redeeming one still takes a signed-in account, so the code alone
  -- authenticates nobody, and the guild has to be shown its own codes back. Sixteen characters of M10's
  -- alphabet, about 69 bits, because an invite may never expire and be guessed at for as long as it lives.
  -- It never travels in a request path (ADR 0029).
  code       varchar(16) NOT NULL UNIQUE,
  guild_id   bigint NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
  -- Where the joiner lands, and where PermCreateInvite was resolved. Deleting the channel takes its invites
  -- with it rather than leaving codes that lead nowhere.
  channel_id bigint NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  -- No ON DELETE, matching messages.author_id: an account is soft-deleted, and a row naming who let
  -- somebody in should not lose the answer.
  inviter_id bigint NOT NULL REFERENCES users(id),
  -- NULL is unlimited. The invite is deleted in the transaction that spends its last use; every read asks
  -- about uses as well, so that deletion is tidiness rather than what keeps a spent code quiet.
  max_uses   integer NULL,
  uses       integer NOT NULL DEFAULT 0,
  -- NULL never expires. §2's max_age_seconds is left out: it restates this column as a duration, and a
  -- column that can disagree with another is one somebody reads instead of the right one.
  expires_at timestamptz NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  -- instance_invites_uses_sane's guard. Redemption's WHERE already refuses a spent invite; this makes a
  -- writer that forgot it fail rather than overrun.
  CONSTRAINT invites_uses_sane CHECK (
    uses >= 0 AND (max_uses IS NULL OR (max_uses > 0 AND uses <= max_uses)))
);
-- §2's `temporary` is not here: a membership that ends with its session needs presence (M38). Left out
-- rather than stored and ignored, which is the reversible direction.

-- The guild's listing and live-invite count, and the cascade from guilds.
CREATE INDEX invites_guild_id_idx ON invites (guild_id);

-- The cascade from channels. Without it deleting a channel scans every invite on the instance — M12's
-- foreign-key lesson.
CREATE INDEX invites_channel_id_idx ON invites (channel_id);

-- inviter_id refuses a delete rather than cascading, and a refusing foreign key still has to find the rows
-- it refuses on: M16b measured 71.5 ms against 0.135 ms per account deletion for the missing index on
-- message_audit_entries.actor_id. Accounts are soft-deleted today, so this pays when M76a asks the
-- question rather than now.
CREATE INDEX invites_inviter_id_idx ON invites (inviter_id);

-- The sweep (auth.SweepExpired) deletes expired invites. Not partial, for 000005's reason: the sweep
-- deletes by expiry alone, and an index predicated on anything else cannot serve it.
CREATE INDEX invites_expires_at_idx ON invites (expires_at);
