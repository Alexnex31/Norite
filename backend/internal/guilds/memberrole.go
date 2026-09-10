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

// AssignRole gives a member a role.
//
// # Why this endpoint had to exist for any of the rest to mean anything
//
// Nothing wrote guild_member_roles before M13 — not M12, not architecture.md's endpoint list, not any
// milestone through M125. So every non-owner's standing was permanently 0, and a hierarchy check landed
// without this would have shipped with its interesting branch unreachable through the API. Worse, "give
// yourself a role above your own" is the attack the position rule exists to refuse, and nothing could
// attempt it.
//
// # Four checks
//
// PermManageRoles gets the caller in. Then: the role must be strictly below their standing, the target
// must be too, and — the one that departs from Discord — the role's permissions must be a subset of what
// the caller holds.
//
// That last check is deliberate and is this milestone's only deviation from parity. Discord gates
// assignment on hierarchy alone and relies on powerful roles being placed high, so a Bot role sitting at
// position 2 with MANAGE_GUILD is a misconfiguration rather than a vulnerability in their model. That is
// a configuration assumption a public instance should not rest on: without the subset check, a moderator
// holding PermManageRoles and not PermManageGuild assigns such a role to an account they control and has
// PermManageGuild by proxy. "A delegated authority never exceeds its delegator" holds everywhere else in
// this codebase — auth's scopes, role creation, the overwrite endpoints — and assignment is that question
// wearing different clothes.
//
// It needs no owner or Instance-Admin branch: refuseEscalation takes a decision, and decision.allows
// already returns true for layer 1 and for the owner, who resolves to permAll.
func (s *Service) AssignRole(
	ctx context.Context, actor auth.Actor, guildID, userID, roleID snowflake.ID,
) (Member, error) {
	return s.changeMemberRole(ctx, actor, guildID, userID, roleID, true)
}

// UnassignRole takes a role away from a member.
//
// # Not the pure demotion it looks like
//
// Removing a role reads as safe — the member ends with less. That was true of every permission model this
// project had before M13 and is false now that a role can carry a channel *deny*: the commonest use of a
// role overwrite in any guild is exactly that, a `muted` role denying PermSendMessages. Taking such a role
// off somebody lifts the restriction it carried.
//
// So this is checked like an assignment and not like a cleanup. The role must be below the caller's
// standing — including when they are removing it from themselves, which is why there is no self-exemption
// on that half and why nobody can shed the role that establishes their own position. And the caller must
// hold whatever the role's overwrites touch, in the channels they touch.
//
// The limit that remains is worth stating rather than papering over: a positional hierarchy cannot protect
// a restriction placed low, and a restricting role is placed low by definition. A moderator who holds both
// PermManageRoles and a `muted` role above the floor can still shed it. That is not a hole in this check,
// it is the reason PermModerateMembers at M74 is a bit on the member rather than a role.
func (s *Service) UnassignRole(
	ctx context.Context, actor auth.Actor, guildID, userID, roleID snowflake.ID,
) (Member, error) {
	return s.changeMemberRole(ctx, actor, guildID, userID, roleID, false)
}

