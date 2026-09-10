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
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// ListRoles returns a guild's roles in position order.
func (s *Service) ListRoles(
	ctx context.Context, actor auth.Actor, guildID snowflake.ID,
) ([]Role, error) {
	if err := s.authorize(ctx, actor, guildID, 0, roles.PermViewChannel); err != nil {
		return nil, err
	}

	rows, err := s.queries.ListGuildRoles(ctx, int64(guildID))
	if err != nil {
		return nil, fmt.Errorf("guilds: list roles: %w", err)
	}

	out := make([]Role, 0, len(rows))
	for _, row := range rows {
		out = append(out, roleFromRow(row))
	}

	return out, nil
}

// CreateRoleInput is the request to create a role.
type CreateRoleInput struct {
	Name        string
	Color       int32
	Permissions roles.Permission
	Hoist       bool
	Mentionable bool
}

// CreateRole adds a role to a guild.
//
// # The escalation this refuses
//
// A caller may not grant a role permissions they do not themselves hold. Without that check,
// PermManageRoles is every permission: create a role carrying PermAdministrator, assign it to yourself,
// and the hierarchy is gone. It is the single most important check in this file and the one whose absence
// would look completely normal.
//
// The check is against the caller's *resolved* permissions, so an owner and an administrator pass it
// trivially — they hold everything — and an Instance Admin passes by layer 1 without being resolved at
// all.
//
// # Where the new role lands, and why not at the top
//
// At the bottom, immediately above @everyone, which is what Discord does. M12 created roles at
// max(position)+1 and said so deliberately, on the grounds that a role landing below an existing one
// reads as broken. That was the right call for a milestone with no hierarchy and the wrong one the
// moment this milestone added it: a non-owner who creates a role at the top cannot then edit, delete,
// assign or reposition it, because all four require the role to be strictly below their own standing.
// A dead end reachable by an ordinary moderator on their first use of the endpoint.
//
// One case behaves oddly and is allowed rather than refused. A member holding PermManageRoles only
// through @everyone has standing 0, so the role they create at position 1 is above them and they cannot
// manage it either. Nothing escalates — they cannot assign it to themselves, since that too requires it
// to be below them — so it is a harmless dead end reachable only in a guild that has deliberately given
// role management to everybody. Discord permits it; so does this.
func (s *Service) CreateRole(
	ctx context.Context, actor auth.Actor, guildID snowflake.ID, in CreateRoleInput,
) (Role, error) {
	roleID, err := s.ids.Next()
	if err != nil {
		return Role{}, fmt.Errorf("guilds: mint role id: %w", err)
	}

	var out Role

	err = s.inTx(ctx, func(q *db.Queries) error {
		allowed, err := authorizeWith(ctx, q, actor, guildID, 0, roles.PermManageRoles)
		if err != nil {
			return err
		}
		if err := refuseEscalation(allowed, in.Permissions); err != nil {
			return err
		}

		// The ceiling, checked after authorization so a non-member cannot learn how full a guild is.
		count, err := q.CountGuildRoles(ctx, int64(guildID))
		if err != nil {
			return fmt.Errorf("guilds: count roles: %w", err)
		}
		if count >= int64(s.maxRolesPerGuild) {
			return httpx.Errorf(ErrGuildFull, "a guild may hold at most %d roles", s.maxRolesPerGuild)
		}

		// The lock, then the renumber, then the insert at the bottom.
		//
		// The lock is not optional and its reason survives the change of placement: renumbering and then
		// inserting is a read-modify-write, and under READ COMMITTED two concurrent creates would both
		// renumber, both insert at 1, and leave two roles sharing a position — which migration 000015
		// deliberately declines the unique constraint that would catch.
		//
		// ShiftRolePositionsUp renumbers the guild's live non-default roles to 2..N+1, leaving 1 free.
		// Renumbering rather than incrementing is what keeps positions bounded by the role ceiling instead
		// of by the guild's lifetime creation count — see the query.
		if err := q.LockGuildRolePositions(ctx, int64(guildID)); err != nil {
			return fmt.Errorf("guilds: lock role positions: %w", err)
		}

		if err := q.ShiftRolePositionsUp(ctx, int64(guildID)); err != nil {
			return fmt.Errorf("guilds: renumber role positions: %w", err)
		}

		// One, never zero: zero is @everyone's and a non-default role sharing it would be neither above
		// nor below the floor every layer-4 resolution starts from.
		const bottom = 1

		row, err := q.CreateRole(ctx, db.CreateRoleParams{
			ID:          int64(roleID),
			GuildID:     int64(guildID),
			Name:        in.Name,
			Color:       in.Color,
			Permissions: in.Permissions.Int64(),
			Position:    bottom,
			Hoist:       in.Hoist,
			Mentionable: in.Mentionable,
			IsDefault:   false,
		})
		if err != nil {
			return fmt.Errorf("guilds: create role: %w", err)
		}

		if err := s.writeAudit(ctx, q, guildID, actor.UserID, ActionRoleCreate, &roleID, map[string]any{
			"name":        in.Name,
			"permissions": in.Permissions.Int64(),
		}); err != nil {
			return err
		}

		out = roleFromRow(row)
		return nil
	})
	if err != nil {
		return Role{}, err
	}

	return out, nil
}

