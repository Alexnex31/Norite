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
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// defaultEveryonePermissions is what @everyone gets in a newly created guild.
//
// Viewing, sending, connecting to voice, speaking and adding a reaction — the set a person joining a
// social space expects to work without anybody configuring anything. Deliberately not PermCreateInvite:
// who may bring others in is the first decision an owner should make on purpose rather than discover.
const defaultEveryonePermissions = roles.PermViewChannel |
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
}

// Update changes a guild's own fields.
func (s *Service) Update(
	ctx context.Context, actor auth.Actor, guildID snowflake.ID, in UpdateGuildInput,
) (Guild, error) {
	var out Guild

	err := s.inTx(ctx, func(q *db.Queries) error {
		// Authorized on the transaction's querier, not the pool, so the permissions that allow the write
		// are read in the same snapshot the write happens in (rule 1). See authorizeWith.
		if _, err := authorizeWith(ctx, q, actor, guildID, 0, roles.PermManageGuild); err != nil {
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
		existing, err := q.GetGuild(ctx, int64(guildID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: get guild: %w", err)
		}

		row, err := q.UpdateGuild(ctx, db.UpdateGuildParams{
			ID:               int64(guildID),
			Name:             in.Name,
			Description:      in.Description,
			ClearDescription: in.ClearDescription,
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

		if err := s.writeAudit(
			ctx, q, guildID, actor.UserID, ActionGuildUpdate, &guildID, changes.payload(),
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
		allowed, err := authorizeWith(ctx, q, actor, guildID, 0, roles.PermViewChannel)
		if err != nil {
			return err
		}

		// Not the owner. An Instance Admin is still allowed through, and a member who merely holds
		// PermManageGuild is not — so this cannot be a plain permission check.
		if !allowed.instanceAdmin && !allowed.owns() {
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
		if allowed.instanceAdmin {
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
