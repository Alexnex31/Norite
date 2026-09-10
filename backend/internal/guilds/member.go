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

// Member listing page size.
//
// The cap is enforced rather than merely defaulted, and it is rule 21's browser sanity check as much as
// rule 7's hot path: an unbounded member list on a large guild is a response that scales with something
// the client did not choose. 100 matches the ceiling every comparable API uses, so a client written
// against one of them paginates correctly here by accident rather than by reading this.
const (
	defaultMemberPageSize = 50
	maxMemberPageSize     = 100
)

// ListMembersInput is a cursor page request.
type ListMembersInput struct {
	// After is the user id to resume from, exclusive. Zero starts at the beginning.
	//
	// Cursor rather than offset, everywhere (§2). An OFFSET makes page N cost N pages of scanning and
	// shifts every row when somebody joins mid-read; a cursor costs one index descent and is stable.
	After snowflake.ID
	Limit int32
}

// ListMembers returns a page of a guild's members, each with the roles they hold.
func (s *Service) ListMembers(
	ctx context.Context, actor auth.Actor, guildID snowflake.ID, in ListMembersInput,
) ([]Member, error) {
	if err := s.authorize(ctx, actor, guildID, 0, roles.PermViewChannel); err != nil {
		return nil, err
	}

	limit := in.Limit
	switch {
	case limit <= 0:
		limit = defaultMemberPageSize
	case limit > maxMemberPageSize:
		// Clamped rather than refused. A client asking for more than the ceiling is not making an error
		// worth failing a request over, and telling it the ceiling by returning that many is clearer than
		// a 400 it has to parse.
		limit = maxMemberPageSize
	}

	rows, err := s.queries.ListGuildMembers(ctx, db.ListGuildMembersParams{
		GuildID: int64(guildID),
		UserID:  int64(in.After),
		Limit:   limit,
	})
	if err != nil {
		return nil, fmt.Errorf("guilds: list members: %w", err)
	}

	// One query for the whole page's role grants, not one per member — the N+1 §15.2 names.
	userIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		userIDs = append(userIDs, row.UserID)
	}

	byUser := map[int64][]snowflake.ID{}
	if len(userIDs) > 0 {
		grants, err := s.queries.ListMemberRoleIDs(ctx, db.ListMemberRoleIDsParams{
			GuildID: int64(guildID),
			UserIds: userIDs,
		})
		if err != nil {
			return nil, fmt.Errorf("guilds: list member roles: %w", err)
		}
		for _, g := range grants {
			byUser[g.UserID] = append(byUser[g.UserID], snowflake.ID(g.RoleID))
		}
	}

	out := make([]Member, 0, len(rows))
	for _, row := range rows {
		out = append(out, memberFromRow(row, byUser[row.UserID]))
	}

	return out, nil
}

// UpdateMemberInput is a partial update. A nil field is left alone.
type UpdateMemberInput struct {
	Nickname      *string
	ClearNickname bool
	Deaf          *bool
	Mute          *bool
}