// UpdateRoleInput is a partial update. A nil field is left alone.
//
// Position is absent on purpose, and not because reordering does not exist — ReorderRoles is in this
// file. It is a multi-row swap: moving one role moves every role between it and its destination, so a
// single-row update cannot express one without leaving two roles sharing a position part-way through.
type UpdateRoleInput struct {
	Name        *string
	Color       *int32
	Permissions *roles.Permission
	Hoist       *bool
	Mentionable *bool
}

// UpdateRole changes a role's own fields.
func (s *Service) UpdateRole(
	ctx context.Context, actor auth.Actor, guildID, roleID snowflake.ID, in UpdateRoleInput,
) (Role, error) {
	var out Role

	err := s.inTx(ctx, func(q *db.Queries) error {
		allowed, err := authorizeWith(ctx, q, actor, guildID, 0, roles.PermManageRoles)
		if err != nil {
			return err
		}

		existing, err := q.GetRole(ctx, db.GetRoleParams{ID: int64(roleID), GuildID: int64(guildID)})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: get role: %w", err)
		}

		// Layer 4's second sentence, and it is the half M12 could not build: a role above your own is not
		// yours to edit. Below the load, because it reads a row the caller may not be able to see — the
		// ordering M12 had corrected twice by review.
		//
		// This bounds what refuseEscalation cannot. That check stops you granting a permission you do not
		// hold; it says nothing about *which* role you may grant it to, so without this a moderator could
		// edit the administrator role's color, name, or the permissions it hands everybody who holds it.
		if !allowed.outranks(existing.Position) {
			return httpx.Errorf(ErrOutranked, "you cannot manage a role above your own")
		}

		if in.Permissions != nil {
			if err := refuseEscalation(allowed, *in.Permissions); err != nil {
				return err
			}
		}

		// @everyone may have its permissions edited — that is how a guild sets its floor — but not its
		// name or its identity. Renaming it would leave every client's "@everyone" label pointing at a role
		// that no longer says so, and the mention syntax is keyed off the name.
		if existing.IsDefault && in.Name != nil {
			return httpx.Errorf(ErrDefaultRoleImmutable, "the default role cannot be renamed")
		}

		var permissions *int64
		if in.Permissions != nil {
			v := in.Permissions.Int64()
			permissions = &v
		}

		row, err := q.UpdateRole(ctx, db.UpdateRoleParams{
			ID:          int64(roleID),
			GuildID:     int64(guildID),
			Name:        in.Name,
			Color:       in.Color,
			Permissions: permissions,
			Hoist:       in.Hoist,
			Mentionable: in.Mentionable,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: update role: %w", err)
		}

		changes := map[string]any{}
		if in.Name != nil {
			changes["name"] = *in.Name
		}
		if in.Permissions != nil {
			changes["permissions"] = in.Permissions.Int64()
		}

		if err := s.writeAudit(ctx, q, guildID, actor.UserID, ActionRoleUpdate, &roleID, changes); err != nil {
			return err
		}

		out = roleFromRow(row)
		return nil
	})
	if err != nil {
		return Role{}, err
	}

	return out, nil
}

