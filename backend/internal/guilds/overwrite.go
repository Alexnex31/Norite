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
// authorizeWith is given the channel id, so the decision's resolved permissions are what the caller holds
// *here*. At guild level the escalation check asks the wrong question on the one endpoint whose entire
// subject is per-channel permissions: a caller denied PermSendMessages in this channel would pass a
// guild-level check and allow it back to themselves, in one request.
func (s *Service) SetOverwrite(ctx context.Context, actor auth.Actor, in SetOverwriteInput) (Overwrite, error) {
	var out Overwrite

	err := s.inTx(ctx, func(q *db.Queries) error {
		_, guildID, allowed, err := s.authorizeChannel(ctx, q, actor, in.ChannelID, roles.PermManageRoles)
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
		// an overwrite's deny grants whatever it denied.
		//
		// The example this comment used to give is no longer reachable: it described a caller holding
		// PermManageRoles in a channel they cannot view, and authorizeChannel now refuses that caller 404
		// before this code runs. The check is unchanged and still right for every other bit — a caller
		// denied PermSendMessages here can blank the row that denies it and gain the permission — but the
		// scenario that motivated it was closed by this milestone's own first commit.
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
		_, guildID, allowed, err := s.authorizeChannel(ctx, q, actor, channelID, roles.PermManageRoles)
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

// authorizeChannel resolves a channel to its guild and authorizes a permission within it.
//
// # Viewing is required alongside whatever else is asked for
//
// Every caller gets PermViewChannel added to its `need`, and that is a correction to M13 rather than a
// convenience. M13 taught the channel listing to hide channels and did not teach these routes the same
// thing, so the two disagreed about whether a channel existed: a moderator holding PermManageChannels and
// denied PermViewChannel — an @everyone view-deny removes only the view bit — got the channel omitted
// from their listing and could still rename it, delete it, and write its permission overwrites.
//
// Found by driving a real guild by hand after M13 was tagged, and it needed that: every test that hid a
// channel hid it from somebody holding nothing else, and every test that managed one managed a channel
// that was visible. The divergence needs an actor who holds a management permission and lacks view in the
// same channel, which no unit test constructed and four review passes did not think to ask for.
//
// Requiring it here rather than at each call site is the same argument this package has made four times:
// a rule written as N call sites has N chances to miss one. It also composes with the refusal below —
// a caller who fails for want of the view bit is answered as though the channel were not there, which is
// what the listing already told them.
//
// The guild comes off the channel row and never from the caller, because these routes carry no guild in
// their path — the same reason UpdateChannel loads its own (rule 1). The channel id is passed to
// authorizeWith so the decision is what the caller holds in *this* channel.
func (s *Service) authorizeChannel(
	ctx context.Context, q *db.Queries, actor auth.Actor, channelID snowflake.ID, need roles.Permission,
) (db.Channel, snowflake.ID, decision, error) {
	row, err := q.GetChannel(ctx, int64(channelID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Channel{}, 0, decision{}, httpx.ErrNotFound
		}
		return db.Channel{}, 0, decision{}, fmt.Errorf("guilds: get channel: %w", err)
	}

	guildID, err := guildOf(row)
	if err != nil {
		return db.Channel{}, 0, decision{}, err
	}

	allowed, err := authorizeWith(ctx, q, actor, guildID, channelID, need.Add(roles.PermViewChannel))
	if err != nil {
		// A member of the guild who cannot *see* this channel is refused as though it were not there.
		//
		// authorizeWith answers 403 for a member lacking a permission and 404 for a non-member, and that
		// split was right while every channel in a member's guild was listed to them: they already knew
		// it existed, so naming it disclosed nothing. This milestone's channel listing hides channels, so
		// the two answers became an oracle — 403 confirms a hidden channel to anybody holding its id,
		// which is exactly what the filter withholds.
		//
		// Only the view permission is consulted here. Somebody who can see the channel and merely lacks
		// PermManageRoles still gets 403, because for them the channel's existence was never a secret.
		if errors.Is(err, httpx.ErrForbidden) {
			_, viewErr := authorizeWith(ctx, q, actor, guildID, channelID, roles.PermViewChannel)
			switch {
			case viewErr == nil:
				// They can see it, so its existence was never a secret. The original 403 stands.
			case errors.Is(viewErr, httpx.ErrForbidden), errors.Is(viewErr, httpx.ErrNotFound):
				return db.Channel{}, 0, decision{}, httpx.ErrNotFound
			default:
				// A database failure during the second check is not evidence about the channel. Reporting
				// it as missing would turn a connection blip into a 404 the caller would cache as truth.
				return db.Channel{}, 0, decision{}, viewErr
			}
		}
		return db.Channel{}, 0, decision{}, err
	}

	return row, guildID, allowed, nil
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

// refuseRemovingOverwritesFor refuses a caller who does not hold the permissions a target's overwrites
// carry, in the channels those overwrites sit on.
//
// # Why deleting a role is an overwrite operation
//
// DeleteOverwrite refuses to remove a single row whose bits the caller does not hold, because removing a
// deny grants whatever it denied. Deleting the *role* that row names reaches the same outcome on every
// channel at once, and reached it behind a guild-level permission check alone — so a member holding
// PermManageRoles could escape a channel restriction by deleting the role carrying it, having been
// refused a moment earlier when they tried to delete the overwrite directly. Found by a security review.
//
// **Two of the three paths that delete overwrites use this; RemoveMember deliberately does not.** Kicking
// somebody deletes their member-tier rows, which is the same deletion — but guarding it would make a
// member unkickable by holding an overwrite whose bits the moderator lacks, trading an escalation for a
// denial of moderation. The residual is that a kick clears a member-tier deny, which only matters once
// that member can come back: the rejoin question, routed to M57 and M72a where a join path first exists.
//
// # Resolved per channel, which is the only scope that answers it
//
// A guild-level check does not close this, and that is worth stating because it is the cheaper thing to
// write. The interesting caller holds a permission guild-wide and is denied it in one channel by exactly
// the overwrite being removed: at guild level they hold the bit and pass. So each affected channel is
// resolved from the caller's existing resolution and that channel's own rows — no second authority
// query, one read for the overwrites.
//
// An Instance Admin passes by layer 1. The owner and any administrator pass through InChannel's
// short-circuit, which is where layers 2 and 3 live.
func (s *Service) refuseRemovingOverwritesFor(
	ctx context.Context,
	q *db.Queries,
	allowed decision,
	guildID snowflake.ID,
	targetType int16,
	targetID snowflake.ID,
) error {
	affected, err := q.ListOverwritesForTarget(ctx, db.ListOverwritesForTargetParams{
		GuildID:    int64(guildID),
		TargetType: targetType,
		TargetID:   int64(targetID),
	})
	if err != nil {
		return fmt.Errorf("guilds: list overwrites for target: %w", err)
	}
	if len(affected) == 0 {
		return nil
	}

	channelIDs := make([]int64, 0, len(affected))
	for _, ow := range affected {
		channelIDs = append(channelIDs, ow.ChannelID)
	}

	// Every overwrite on the affected channels, not only the target's: resolving a channel needs the
	// @everyone tier and the caller's own role tiers as well as the row being removed.
	surrounding, err := q.ListGuildPermissionOverwrites(ctx, db.ListGuildPermissionOverwritesParams{
		ChannelIds: channelIDs,
		GuildID:    int64(guildID),
	})
	if err != nil {
		return fmt.Errorf("guilds: list overwrites for removal check: %w", err)
	}

	byChannel := make(map[snowflake.ID][]db.PermissionOverwrite, len(affected))
	for _, ow := range surrounding {
		id := snowflake.ID(ow.ChannelID)
		byChannel[id] = append(byChannel[id], ow)
	}

	for _, ow := range affected {
		channelID := snowflake.ID(ow.ChannelID)
		removing := roles.PermissionFromInt64(ow.Allow).Add(roles.PermissionFromInt64(ow.Deny))
		if allowed.allowsInChannel(channelID, byChannel[channelID], removing) {
			continue
		}
		// Names neither the channel nor the bits, for the reason every refusal in this package names
		// neither: the caller cannot necessarily see the channel being talked about.
		return httpx.Errorf(httpx.ErrForbidden,
			"this would remove a permission overwrite you do not hold the permissions for")
	}

	return nil
}
