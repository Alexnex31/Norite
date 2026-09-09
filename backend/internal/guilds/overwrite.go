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

// maxOverwritesPerChannel bounds what one channel may accumulate.
//
// The same decision channels and roles already carry: a list that cannot be paginated is bounded at
// creation instead. Overwrites are read whole — once per channel before every channel-scoped mutation,
// and once per guild on the channel listing — so an unbounded set is work on two hot paths.
//
// A constant rather than a config value, unlike the channel and role ceilings. Those bound what an
// instance operator might legitimately want more of; this bounds a per-channel access-control list, and a
// channel needing more than fifty distinct overwrites is a guild that wants a role instead. Discord's own
// limit is a similar order.
const maxOverwritesPerChannel = 50

// SetOverwriteInput is the request to create or replace one overwrite.
type SetOverwriteInput struct {
	ChannelID  snowflake.ID
	TargetType int16
	TargetID   snowflake.ID
	Allow      roles.Permission
	Deny       roles.Permission
}

// SetOverwrite writes a channel permission overwrite — ADR 0008 layer 5, finally given rows to resolve.
//
// # Three checks, and none of them is the permission check
//
// PermManageRoles is what gets a caller into this function. What stops it being every permission in the
// guild is the rest:
//
//   - the target must exist *in this guild*, verified per type rather than either-or (a request naming a
//     role id while claiming a member target would otherwise write a row no client can explain);
//   - the caller must outrank the target, because an overwrite is an act on the thing it names;
//   - the caller may not touch a permission bit they do not hold, over the union of what the request
//     changes rather than only what it writes.
//
// # Resolved in the channel, not in the guild
//
// authorizeWith is given the channel id, so decision.permissions is what the caller holds *here*. At
// guild level the escalation check asks the wrong question on the one endpoint whose entire subject is
// per-channel permissions: a caller denied PermSendMessages in this channel would pass a guild-level
// check and allow it back to themselves, in one request.
func (s *Service) SetOverwrite(ctx context.Context, actor auth.Actor, in SetOverwriteInput) (Overwrite, error) {
	var out Overwrite

	err := s.inTx(ctx, func(q *db.Queries) error {
		guildID, allowed, err := s.authorizeChannel(ctx, q, actor, in.ChannelID)
		if err != nil {
			return err
		}

		if err := s.checkOverwriteTarget(
			ctx, q, actor, allowed, guildID, in.TargetType, in.TargetID,
		); err != nil {
			return err
		}

		// The union of what this request changes, which is not the same as what it writes.
		//
		// An escalation check on the new value alone leaves the *removal* of a bit unchecked, and removing
		// an overwrite's deny grants whatever it denied. A caller who holds PermManageRoles in a channel
		// they cannot view — an ordinary configuration, since the deny that hides it removes viewing and
		// not managing — could otherwise blank the row and see the channel.
		existing, err := q.GetPermissionOverwrite(ctx, db.GetPermissionOverwriteParams{
			ChannelID:  int64(in.ChannelID),
			TargetType: in.TargetType,
			TargetID:   int64(in.TargetID),
			GuildID:    int64(guildID),
		})
		switch {
		case err == nil:
			changed := roles.PermissionFromInt64(existing.Allow).
				Add(roles.PermissionFromInt64(existing.Deny)).
				Add(in.Allow).Add(in.Deny)
			if err := refuseEscalation(allowed, changed); err != nil {
				return err
			}
		case errors.Is(err, pgx.ErrNoRows):
			// Nothing to replace, so the union is what is being written.
			if err := refuseEscalation(allowed, in.Allow.Add(in.Deny)); err != nil {
				return err
			}
			// The ceiling, checked only when a row is actually being added — replacing one is not growth,
			// and refusing it would strand a channel that reached the limit with no way to edit what is
			// already there. After authorization, so a stranger cannot measure how full a channel is.
			count, err := q.CountChannelPermissionOverwrites(ctx, int64(in.ChannelID))
			if err != nil {
				return fmt.Errorf("guilds: count overwrites: %w", err)
			}
			if count >= maxOverwritesPerChannel {
				return httpx.Errorf(ErrChannelFull,
					"a channel may carry at most %d permission overwrites", maxOverwritesPerChannel)
			}
		default:
			return fmt.Errorf("guilds: get overwrite: %w", err)
		}

		row, err := q.UpsertPermissionOverwrite(ctx, db.UpsertPermissionOverwriteParams{
			TargetType: in.TargetType,
			TargetID:   int64(in.TargetID),
			Allow:      in.Allow.Int64(),
			Deny:       in.Deny.Int64(),
			ChannelID:  int64(in.ChannelID),
			GuildID:    int64(guildID),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// The channel stopped belonging to this guild between the read and the write, which means
				// it was deleted. Same answer a stranger gets.
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: upsert overwrite: %w", err)
		}

		target := in.TargetID
		if err := s.writeAudit(ctx, q, guildID, actor.UserID, ActionOverwriteSet, &target, map[string]any{
			"channel_id": in.ChannelID.String(),
			"type":       in.TargetType,
			"allow":      in.Allow.Int64(),
			"deny":       in.Deny.Int64(),
		}); err != nil {
			return err
		}

		out = overwriteFromRow(row)
		return nil
	})
	if err != nil {
		return Overwrite{}, err
	}

	return out, nil
}

// DeleteOverwrite removes one overwrite.
//
// Every check SetOverwrite makes, including the escalation one — which is the half that looks unnecessary
// and is not. Deleting an overwrite changes exactly the permissions it allowed or denied, so the bits it
// carried are the bits this request changes, and a caller who may not touch them may not remove them.
func (s *Service) DeleteOverwrite(
	ctx context.Context, actor auth.Actor, channelID snowflake.ID, targetType int16, targetID snowflake.ID,
) error {
	return s.inTx(ctx, func(q *db.Queries) error {
		guildID, allowed, err := s.authorizeChannel(ctx, q, actor, channelID)
		if err != nil {
			return err
		}

		existing, err := q.GetPermissionOverwrite(ctx, db.GetPermissionOverwriteParams{
			ChannelID:  int64(channelID),
			TargetType: targetType,
			TargetID:   int64(targetID),
			GuildID:    int64(guildID),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: get overwrite: %w", err)
		}

		// The target is checked on the way out as well as on the way in. An overwrite protecting a role
		// above the caller is that role's protection, and removing it is acting on them.
		if err := s.checkOverwriteTarget(
			ctx, q, actor, allowed, guildID, targetType, targetID,
		); err != nil {
			return err
		}

		if err := refuseEscalation(allowed, roles.PermissionFromInt64(existing.Allow).
			Add(roles.PermissionFromInt64(existing.Deny))); err != nil {
			return err
		}

		if err := s.writeAudit(ctx, q, guildID, actor.UserID, ActionOverwriteDelete, &targetID, map[string]any{
			"channel_id": channelID.String(),
			"type":       targetType,
		}); err != nil {
			return err
		}

		affected, err := q.DeletePermissionOverwrite(ctx, db.DeletePermissionOverwriteParams{
			GuildID:    int64(guildID),
			ChannelID:  int64(channelID),
			TargetType: targetType,
			TargetID:   int64(targetID),
		})
		if err != nil {
			return fmt.Errorf("guilds: delete overwrite: %w", err)
		}
		if affected == 0 {
			return httpx.ErrNotFound
		}

		return nil
	})
}

// authorizeChannel resolves a channel to its guild and authorizes PermManageRoles within it.
//
// The guild comes off the channel row and never from the caller, because these routes carry no guild in
// their path — the same reason UpdateChannel loads its own (rule 1). The channel id is passed to
// authorizeWith so the decision is what the caller holds in *this* channel.
func (s *Service) authorizeChannel(
	ctx context.Context, q *db.Queries, actor auth.Actor, channelID snowflake.ID,
) (snowflake.ID, decision, error) {
	row, err := q.GetChannel(ctx, int64(channelID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, decision{}, httpx.ErrNotFound
		}
		return 0, decision{}, fmt.Errorf("guilds: get channel: %w", err)
	}

	guildID, err := guildOf(row)
	if err != nil {
		return 0, decision{}, err
	}

	allowed, err := authorizeWith(ctx, q, actor, guildID, channelID, roles.PermManageRoles)
	if err != nil {
		return 0, decision{}, err
	}

	return guildID, allowed, nil
}

// checkOverwriteTarget verifies that an overwrite names something real in this guild, and that the caller
// outranks it.
//
// # Type-directed, not either-or
//
// "this id is a role in the guild *or* a member of it" accepts a request that says `type: member` while
// naming a role id. Snowflakes are unique so nothing collides and the row is inert — but it is a row in a
// permission table that no client can explain, written through the endpoint whose whole job is to be
// auditable. One check per branch.
//
// # Below the authorization check, always
//
// Every refusal here reads a loaded row, so it is an authorization question rather than an input one.
// M12 shipped two endpoints that answered a non-member with a public error naming something they could
// not see, and both were corrected by review; this function is only ever called after authorizeChannel.
func (s *Service) checkOverwriteTarget(
	ctx context.Context,
	q *db.Queries,
	actor auth.Actor,
	allowed decision,
	guildID snowflake.ID,
	targetType int16,
	targetID snowflake.ID,
) error {
	switch targetType {
	case roles.OverwriteTargetRole:
		role, err := q.GetRole(ctx, db.GetRoleParams{ID: int64(targetID), GuildID: int64(guildID)})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: get overwrite target role: %w", err)
		}
		if !allowed.outranks(role.Position) {
			return httpx.Errorf(ErrOutranked, "you cannot manage a role above your own")
		}

	case roles.OverwriteTargetMember:
		standing, err := q.GetMemberHighestRolePosition(ctx, db.GetMemberHighestRolePositionParams{
			GuildID: int64(guildID),
			UserID:  int64(targetID),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Not a member of this guild. The same 404 a role from elsewhere gets, and the reason
				// GetMemberHighestRolePosition returns no row rather than a coalesced zero.
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: get overwrite target standing: %w", err)
		}
		// Your own overwrite is not a hierarchy question, and refusing it is the answer the strict rule
		// gives on its own: nobody outranks themselves, so without this carve-out no member could ever
		// write or remove a per-member overwrite naming themselves — including a moderator setting up
		// their own access to a channel they are configuring.
		//
		// Safe to skip because the standing check is not what bounds this case. The escalation check is,
		// and it is measured against what the caller holds *in this channel*: they can only name bits they
		// already have here, so allowing themselves something is a no-op and denying themselves is their
		// own business. Lifting a deny that sits on them is the case that matters, and it is refused by
		// that same check rather than by this one — the bits being removed are bits they do not hold.
		//
		// Same shape as self-unassignment of a role: the target comparison is skipped, the check that
		// actually bounds the operation is not.
		if targetID != actor.UserID && !allowed.outranksMember(targetID, standing) {
			return httpx.Errorf(ErrOutranked, "you cannot act on a member above you")
		}

	default:
		// A type roles.applyOverwrites would silently ignore. Refused rather than stored, so the table
		// cannot accumulate rows that resolve to nothing and that no cleanup path knows about.
		return httpx.Errorf(httpx.ErrBadRequest, "type must be 0 (role) or 1 (member)")
	}

	return nil
}