// DeleteRole removes a role. @everyone is refused, in SQL — see DeleteRole in guilds.sql.
func (s *Service) DeleteRole(ctx context.Context, actor auth.Actor, guildID, roleID snowflake.ID) error {
	return s.inTx(ctx, func(q *db.Queries) error {
		allowed, err := authorizeWith(ctx, q, actor, guildID, 0, roles.PermManageRoles)
		if err != nil {
			return err
		}

		existing, err := q.GetRole(ctx, db.GetRoleParams{ID: int64(roleID), GuildID: int64(guildID)})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: get role: %w", err)
		}

		// Layer 4's second sentence: a role above your own is not yours to delete. Below the load, because
		// it reads a row the caller may not be able to see — the ordering M12 had corrected twice.
		if !allowed.outranks(existing.Position) {
			return httpx.Errorf(ErrOutranked, "you cannot manage a role above your own")
		}

		// And the overwrites it carries are not yours to remove unless you hold what they carry. The
		// delete below wipes them on every channel at once, which is the same act DeleteOverwrite refuses
		// one row at a time — see refuseRemovingOverwritesFor.
		if err := s.refuseRemovingOverwritesFor(
			ctx, q, allowed, guildID, roles.OverwriteTargetRole, roleID,
		); err != nil {
			return err
		}

		if existing.IsDefault {
			// Reported here so the caller gets a message rather than a bare 404, but the *guarantee* is the
			// `AND NOT is_default` in the statement below — this check could be raced and that one cannot.
			return httpx.Errorf(ErrDefaultRoleImmutable, "the default role cannot be deleted")
		}

		if err := s.writeAudit(ctx, q, guildID, actor.UserID, ActionRoleDelete, &roleID, nil); err != nil {
			return err
		}

		// The overwrites naming this role go with it. target_id is polymorphic and so cannot be a foreign
		// key, which means nothing cascades — without this the rows outlive the role permanently, on every
		// channel that had one, and ListChannelPermissionOverwrites keeps scanning them on every check.
		if err := q.DeleteOverwritesForTarget(ctx, db.DeleteOverwritesForTargetParams{
			GuildID:    int64(guildID),
			TargetType: roles.OverwriteTargetRole,
			TargetID:   int64(roleID),
		}); err != nil {
			return fmt.Errorf("guilds: delete role overwrites: %w", err)
		}

		affected, err := q.DeleteRole(ctx, db.DeleteRoleParams{ID: int64(roleID), GuildID: int64(guildID)})
		if err != nil {
			return fmt.Errorf("guilds: delete role: %w", err)
		}
		if affected == 0 {
			return httpx.ErrNotFound
		}

		return nil
	})
}

// refuseEscalation refuses granting permissions the actor does not hold.
//
// See CreateRole's comment for why this is the most important check in the file.
//
// Pure, and takes the decision authorizeWith already reached rather than re-deriving it. It used to run
// its own IsInstanceAdmin and its own full roles.Resolve immediately after authorizeWith had run both,
// inside the same transaction — five round trips for one INSERT. The facts cannot change between the two
// calls; only the question does.
//
// An Instance Admin passes through decision.allows, because layer 1 sits outside the guild: they are not
// a member, resolve to zero permissions, and would otherwise be unable to grant anything at all.
func refuseEscalation(allowed decision, want roles.Permission) error {
	// Bits no constant defines are refused before authority is consulted at all, because the Instance Admin
	// short-circuit below does not consult the bitfield — so without this an admin could store bit 62, and
	// a later milestone defining it would find it already granted. For a non-admin the escalation check
	// masks unknown bits incidentally, since permAll.Has(unknownBit) is false; incidental is not a rule.
	if !want.Known() {
		return httpx.Errorf(httpx.ErrBadRequest,
			"permissions contains bits this instance does not define")
	}

	if allowed.allows(want) {
		return nil
	}

	// Deliberately does not name the bits. Reporting which permission was refused tells a caller probing
	// the boundary exactly where it is, and they can already read their own permissions from the role
	// listing.
	return httpx.Errorf(httpx.ErrForbidden,
		"a role cannot be given permissions you do not hold yourself")
}

// RolePosition is one row of a reorder request.
type RolePosition struct {
	ID       snowflake.ID
	Position int32
}