// changeMemberRole is both verbs, because they differ in three lines and share every check.
//
// Written as one function rather than two for the reason authorize is one function: the checks below are
// the whole of what stops PermManageRoles being every permission in the guild, and two copies of them is
// two places for the next change to land in only one of.
func (s *Service) changeMemberRole(
	ctx context.Context, actor auth.Actor, guildID, userID, roleID snowflake.ID, assigning bool,
) (Member, error) {
	var out Member

	err := s.inTx(ctx, func(q *db.Queries) error {
		allowed, err := authorizeWith(ctx, q, actor, guildID, 0, roles.PermManageRoles)
		if err != nil {
			return err
		}

		role, err := q.GetRole(ctx, db.GetRoleParams{ID: int64(roleID), GuildID: int64(guildID)})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: get role: %w", err)
		}

		// @everyone is held by virtue of membership and is never stored in guild_member_roles — the
		// authority query reads it through `is_default OR EXISTS(...)` precisely so it does not have to
		// be. The statement refuses it too; this is here so the caller gets a message rather than a 404.
		if role.IsDefault {
			return httpx.Errorf(ErrDefaultRoleImmutable,
				"the default role is held by every member and cannot be assigned")
		}

		// Layer 4, on the role. Applies to self as much as to anybody: you cannot take a role above your
		// own standing, and you cannot shed the one that establishes it.
		if !allowed.outranks(role.Position) {
			return httpx.Errorf(ErrOutranked, "you cannot manage a role above your own")
		}

		// Layer 4, on the person. GetMemberHighestRolePosition returns no row for a non-member rather than
		// a coalesced zero, which is what keeps "not in this guild" from evaluating as "at the floor".
		standing, err := q.GetMemberHighestRolePosition(ctx, db.GetMemberHighestRolePositionParams{
			GuildID: int64(guildID),
			UserID:  int64(userID),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: get target standing: %w", err)
		}

		// Skipped when the target is the caller, and only this half is skipped.
		//
		// Nobody outranks themselves, so a flat "no self-exemption" would make self-assignment and
		// self-removal impossible for every non-owner — breaking self-service roles, which are an ordinary
		// configuration, and making the rule above unreachable rather than strict. The role check is the
		// one that has to hold on yourself, and it does.
		if userID != actor.UserID && !allowed.outranksMember(userID, standing) {
			return httpx.Errorf(ErrOutranked, "you cannot act on a member above you")
		}

		// The subset check, and its mirror.
		//
		// Assigning hands the target this role's permissions, so the caller must hold them. Unassigning
		// lifts whatever the role's channel overwrites imposed on that member, which is the same question
		// asked backwards — and reuses the check DeleteRole already needed, because deleting a role and
		// taking it off somebody remove the same denies from the same person's resolution.
		if assigning {
			if err := refuseEscalation(allowed, roles.PermissionFromInt64(role.Permissions)); err != nil {
				return err
			}
		}
		if err := s.refuseRemovingOverwritesFor(
			ctx, q, allowed, guildID, roles.OverwriteTargetRole, roleID,
		); err != nil {
			return err
		}

		action := ActionMemberRoleRemove
		if assigning {
			action = ActionMemberRoleAdd

			// Zero rows is the PUT succeeding on a grant that already exists, and it can only mean that:
			// the role read and the standing read above have already answered 404 for a role outside this
			// guild and for a target outside it. The statement carries ON CONFLICT DO NOTHING rather than
			// letting the violation surface, because a constraint error aborts this transaction — and the
			// audit write and the member read below are in it.
			if _, err := q.AssignRoleToMember(ctx, db.AssignRoleToMemberParams{
				GuildID: int64(guildID),
				UserID:  int64(userID),
				RoleID:  int64(roleID),
			}); err != nil {
				return fmt.Errorf("guilds: assign role: %w", err)
			}
		} else if _, err := q.UnassignRoleFromMember(ctx, db.UnassignRoleFromMemberParams{
			GuildID: int64(guildID),
			UserID:  int64(userID),
			RoleID:  int64(roleID),
		}); err != nil {
			// Zero rows means the member did not hold it, which is an idempotent DELETE succeeding. Their
			// membership was already established by the standing read above.
			return fmt.Errorf("guilds: unassign role: %w", err)
		}

		target := userID
		if err := s.writeAudit(ctx, q, guildID, actor.UserID, action, &target, map[string]any{
			"role_id": roleID.String(),
		}); err != nil {
			return err
		}

		member, err := q.GetGuildMember(ctx, db.GetGuildMemberParams{
			GuildID: int64(guildID),
			UserID:  int64(userID),
		})
		if err != nil {
			return fmt.Errorf("guilds: get member: %w", err)
		}

		grants, err := q.ListMemberRoleIDs(ctx, db.ListMemberRoleIDsParams{
			GuildID: int64(guildID),
			UserIds: []int64{int64(userID)},
		})
		if err != nil {
			return fmt.Errorf("guilds: list member roles: %w", err)
		}

		held := make([]snowflake.ID, 0, len(grants))
		for _, g := range grants {
			held = append(held, snowflake.ID(g.RoleID))
		}

		out = memberFromRow(member, held)
		return nil
	})
	if err != nil {
		return Member{}, err
	}

	return out, nil
}
