// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/guildauth"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// TransferOwnership hands a guild to another of its members (M13a).
//
// # Why it exists
//
// M12 made deletion owner-only and refuses to remove the owner by any route, both deliberately — and
// together they left a guild whose owner has gone stuck: nobody could leave it or delete it but an
// Instance Admin, and on a self-hosted instance with no active admin, nobody at all. This widens nothing.
// Deletion stays owner-only and undelegable; the owner gets somewhere to hand the guild to.
//
// # Who may
//
// The owner, read off the guild row locked in this transaction — **not** Decision.Owns(), which is false
// for an Instance Admin who happens to own the guild (M16b's finding). And an Instance Admin, by layer 1,
// because a guild whose owner is gone is the case that motivates the milestone. Rule 14 wants that recorded
// in `instance_audit_log`, which is M72's; until then the guild's own log is the record, the same position
// M16 took for closing a report (see the M72 entry and TestAnInstanceAdminsCloseIsRecordedOnlyInTheGuildLog).
//
// A PermManageGuild or PermAdministrator holder may not. Ownership is layer 2, which no permission reaches
// — the reason deletion is the owner's too.
//
// The route also requires a user actor with a live session. That lives in the router rather than here,
// because it is a property of the credential rather than of the guild.
//
// # To whom
//
// A current member whose account is not deleted, who is not already the owner, and who is below the
// owned-guild ceiling. A non-member answers 404 — the same as a guild that does not exist, so a list of
// user ids cannot be sorted into "in this guild" and "not". The ceiling is checked after membership for the
// same reason: it describes an account, and is nobody's business until that account is known to be here.
//
// **The recipient is not asked.** Decided at M13a's planning, and in the ledger with what would reopen it:
// an offer-and-accept flow is a second endpoint and a table with a TTL, and the ceiling refusal already
// stops a transfer exceeding it.
//
// # The race with a kick
//
// GetGuildForNoKeyUpdate here and GetGuildForShare in RemoveMember conflict, so the two serialize on the
// guild row; the statement's own guards (still the owner, recipient still a member) make whichever loses
// the race match nothing rather than write a guild owned by a non-member.
func (s *Service) TransferOwnership(
	ctx context.Context, actor auth.Actor, guildID, to snowflake.ID,
) (Guild, error) {
	var out Guild

	err := s.inTx(ctx, func(q *db.Queries) error {
		// PermViewChannel is what a non-member fails, so a stranger gets the ordinary 404 rather than a
		// 403 that would confirm the guild exists — Delete's opening, for Delete's reason.
		allowed, err := guildauth.Authorize(ctx, q, actor, guildID, 0, roles.PermViewChannel)
		if err != nil {
			return err
		}

		// Two reads of the owner: unlocked to refuse, then locked to act.
		//
		// The first is what keeps a caller who will be refused from taking any lock. It was one locked read
		// at first, FOR UPDATE, so any member could hold the guild's exclusive lock by posting a transfer
		// they would be refused — and FOR UPDATE blocks the key-share lock every insert into a child table
		// takes, so the guild's sends waited behind it. Found by /code-review;
		// TestARefusedCallerTakesNoLockOnTheGuild.
		unlocked, err := q.GetGuild(ctx, int64(guildID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: get guild: %w", err)
		}
		if snowflake.ID(unlocked.OwnerID) != actor.UserID && !allowed.InstanceAdmin() {
			return httpx.Errorf(httpx.ErrForbidden, "only the guild's owner may transfer it")
		}

		// The second holds the row, FOR NO KEY UPDATE (see the query), and is the owner this acts on: a
		// transfer that committed between the two reads moved it, and the caller is asked again.
		guild, err := q.GetGuildForNoKeyUpdate(ctx, int64(guildID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: get guild: %w", err)
		}
		from := snowflake.ID(guild.OwnerID)

		if from != actor.UserID && !allowed.InstanceAdmin() {
			return httpx.Errorf(httpx.ErrForbidden, "only the guild's owner may transfer it")
		}

		if to == from {
			return httpx.Errorf(httpx.ErrBadRequest, "that member already owns this guild")
		}

		live, err := q.IsLiveGuildMember(ctx, db.IsLiveGuildMemberParams{
			GuildID: int64(guildID), UserID: int64(to),
		})
		if err != nil {
			return fmt.Errorf("guilds: check recipient: %w", err)
		}
		if !live {
			return httpx.ErrNotFound
		}

		// The ceiling Create enforces, applied to the account receiving a guild, which would otherwise be
		// the one way past it — and counted under the account's ownership lock, which Create takes too.
		// This guild's row lock serializes nothing across guilds, so without it two transfers to one
		// account from different guilds both counted it below the ceiling and both committed. Found by
		// /code-review on this branch; TestTwoTransfersToOneAccountCannotBothPassTheCeiling.
		if err := q.LockAccountOwnership(ctx, int64(to)); err != nil {
			return fmt.Errorf("guilds: lock recipient's ownership: %w", err)
		}
		owned, err := q.CountGuildsOwnedBy(ctx, int64(to))
		if err != nil {
			return fmt.Errorf("guilds: count recipient's owned guilds: %w", err)
		}
		if owned >= int64(s.maxGuildsPerAccount) {
			return httpx.Errorf(ErrGuildFull,
				"that member already owns the most guilds an account may own (%d)", s.maxGuildsPerAccount)
		}

		row, err := q.TransferGuildOwnership(ctx, db.TransferGuildOwnershipParams{
			GuildID: int64(guildID), FromOwner: int64(from), ToOwner: int64(to),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// A guard in the statement refused: the recipient left or was removed after the check
				// above, or their account was deleted. The owner cannot have changed — this transaction
				// holds the row — so the answer is the recipient's, and it is the one above.
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: transfer ownership: %w", err)
		}

		changes := auditDiff{}
		changes.changed("owner_id", from, to)
		if err := s.writeAudit(
			ctx, q, guildID, actor.UserID, ActionGuildOwnerTransfer, &guildID, changes.payload(),
		); err != nil {
			return err
		}

		out = guildFromRow(row)
		return nil
	})
	return out, err
}
