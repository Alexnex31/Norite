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
// all. M13 adds the other half, position-based hierarchy: who may manage a role *above* their own.
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
		if count >= maxRolesPerGuild {
			return httpx.Errorf(ErrGuildFull, "a guild may hold at most %d roles", maxRolesPerGuild)
		}

		// Appended above every existing role. Position is the hierarchy M13 enforces, and a new role
		// landing at or below an existing one reads as broken — see NextRolePosition for why this is
		// max(position)+1 and not the role count, which collides as soon as anything has been deleted.
		//
		// The lock is the other half of the same guarantee. Reading a max and then inserting is a
		// read-modify-write, and under READ COMMITTED two concurrent creates both read the same value and
		// both take it. Fixing only the count-versus-max cause left the race, which produces the identical
		// corruption by a different route.
		if err := q.LockGuildRolePositions(ctx, int64(guildID)); err != nil {
			return fmt.Errorf("guilds: lock role positions: %w", err)
		}

		position, err := q.NextRolePosition(ctx, int64(guildID))
		if err != nil {
			return fmt.Errorf("guilds: next role position: %w", err)
		}

		row, err := q.CreateRole(ctx, db.CreateRoleParams{
			ID:          int64(roleID),
			GuildID:     int64(guildID),
			Name:        in.Name,
			Color:       in.Color,
			Permissions: in.Permissions.Int64(),
			Position:    position,
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
// Position is absent on purpose: reordering is a multi-row swap and hierarchy semantics are M13's. This
// milestone stores the column and orders by it.
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
		if _, err := authorizeWith(ctx, q, actor, guildID, 0, roles.PermManageRoles); err != nil {
			return err
		}

		existing, err := q.GetRole(ctx, db.GetRoleParams{ID: int64(roleID), GuildID: int64(guildID)})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: get role: %w", err)
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
