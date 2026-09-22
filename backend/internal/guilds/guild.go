// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/guildauth"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// defaultEveryonePermissions is what @everyone gets in a newly created guild.
//
// Viewing, reading the backlog, sending, connecting to voice, speaking and adding a reaction — the set a
// person joining a social space expects to work without anybody configuring anything. Deliberately not
// PermCreateInvite: who may bring others in is the first decision an owner should make on purpose rather
// than discover.
//
// PermReadMessageHistory joins the default at M15 rather than being left to the owner, because the
// alternative is a guild where every new channel reads as empty until somebody finds the bit. Discord
// grants it by default for the same reason. Withholding it is the deliberate configuration — a support
// thread whose earlier conversation is not for whoever was added last.
const defaultEveryonePermissions = roles.PermViewChannel |
	roles.PermReadMessageHistory |
	roles.PermSendMessages |
	roles.PermConnectVoice |
	roles.PermSpeakVoice

// CreateGuildInput is the request to create a guild.
type CreateGuildInput struct {
	Name        string
	Description *string
}

// Create makes a guild, its @everyone role and the owner's membership, in one transaction.
//
// # No permission check, and why that is not a gap
//
// This is the one mutation in the package that calls neither authorize nor anything like it, because
// there is no guild yet to resolve permissions against. Anyone who can authenticate may create a guild —
// which is the same rule as every comparable platform, and the reason instance-wide abuse controls
// (M67a's registration challenge, and whatever caps follow) sit at account creation rather than here.
//
// # Why all three rows share a transaction
//
// A guild with no @everyone role has no permission floor: roles.Resolve would find no default role, and
// every member would resolve to zero permissions until somebody noticed. A guild with no owner membership
// is worse in a subtler way — the owner still bypasses every check by layer 2, so nothing would look
// broken until the member list came back without them. Neither state is one a client can repair, so
// neither is a state this code can produce.
func (s *Service) Create(ctx context.Context, actor auth.Actor, in CreateGuildInput) (Guild, error) {
	guildID, err := s.ids.Next()
	if err != nil {
		return Guild{}, fmt.Errorf("guilds: mint guild id: %w", err)
	}
	roleID, err := s.ids.Next()
	if err != nil {
		return Guild{}, fmt.Errorf("guilds: mint role id: %w", err)
	}

	var out Guild

	err = s.inTx(ctx, func(q *db.Queries) error {
		// The ceiling. No permission is checked on this path, so this is the only bound on it — see
		// Service.maxGuildsPerAccount. Racy under READ COMMITTED in the same way and for the same reason the
		// channel and role ceilings are, and acceptable for the same reason: the consequence is one guild
		// over a soft limit, not a corrupted ordering.
		owned, err := q.CountGuildsOwnedBy(ctx, int64(actor.UserID))
		if err != nil {
			return fmt.Errorf("guilds: count owned guilds: %w", err)
		}
		if owned >= int64(s.maxGuildsPerAccount) {
			return httpx.Errorf(ErrGuildFull,
				"an account may own at most %d guilds", s.maxGuildsPerAccount)
		}

		row, err := q.CreateGuild(ctx, db.CreateGuildParams{
			ID:          int64(guildID),
			Name:        in.Name,
			OwnerID:     int64(actor.UserID),
			Description: in.Description,
		})
		if err != nil {
			return fmt.Errorf("guilds: create guild: %w", err)
		}

		// Position 0 and is_default. The default role is the floor every other role stacks on, so it sits
		// at the bottom by construction rather than by whoever created it choosing well.
		if _, err := q.CreateRole(ctx, db.CreateRoleParams{
			ID:          int64(roleID),
			GuildID:     int64(guildID),
			Name:        "@everyone",
			Permissions: defaultEveryonePermissions.Int64(),
			Position:    0,
			Mentionable: true,
			IsDefault:   true,
		}); err != nil {
			return fmt.Errorf("guilds: create default role: %w", err)
		}

		if _, err := q.AddGuildMember(ctx, db.AddGuildMemberParams{
			GuildID: int64(guildID),
			UserID:  int64(actor.UserID),
		}); err != nil {
			// AddGuildMember deliberately carries no ON CONFLICT clause, so a duplicate membership arrives
			// as a unique violation rather than as a silent no-op — see its comment. Translating it here is
			// what makes ErrAlreadyAMember reachable: without this the sentinel is declared, mapped to a
			// 400 in writeErr, and returned by nothing, so a collision would answer 500 with a shape the
			// contract does not declare. Unreachable through this path, since the guild id is freshly
			// minted; wired up because M13's join endpoint is the caller that will meet it.
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
				return ErrAlreadyAMember
			}
			return fmt.Errorf("guilds: add owner membership: %w", err)
		}

		// Rule 2. The only audit entry in this package describing something that did not exist when the
		// request arrived, which is why its target is the guild itself.
		changes := auditDiff{}
		changes.created("name", in.Name)
		if in.Description != nil {
			changes.created("description", *in.Description)
		}

		if err := s.writeAudit(
			ctx, q, guildID, actor.UserID, ActionGuildCreate, &guildID, changes.payload(),
		); err != nil {
			return err
		}

		out = guildFromRow(row)
		return nil
	})
	if err != nil {
		return Guild{}, err
	}

	return out, nil
}