// ReorderRoles rearranges a guild's role hierarchy in one transaction.
//
// # Why this is not a field on UpdateRole
//
// Reordering is a multi-row swap. Moving a role from 2 to 5 means moving whatever sits at 3, 4 and 5 as
// well, and done as four separate requests there is a window between each where two roles share a
// position — which migration 000015 declines the unique constraint that would catch, because a reorder
// needs to pass through exactly such a state. Two roles at one position are neither above nor below each
// other, and that dissolves the strictly-greater comparison every check in this milestone rests on. One
// request, one transaction, one lock.
//
// # Five checks, and the one that is easy to leave out
//
// The obvious rule is that every requested position must be strictly below the caller's standing. That is
// necessary and not sufficient: it says nothing about where the role is *now*. A caller at standing 5
// could name the administrator role at position 10 and move it to 2 — the destination passes, the role
// is demoted, and from there it can be edited, deleted or assigned. **Both ends are checked**, and the
// origin is the one that is easy to omit, because the destination is the value in the request body and
// the origin is not.
//
// The rest: the request must be well-formed (bounded, no repeated id, every id a role in this guild),
// every position must sit in a range that cannot collide with @everyone or overflow the column, and the
// *resulting arrangement* must leave no two non-default roles sharing a position — which requires
// checking the request against the roles it does not mention, not only against itself.
func (s *Service) ReorderRoles(
	ctx context.Context, actor auth.Actor, guildID snowflake.ID, in []RolePosition,
) ([]Role, error) {
	var out []Role

	err := s.inTx(ctx, func(q *db.Queries) error {
		allowed, err := authorizeWith(ctx, q, actor, guildID, 0, roles.PermManageRoles)
		if err != nil {
			return err
		}

		// The same lock role creation takes, and for the same reason: this reads every position and then
		// writes them, which under READ COMMITTED is a read-modify-write. A concurrent create renumbering
		// underneath would leave an arrangement neither operation intended.
		if err := q.LockGuildRolePositions(ctx, int64(guildID)); err != nil {
			return fmt.Errorf("guilds: lock role positions: %w", err)
		}

		current, err := q.ListGuildRoles(ctx, int64(guildID))
		if err != nil {
			return fmt.Errorf("guilds: list roles: %w", err)
		}

		byID := make(map[snowflake.ID]db.Role, len(current))
		for _, r := range current {
			byID[snowflake.ID(r.ID)] = r
		}

		// The upper bound. Positions only ever grow through creation, which renumbers to the live role
		// count plus one, so the ceiling bounds them — and a reorder is the one path that could write an
		// arbitrary integer. Without it an owner could place a role near the column's maximum and the next
		// creation's renumber would overflow, taking role creation in that guild out permanently.
		maxPosition := s.maxRolesPerGuild

		seen := make(map[snowflake.ID]struct{}, len(in))
		for _, want := range in {
			role, ok := byID[want.ID]
			if !ok {
				return httpx.ErrNotFound
			}
			if _, duplicate := seen[want.ID]; duplicate {
				return httpx.Errorf(httpx.ErrBadRequest, "a role appears more than once")
			}
			seen[want.ID] = struct{}{}

			if role.IsDefault {
				return httpx.Errorf(ErrDefaultRoleImmutable, "the default role cannot be repositioned")
			}
			if want.Position < 1 || want.Position > maxPosition {
				// Zero is @everyone's and below zero is beneath the floor every layer-4 resolution starts
				// from. A client sending zero-based positions is the ordinary way in, since role lists
				// render 0-indexed.
				return httpx.Errorf(httpx.ErrBadRequest,
					"position must be between 1 and %d", maxPosition)
			}

			// Both ends. See the doc comment: checking only the destination lets a caller demote a role
			// that is currently above them, which is a takeover in three requests.
			if !allowed.outranks(role.Position) || !allowed.outranks(want.Position) {
				return httpx.Errorf(ErrOutranked, "you cannot manage a role above your own")
			}
		}

		// The resulting arrangement, including the roles the request does not mention. A request that is
		// internally consistent can still collide with one of those, and a collision is the corruption
		// this whole operation exists to avoid producing.
		final := make(map[int32]snowflake.ID, len(current))
		for _, r := range current {
			if r.IsDefault {
				continue
			}
			id := snowflake.ID(r.ID)
			position := r.Position
			for _, want := range in {
				if want.ID == id {
					position = want.Position
					break
				}
			}
			if other, taken := final[position]; taken {
				return httpx.Errorf(httpx.ErrConflict,
					"the result would put two roles at position %d — role %s is already there",
					position, other)
			}
			final[position] = id
		}

		changes := make(map[string]any, len(in))
		for _, want := range in {
			affected, err := q.SetRolePosition(ctx, db.SetRolePositionParams{
				Position: want.Position,
				ID:       int64(want.ID),
				GuildID:  int64(guildID),
			})
			if err != nil {
				return fmt.Errorf("guilds: set role position: %w", err)
			}
			if affected == 0 {
				// Every reason this statement matches nothing was ruled out above, so reaching here means
				// the guild changed underneath the lock — which it cannot, or the checks are wrong.
				return fmt.Errorf("guilds: role %s was not repositioned", want.ID)
			}
			changes[want.ID.String()] = want.Position
		}

		if err := s.writeAudit(
			ctx, q, guildID, actor.UserID, ActionRoleReorder, nil, changes,
		); err != nil {
			return err
		}

		reordered, err := q.ListGuildRoles(ctx, int64(guildID))
		if err != nil {
			return fmt.Errorf("guilds: list roles: %w", err)
		}
		out = make([]Role, 0, len(reordered))
		for _, row := range reordered {
			out = append(out, roleFromRow(row))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
