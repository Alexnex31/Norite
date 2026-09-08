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
		need := roles.PermManageGuild
		if in.Mute != nil {
			need = need.Add(roles.PermMuteMembers)
		}
		if in.Deaf != nil {
			need = need.Add(roles.PermDeafenMembers)
		}

		if err := authorizeWith(ctx, q, actor, guildID, 0, need); err != nil {
			return err
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

		out = memberFromRow(row, nil)
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

		if snowflake.ID(guild.OwnerID) == userID {
			// Checked before the permission, so an owner trying to leave gets the real reason rather than
			// a 403 that suggests a permission would help. Nothing grants this: ownership transfer is the
			// operation that makes it reachable and it is not this milestone.
			return httpx.Errorf(ErrCannotRemoveOwner,
				"transfer ownership before leaving a guild you own")
		}

		if userID == actor.UserID {
			// Leaving. Still needs to be a member, which PermViewChannel establishes, and still writes an
			// audit entry — "who left" is exactly what an operator reads this log for.
			if err := authorizeWith(ctx, q, actor, guildID, 0, roles.PermViewChannel); err != nil {
				return err
			}
		} else if err := authorizeWith(ctx, q, actor, guildID, 0, roles.PermKickMembers); err != nil {
			return err
		}

		if err := s.writeAudit(ctx, q, guildID, actor.UserID, ActionMemberRemove, &userID, nil); err != nil {
			return err
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