// Get returns one guild.
//
// Authorized with PermViewChannel, which every member holds by default — so in practice this asks "are
// you in this guild", and answers 404 when you are not. That is the same refusal a guild that does not
// exist gets, deliberately; see authorize.
func (s *Service) Get(ctx context.Context, actor auth.Actor, guildID snowflake.ID) (Guild, error) {
	if err := s.authorize(ctx, actor, guildID, 0, roles.PermViewChannel); err != nil {
		return Guild{}, err
	}

	row, err := s.queries.GetGuild(ctx, int64(guildID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Unreachable in practice — authorize already resolved the guild — but a race with a
			// concurrent delete is real, and the answer to it is the same 404 a non-member gets.
			return Guild{}, httpx.ErrNotFound
		}
		return Guild{}, fmt.Errorf("guilds: get guild: %w", err)
	}

	return guildFromRow(row), nil
}

// UpdateGuildInput is a partial update. A nil field is left alone.
type UpdateGuildInput struct {
	Name        *string
	Description *string
	// ClearDescription distinguishes "remove the description" from "do not touch it", which a nil pointer
	// alone cannot express — see UpdateGuild in guilds.sql.
	ClearDescription bool
	// MessageAuditEnabled is M16b's recording switch, and it is the one field on this struct that its
	// holder needs more than PermManageGuild to set. See Service.Update.
	MessageAuditEnabled *bool
}

// mayFlipMessageAudit reports whether this actor may turn a guild's message recording on or off.
//
// # Why this is a function and not `existing.OwnerID == actor.UserID` at the call site
//
// Because it departs from every other authority check in this package in two ways at once, and an
// unexplained inequality reads as a caller that forgot the tier — which is the exact thing Decision's
// four helpers (Allows, Outranks, AllowsInChannel, OutranksMember) exist to make impossible.
//
// # It does not take the Decision, and that is the subtle half
//
// Ownership here is read off the loaded guild row, never from [guildauth.Decision.Owns]. Authorize
// short-circuits at layer 1 and returns a Decision with a *zero* resolution, deliberately, because an
// Instance Admin is not a member and resolving them would be ADR 0008's conflation. So `decision.Owns()`
// is false for an Instance Admin **even when they genuinely own the guild** — and a check written the
// obvious way would refuse an instance operator the switch on a guild they created themselves. That
// reads as a permission bug and invites the repair that hands the tier the capability this function
// exists to withhold. It is the same failure shape M13 recorded for `highestPosition`, which reports the
// owner at the bottom of their own hierarchy.
//
// RemoveMember makes the same move for the same reason: it asks whether the *target* owns the guild, and
// on the path that most needs the answer there is no resolution to read it from. The row is already in
// hand here — Update loads it for the diff — so this costs nothing.
//
// # An Instance Admin is refused, which is the first time layer 1 is narrower than layer 2
//
// Rule 14 requires every Instance Admin action to be written to instance_audit_log, and that table is
// M72's and does not exist. So the tier could otherwise switch recording on for a guild it has never
// joined, read everything the guild subsequently says, and leave no record anywhere that it did. The
// refusal is temporary by construction and the direction is the reversible one: lifting it when M72
// lands is additive, while withdrawing a capability operators have built around is not.
//
// **What it buys is narrow and should not be oversold.** Decision.Allows returns true for layer 1, so an
// Instance Admin still *reads* any recording guild's log — that is M16a's already-ledgered gap and this
// does not close it. What they cannot do is start the recording. The line is between reading what a
// guild chose to collect and deciding what the instance collects about a guild that chose nothing.
//
// docs/security-ledger.md carries it with both reopening conditions: M72 arriving, and a *second* layer-1
// exception appearing anywhere — at which point two comments that do not know about each other need to
// become a mechanism, which is what this package has done four times rather than trust to memory.
func mayFlipMessageAudit(actor auth.Actor, existing db.Guild) bool {
	return snowflake.ID(existing.OwnerID) == actor.UserID
}

