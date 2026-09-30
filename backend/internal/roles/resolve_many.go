// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package roles

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// ResolveMany is Resolve for many accounts in one guild at once, which is the gateway's question at fan-out
// time (M18): of the accounts connected here, which may see this event?
//
// It answers exactly what Resolve would for each account, and a test holds the two equal over a guild
// exercising every layer. Same layers, same order, same applyOverwrites — owner first and ahead of
// membership, then membership, then the OR of every held role including @everyone, then the administrator
// short-circuit, then the channel's overwrites. Only the queries differ: three round trips for any number
// of accounts, where calling Resolve per recipient would be two per recipient, per event.
//
// Nothing is cached, and that is the M18 decision rather than an omission (docs/architecture.md, "It is not
// cached"): every call reads the rows as they are now.
//
// channelID zero means a guild-level question, answered with the guild-wide permissions. overwrites, when
// non-nil, are used instead of reading the channel's, for a channel whose rows no longer exist; see
// dispatch.Event.Overwrites.
//
// The result holds an entry for each account that is a member, or the owner. An account absent from it is
// not in the guild, and gets nothing.
func ResolveMany(
	ctx context.Context, q db.Querier, guildID, channelID snowflake.ID, userIDs []snowflake.ID,
	overwrites []db.PermissionOverwrite,
) (map[snowflake.ID]Permission, error) {
	out := make(map[snowflake.ID]Permission, len(userIDs))
	if len(userIDs) == 0 {
		return out, nil
	}

	base, err := q.GetGuildAuthorityBase(ctx, int64(guildID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return out, nil
		}
		return nil, fmt.Errorf("roles: load guild authority base: %w", err)
	}

	ids := make([]int64, len(userIDs))
	for i, id := range userIDs {
		ids[i] = int64(id)
	}
	rows, err := q.ListMemberRolesForUsers(ctx, db.ListMemberRolesForUsersParams{UserIds: ids, GuildID: int64(guildID)})
	if err != nil {
		return nil, fmt.Errorf("roles: load member roles: %w", err)
	}

	type member struct {
		perms Permission
		held  map[int64]struct{}
	}
	members := make(map[snowflake.ID]*member, len(userIDs))
	for _, row := range rows {
		if !row.IsMember {
			continue
		}
		id := snowflake.ID(row.UserID)
		m, ok := members[id]
		if !ok {
			// Layer 4's floor: every member holds @everyone.
			m = &member{
				perms: PermissionFromInt64(base.EveryonePermissions),
				held:  map[int64]struct{}{base.EveryoneRoleID: {}},
			}
			members[id] = m
		}
		if row.RoleID != nil {
			m.held[*row.RoleID] = struct{}{}
			if row.RolePermissions != nil {
				m.perms = m.perms.Add(PermissionFromInt64(*row.RolePermissions))
			}
		}
	}

	owner := snowflake.ID(base.OwnerID)
	needOverwrites := channelID != 0 && overwrites == nil
	for _, id := range userIDs {
		// Layer 2, ahead of membership exactly as Resolve orders it.
		if id == owner {
			out[id] = permAll
			continue
		}
		m, ok := members[id]
		if !ok {
			continue
		}
		// Layer 3.
		if m.perms.Has(PermAdministrator) {
			out[id] = permAll
			continue
		}
		if channelID != 0 && needOverwrites {
			overwrites, err = q.ListChannelPermissionOverwrites(ctx, db.ListChannelPermissionOverwritesParams{
				ChannelID: int64(channelID),
				GuildID:   int64(guildID),
			})
			if err != nil {
				return nil, fmt.Errorf("roles: load channel overwrites: %w", err)
			}
			needOverwrites = false
		}
		if channelID == 0 {
			out[id] = m.perms
			continue
		}
		// Layer 5.
		out[id] = applyOverwrites(m.perms, channelID, overwrites, m.held, base.EveryoneRoleID, id)
	}
	return out, nil
}