// UpdateMember changes a member's guild-scoped attributes.
//
// The permission depends on the field, which is why this is not one authorize call. A nickname is
// cosmetic and lives under PermManageGuild; server mute and deafen are moderation actions with their own
// bits, and conflating them would let anybody who can rename people also silence them.
func (s *Service) UpdateMember(
	ctx context.Context, actor auth.Actor, guildID, userID snowflake.ID, in UpdateMemberInput,
) (Member, error) {
	var out Member

	err := s.inTx(ctx, func(q *db.Queries) error {
		// Each field brings its own permission, and only its own.
		//
		// The first version started `need` at PermManageGuild and added the moderation bits to it, which
		// made both of them undeliverable as standalone grants: a "voice moderator" role holding exactly
		// PermMuteMembers was refused, because Has requires every bit and PermManageGuild was always in the
		// set. The only way to grant muting was to also grant the ability to rename the guild and edit its
		// settings — the opposite of what splitting the bits was for.
		//
		// So the base is empty and each present field contributes. A request that sends nothing needs
		// nothing beyond membership, which authorizeWith establishes anyway by resolving at all.
		var need roles.Permission
		if in.Nickname != nil || in.ClearNickname {
			need = need.Add(roles.PermManageGuild)
		}
		if in.Mute != nil {
			need = need.Add(roles.PermMuteMembers)
		}
		if in.Deaf != nil {
			need = need.Add(roles.PermDeafenMembers)
		}

		// A request that changes nothing needs nothing, and Permission.Has(0) is true by design — so
		// without this any member could PATCH any other member with an empty body, hold no permission at
		// all, and still append a row to a table migration 000016 says must never be swept. Refused as a
		// bad request, which it is: there is no such thing as a meaningful empty update here.
		if need == 0 {
			return httpx.Errorf(httpx.ErrBadRequest, "no fields to update")
		}

		allowed, err := authorizeWith(ctx, q, actor, guildID, 0, need)
		if err != nil {
			return err
		}

		// Layer 4, on the person — the half M12 shipped without and a security review found.
		//
		// The permission was checked and the *relative standing* of actor and target never was, so a
		// PermMuteMembers holder could server-mute an administrator. The same shape sat on RemoveMember.
		// It goes at the top of the operation rather than beside the two fields the review happened to
		// name, because `nickname` is the third and renaming somebody above you is the same class of act.
		//
		// # The one carve-out, and what it deliberately does not cover
		//
		// Setting your own nickname is not a hierarchy question — nobody outranks themselves, so without
		// this no member could rename themselves at all, which is a regression on an endpoint M12 shipped
		// working.
		//
		// Self-mute and self-deafen get no such exemption. A self-mute is a voice_states flag the client
		// sets on itself (M25); this endpoint's mute and deaf are the *server-side* ones a moderator
		// applies, and letting somebody clear their own would undo the moderation the bits exist for.
		selfNicknameOnly := userID == actor.UserID && in.Mute == nil && in.Deaf == nil
		if !selfNicknameOnly {
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
			if !allowed.outranksMember(userID, standing) {
				return httpx.Errorf(ErrOutranked, "you cannot act on a member above you")
			}
		}

		row, err := q.UpdateGuildMember(ctx, db.UpdateGuildMemberParams{
			GuildID:       int64(guildID),
			UserID:        int64(userID),
			Nickname:      in.Nickname,
			ClearNickname: in.ClearNickname,
			Deaf:          in.Deaf,
			Mute:          in.Mute,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: update member: %w", err)
		}

		changes := map[string]any{}
		if in.ClearNickname {
			changes["nickname"] = nil
		} else if in.Nickname != nil {
			changes["nickname"] = *in.Nickname
		}
		if in.Deaf != nil {
			changes["deaf"] = *in.Deaf
		}
		if in.Mute != nil {
			changes["mute"] = *in.Mute
		}

		if err := s.writeAudit(ctx, q, guildID, actor.UserID, ActionMemberUpdate, &userID, changes); err != nil {
			return err
		}

		// The member's real roles, not an empty array. Returning nil made one schema mean two things
		// depending on which route produced it — and a client refreshing its cache from this 200, which is
		// the ordinary REST pattern the required `roles` field invites, would drop every role the member
		// holds. Unobservable at M12 only because nothing can grant a role yet; live the moment M13 can,
		// in an endpoint M13 does not touch.
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

		out = memberFromRow(row, held)
		return nil
	})
	if err != nil {
		return Member{}, err
	}

	return out, nil
}

// RemoveMember kicks a member from a guild.
//
// Leaving voluntarily is the same operation with the actor as the target, and it needs no permission —
// nobody should need PermKickMembers to walk out of a room. The owner cannot be removed by either route:
// they are ADR 0008's layer 2, and a guild whose owner is not a member has no layer 2 at all.
func (s *Service) RemoveMember(
	ctx context.Context, actor auth.Actor, guildID, userID snowflake.ID,
) error {
	return s.inTx(ctx, func(q *db.Queries) error {
		guild, err := q.GetGuild(ctx, int64(guildID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: get guild: %w", err)
		}

		if userID == actor.UserID {
			// Leaving. Still needs to be a member, which PermViewChannel establishes, and still writes an
			// audit entry — "who left" is exactly what an operator reads this log for.
			//
			// The one self-action exempt from the hierarchy entirely, and the reason is not that it is a
			// demotion — removing a role looked like one too and stopped being one when roles gained
			// channel denies. Leaving forfeits every permission in the guild at once, so it cannot be a
			// route to gaining one. That property, not the shape of the operation, is what earns it.
			if _, err := authorizeWith(ctx, q, actor, guildID, 0, roles.PermViewChannel); err != nil {
				return err
			}
		} else {
			allowed, err := authorizeWith(ctx, q, actor, guildID, 0, roles.PermKickMembers)
			if err != nil {
				return err
			}

			// Layer 4, and M12 shipped without it: a PermKickMembers holder could kick an administrator.
			// Below the guild read above, which established the guild exists, and below the permission
			// check, because it reads a row about somebody the caller may not be entitled to know about.
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
			if !allowed.outranksMember(userID, standing) {
				return httpx.Errorf(ErrOutranked, "you cannot act on a member above you")
			}
		}

		// The owner check comes *after* authorization, and the ordering is the whole point.
		//
		// It used to come first, so that an owner trying to leave got the real reason rather than a 403
		// suggesting a permission would help. That reasoning still holds and costs nothing here: an owner
		// passes PermViewChannel through the layer-2 short-circuit and reaches this line anyway.
		//
		// What the old order also did was answer 409 — a distinct status, with a public message — to a
		// caller who was not in the guild at all, where every other path answers 404. That made the pair
		// (guild exists, this account owns it) readable by anyone holding one guild id and a list of
		// candidate users, which is precisely the oracle roles.ErrNotAMember, authorize's two refusals and
		// even pathID's 404-on-unparseable exist to close. Found by a security review of this milestone.
		//
		// Nothing grants this: ownership transfer is the operation that makes removing an owner reachable,
		// and it is not this milestone.
		if snowflake.ID(guild.OwnerID) == userID {
			return httpx.Errorf(ErrCannotRemoveOwner,
				"transfer ownership before leaving a guild you own")
		}

		if err := s.writeAudit(ctx, q, guildID, actor.UserID, ActionMemberRemove, &userID, nil); err != nil {
			return err
		}

		// Their per-member overwrites go too. Nothing cascades, because target_id is polymorphic and so
		// cannot be a foreign key — and a leftover row is not merely clutter: rejoin the guild later and
		// applyOverwrites matches the member tier again, silently restoring a channel-level deny that
		// nothing in the UI or the audit log explains.
		//
		// Their role grants do cascade, through guild_member_roles' composite FK to guild_members.
		if err := q.DeleteOverwritesForTarget(ctx, db.DeleteOverwritesForTargetParams{
			GuildID:    int64(guildID),
			TargetType: roles.OverwriteTargetMember,
			TargetID:   int64(userID),
		}); err != nil {
			return fmt.Errorf("guilds: delete member overwrites: %w", err)
		}

		affected, err := q.RemoveGuildMember(ctx, db.RemoveGuildMemberParams{
			GuildID: int64(guildID),
			UserID:  int64(userID),
		})
		if err != nil {
			return fmt.Errorf("guilds: remove member: %w", err)
		}
		if affected == 0 {
			return httpx.ErrNotFound
		}

		return nil
	})
}