// Update changes a guild's own fields.
//
// # Two authorities in one request
//
// Everything here needs PermManageGuild, which is checked once below. M16b's recording switch needs the
// guild's owner on top of that, so the authority is built from the fields actually present — M12's
// correction to UpdateMember, where a permission only ever OR'd into a base made two moderation bits
// undeliverable on their own. The difference is that ownership is not a bit, so the extra check is
// [mayFlipMessageAudit] rather than a wider `need`.
//
// The order matters and is M12's "refuse before explaining": the ownership check runs *after* the guild
// row loads and before anything is written, so a non-member still gets the 404 authorize produced and a
// member who merely holds PermManageGuild gets 403 without learning what the setting currently is.
func (s *Service) Update(
	ctx context.Context, actor auth.Actor, guildID snowflake.ID, in UpdateGuildInput,
) (Guild, error) {
	var out Guild

	err := s.inTx(ctx, func(q *db.Queries) error {
		// Authorized on the transaction's querier, not the pool, so the permissions that allow the write
		// are read in the same snapshot the write happens in (rule 1). See guildauth.Authorize.
		if _, err := guildauth.Authorize(ctx, q, actor, guildID, 0, roles.PermManageGuild); err != nil {
			return err
		}

		// The prior row, read for the diff and for nothing else.
		//
		// M13 removed a GetGuild from Delete as a redundant read, and this one is not the same thing: a
		// diff needs the state being written over, and it exists exactly once — here, inside the
		// transaction that replaces it. The alternative is an UPDATE ... RETURNING that carries both sides,
		// which would mean a bespoke query shape on each of the four update paths to save one primary-key
		// lookup on a guild rename. Loading it is what UpdateRole and UpdateChannel already do; this makes
		// the four agree.
		existing, err := q.GetGuildForUpdate(ctx, int64(guildID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: get guild: %w", err)
		}

		// The recording switch needs more than the permission that got us here. Checked after the row
		// loads, so the refusal is an authorization answer rather than an input one (M12's "refuse before
		// explaining"), and before anything is written, so a refused request changes nothing.
		if in.MessageAuditEnabled != nil && !mayFlipMessageAudit(actor, existing) {
			return httpx.Errorf(httpx.ErrForbidden,
				"only the guild's owner may change whether its messages are recorded")
		}

		row, err := q.UpdateGuild(ctx, db.UpdateGuildParams{
			ID:                  int64(guildID),
			Name:                in.Name,
			Description:         in.Description,
			ClearDescription:    in.ClearDescription,
			MessageAuditEnabled: in.MessageAuditEnabled,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: update guild: %w", err)
		}

		changes := auditDiff{}
		if in.Name != nil {
			changes.changed("name", existing.Name, *in.Name)
		}
		if in.ClearDescription {
			changes.changed("description", orNil(existing.Description), nil)
		} else if in.Description != nil {
			changes.changed("description", orNil(existing.Description), *in.Description)
		}

		// **"Flipped" means the value actually moved.** M14 settled that a field sent with the value it
		// already had is not a change, and it matters more here than anywhere else in this package: an
		// entry announcing that recording was enabled when it was already enabled is exactly the noise a
		// real one could be hidden in, on the log whose whole purpose is that this cannot be hidden.
		recordingToggled := in.MessageAuditEnabled != nil &&
			*in.MessageAuditEnabled != existing.MessageAuditEnabled
		touchedUpdateFields := in.Name != nil || in.Description != nil || in.ClearDescription

		// The toggle is its own verb and is deliberately absent from the diff above, so a request that
		// *only* flips it writes one entry rather than a toggle plus a guild.update describing nothing.
		// Everything else is as it has been since M12: a request touching a field guild.update owns
		// writes one, and so does a request that changes nothing at all.
		if touchedUpdateFields || !recordingToggled {
			if err := s.writeAudit(
				ctx, q, guildID, actor.UserID, ActionGuildUpdate, &guildID, changes.payload(),
			); err != nil {
				return err
			}
		}

		if recordingToggled {
			action := ActionGuildMessageAuditDisable
			if *in.MessageAuditEnabled {
				action = ActionGuildMessageAuditEnable
			}

			toggle := auditDiff{}
			toggle.changed("message_audit_enabled", existing.MessageAuditEnabled, *in.MessageAuditEnabled)

			if err := s.writeAudit(
				ctx, q, guildID, actor.UserID, action, &guildID, toggle.payload(),
			); err != nil {
				return err
			}
		}

		out = guildFromRow(row)
		return nil
	})
	if err != nil {
		return Guild{}, err
	}

	return out, nil
}

// Delete removes a guild and everything under it.
//
// # Why the owner rather than a permission
//
// PermManageGuild is the bit for renaming a guild and editing its settings, and it is one an owner
// reasonably hands to a trusted administrator. Deleting the guild is not in that class: it destroys every
// channel, message and membership in it, cascading, with no undo. ADR 0008 makes the owner layer 2 for
// exactly this kind of decision, and an Instance Admin passes by layer 1 because operating the instance
// includes removing what is on it.
func (s *Service) Delete(ctx context.Context, actor auth.Actor, guildID snowflake.ID) error {
	return s.inTx(ctx, func(q *db.Queries) error {
		// One query, not two. This used to read the guild for its owner id and then authorize, which ran
		// ListGuildMemberAuthority — a query whose first column is that same owner id. M12 measured the
		// redundancy and kept it rather than widen roles.Resolve's return for it; M13 widened that return
		// for layer 4's standing, so the owner id arrives here for free.
		//
		// PermViewChannel is what a non-member fails, so a stranger gets the ordinary 404 rather than "you
		// are not the owner" — which would tell them the guild exists.
		allowed, err := guildauth.Authorize(ctx, q, actor, guildID, 0, roles.PermViewChannel)
		if err != nil {
			return err
		}

		// Not the owner. An Instance Admin is still allowed through, and a member who merely holds
		// PermManageGuild is not — so this cannot be a plain permission check.
		if !allowed.InstanceAdmin() && !allowed.Owns() {
			return httpx.ErrForbidden
		}

		// Layer 1 skipped the resolution, so nothing has established that this guild exists.
		//
		// Every other actor reaching this line was resolved against the guild and would have been refused
		// with 404 if it were not there. An Instance Admin is deliberately not resolved — the tier acts on
		// guilds it is not in, so checking membership first would answer 404 for every guild on the
		// instance — and the consequence is that for this one actor the first statement to touch the guild
		// is the audit write, which carries a foreign key to it. Without this read that is a constraint
		// violation and a 500, where everybody else gets 404.
		//
		// M12 had it by accident: Delete opened with a GetGuild whose ErrNoRows branch answered 404, and
		// the owner comparison happened to want the same row. Removing that read for the resolved paths
		// removed the existence check along with it. Paid only on the tier that skipped the resolution,
		// which is the one place it is not redundant.
		if allowed.InstanceAdmin() {
			if _, err := q.GetGuild(ctx, int64(guildID)); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return httpx.ErrNotFound
				}
				return fmt.Errorf("guilds: get guild: %w", err)
			}
		}

		// The audit entry is written before the delete, in the same transaction. It has to be: guild_id
		// references guilds(id) ON DELETE CASCADE, so an entry written afterwards would be deleted by the
		// very cascade it is recording — and an entry written before is removed by that same cascade too.
		//
		// So this row does not survive, and that is a real gap rather than a subtlety being glossed. Rule
		// 2 is satisfied — the entry is written in the mutation's transaction — but nothing can read it
		// afterwards, because M14's audit log is per-guild and this guild is gone. The instance-scoped
		// record of a guild deletion belongs in instance_audit_log, which is rule 14's table and M72's
		// milestone. Written down here rather than discovered there.
		// The one action that records nothing, and deliberately.
		//
		// audit_log_entries cascades from guilds, so this entry is written inside the transaction — rule 2
		// holds — and removed by the DELETE two lines below. Describing what was destroyed would be work
		// whose result nothing can ever read. The durable record of an instance-level action is rule 14's
		// instance_audit_log (M72), which is a different table for exactly this reason.
		if err := s.writeAudit(ctx, q, guildID, actor.UserID, ActionGuildDelete, &guildID, nil); err != nil {
			return err
		}

		affected, err := q.DeleteGuild(ctx, int64(guildID))
		if err != nil {
			return fmt.Errorf("guilds: delete guild: %w", err)
		}
		if affected == 0 {
			return httpx.ErrNotFound
		}

		return nil
	})
}
