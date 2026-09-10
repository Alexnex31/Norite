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

// isGuildChannelType reports whether a guild may contain a channel of this type.
//
// DM and GROUP_DM belong to no guild and are M57's. GUILD_ANNOUNCEMENT and GUILD_STAGE_VOICE are reserved
// values that must stay in the schema (rule 10) and are not yet buildable. PUBLIC_MATCHMAKING is created
// by the matchmaking service, not by a guild owner.
//
// A switch rather than a range check, because the values are a vocabulary rather than an interval — the
// permitted set is not contiguous and never will be. And a switch rather than a package-level map, which
// is what this was: a map at package scope is writable from anywhere in the package, so
// `delete(guildChannelTypes, ChannelGuildVoice)` compiles and would silently disable voice channels
// instance-wide. A switch is immutable by construction and compiles to a jump table.
func isGuildChannelType(t int16) bool {
	switch t {
	case ChannelGuildText, ChannelGuildVoice, ChannelGuildCategory:
		return true
	default:
		return false
	}
}

func channelFromRow(row db.Channel) Channel {
	return Channel{
		// Empty rather than nil, and overwritten by callers that have the rows. A channel with no
		// overwrites and a channel whose overwrites were not loaded must not be distinguishable on the
		// wire, because a client cannot tell which it is looking at.
		PermissionOverwrites: []Overwrite{},

		ID:            snowflake.ID(row.ID),
		GuildID:       idPtr(row.GuildID),
		Type:          row.Type,
		ParentID:      idPtr(row.ParentID),
		Name:          row.Name,
		Topic:         row.Topic,
		Position:      row.Position,
		NSFW:          row.Nsfw,
		LastMessageID: idPtr(row.LastMessageID),
		Bitrate:       row.Bitrate,
		UserLimit:     row.UserLimit,
		CreatedAt:     row.CreatedAt.Time,
		UpdatedAt:     row.UpdatedAt.Time,
	}
}

// channelFromListRow adapts the list query's own row struct.
//
// ListGuildChannels enumerates its columns to leave topic_search behind, so sqlc gives it a row type of
// its own rather than db.Channel. Both are converted through channelFromRow rather than by a second copy
// of the same thirteen assignments: two copies drift, and the failure mode is a field that is populated on
// create and silently null on the listing, which a test exercising one path cannot see.
func channelFromListRow(row db.ListGuildChannelsRow) Channel {
	return channelFromRow(db.Channel{
		ID:            row.ID,
		GuildID:       row.GuildID,
		Type:          row.Type,
		ParentID:      row.ParentID,
		Name:          row.Name,
		Topic:         row.Topic,
		Position:      row.Position,
		Nsfw:          row.Nsfw,
		LastMessageID: row.LastMessageID,
		Bitrate:       row.Bitrate,
		UserLimit:     row.UserLimit,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
	})
}

// ListChannels returns the guild's channels the caller can see, in position order.
//
// # One resolution, one overwrite read, N filters
//
// Rule 7 names this a hot path, and the obvious implementation is the N+1 §15.2 warns about: call
// roles.Resolve once per channel and let it fetch that channel's overwrites. The authority half of that
// resolution is identical for every channel in the guild, so this resolves once at guild level, reads
// every channel's overwrites in one query, and applies them per channel from that single result.
//
// Two queries rather than 2N. Measured at the ceiling that matters, because the shape of the overwrite
// read is not obvious: written as a join on guild_id it costs 13.682 ms on a 500-channel guild, where
// passing the channel ids the listing has already loaded costs 0.536 ms — the planner abandons the nested
// loop and sequentially scans the whole overwrite table. See ListGuildPermissionOverwrites.
//
// # Who is not filtered
//
// Three short-circuits see everything, not two. An Instance Admin holds layer 1 and is never resolved
// against the guild at all, so there is no resolution to filter with and the check is explicit here. The
// owner (layer 2) and any member holding PermAdministrator (layer 3) are handled inside
// Resolution.InChannel, which returns their permissions unchanged — that check lives there rather than
// here so a caller cannot filter a channel away from the one account that must always see it.
func (s *Service) ListChannels(
	ctx context.Context, actor auth.Actor, guildID snowflake.ID,
) ([]Channel, error) {
	// Membership, not guild-level view. The filter below is what decides visibility, one channel at a
	// time, and gating the whole listing on a guild-level PermViewChannel makes the two contradict: a
	// member whose view comes from a channel overwrite — @everyone withholding it at role level and one
	// welcome channel allowing it back, which is an ordinary way to build a guild — resolves to nothing
	// here and is refused the listing the filter would have answered correctly.
	//
	// Permission.Has(0) is true by design, so this establishes membership and asserts nothing else. A
	// non-member is still refused by authorizeWith, with the 404 every other guild route gives them.
	allowed, err := authorizeWith(ctx, s.queries, actor, guildID, 0, 0)
	if err != nil {
		return nil, err
	}

	id := int64(guildID)
	rows, err := s.queries.ListGuildChannels(ctx, &id)
	if err != nil {
		return nil, fmt.Errorf("guilds: list channels: %w", err)
	}

	out := make([]Channel, 0, len(rows))
	channelIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		out = append(out, channelFromListRow(row))
		channelIDs = append(channelIDs, row.ID)
	}

	if len(channelIDs) == 0 {
		return out, nil
	}

	overwrites, err := s.queries.ListGuildPermissionOverwrites(ctx, db.ListGuildPermissionOverwritesParams{
		ChannelIds: channelIDs,
		GuildID:    int64(guildID),
	})
	if err != nil {
		return nil, fmt.Errorf("guilds: list guild overwrites: %w", err)
	}

	// Grouped once, and the grouping is what keeps this linear.
	//
	// InChannel skips rows belonging to other channels, so handing it the whole guild's slice is correct
	// and quadratic: every channel walks every overwrite in the guild. Measured on a guild at the
	// 500-channel ceiling with three overwrites each, 1,500 rows in total:
	//
	//   whole slice   757,295 ns/op
	//   grouped       130,493 ns/op
	//
	// Ten times the channels cost a hundred and nine times the work, against under nine for the grouped
	// form. The absolute number is small at ordinary sizes — 7 us on a fifty-channel guild — but at the
	// ceiling it is twice what the database round trip costs, on a path rule 7 names.
	//
	// The grouping itself is free: this loop replaced one that grouped the same rows into the wire type,
	// so the pass was already being paid for. Converting is now done only for channels that survive the
	// filter, which is the other half of the saving.
	byChannel := make(map[snowflake.ID][]db.PermissionOverwrite, len(out))
	for _, ow := range overwrites {
		id := snowflake.ID(ow.ChannelID)
		byChannel[id] = append(byChannel[id], ow)
	}

	visible := out[:0]
	for _, ch := range out {
		rows := byChannel[ch.ID]

		// Layer 1 is outside the guild and was never resolved, so the tier is asked first — inside
		// allowsInChannel rather than here, so a third caller cannot forget it.
		if !allowed.allowsInChannel(ch.ID, rows, roles.PermViewChannel) {
			continue
		}

		ch.PermissionOverwrites = make([]Overwrite, 0, len(rows))
		for _, ow := range rows {
			ch.PermissionOverwrites = append(ch.PermissionOverwrites, overwriteFromRow(ow))
		}
		visible = append(visible, ch)
	}

	return visible, nil
}

// CreateChannelInput is the request to create a channel.
type CreateChannelInput struct {
	Name      string
	Type      int16
	Topic     *string
	ParentID  *snowflake.ID
	Position  int32
	NSFW      bool
	Bitrate   *int32
	UserLimit *int32
}

// CreateChannel adds a channel to a guild.
func (s *Service) CreateChannel(
	ctx context.Context, actor auth.Actor, guildID snowflake.ID, in CreateChannelInput,
) (Channel, error) {
	if !isGuildChannelType(in.Type) {
		return Channel{}, httpx.Errorf(ErrUnsupportedChannelType,
			"channel type %d cannot be created in a guild", in.Type)
	}

	// bitrate and user_limit belong to voice and to nothing else. The schema says "NULL for every other
	// type" and the contract says "Voice channels only" — without this, a text channel created with a
	// bitrate keeps it, and the response contradicts both documents.
	//
	// Refused rather than silently dropped: a client sending them on a text channel has misunderstood
	// something, and quietly ignoring the field would leave it believing the value took.
	if in.Type != ChannelGuildVoice && (in.Bitrate != nil || in.UserLimit != nil) {
		return Channel{}, httpx.Errorf(httpx.ErrBadRequest,
			"bitrate and user_limit apply only to voice channels")
	}

	channelID, err := s.ids.Next()
	if err != nil {
		return Channel{}, fmt.Errorf("guilds: mint channel id: %w", err)
	}

	var out Channel

	err = s.inTx(ctx, func(q *db.Queries) error {
		if _, err := authorizeWith(ctx, q, actor, guildID, 0, roles.PermManageChannels); err != nil {
			return err
		}

		// The ceiling, checked after authorization so a non-member cannot learn how full a guild is.
		guildForCount := int64(guildID)
		count, err := q.CountGuildChannels(ctx, &guildForCount)
		if err != nil {
			return fmt.Errorf("guilds: count channels: %w", err)
		}
		if count >= int64(s.maxChannelsPerGuild) {
			return httpx.Errorf(ErrGuildFull,
				"a guild may hold at most %d channels", s.maxChannelsPerGuild)
		}

		// A parent must be a category *in this guild*. Checked rather than trusted, because parent_id
		// arrives from the caller and a channel is otherwise free to nest under one in a guild the caller
		// has no permissions in — which would put its children behind that guild's overwrites (rule 1).
		if in.ParentID != nil {
			// One message for every way a parent can be unusable, and that is deliberate.
			//
			// Three distinct messages — "not a channel in this guild", "must name a category", "a category
			// cannot be nested" — tell a caller iterating snowflakes which ids are live channels in their
			// guild and what type each one is. That was harmless while every channel in a member's guild
			// was listed to them. This milestone's listing hides channels, so the distinction became the
			// same oracle the overwrite routes and the channel routes were both corrected for.
			//
			// The view check below is what makes them indistinguishable: a caller who cannot see the
			// parent gets the generic answer before its type is ever consulted, so the type-specific
			// messages only ever reach somebody the channel was not hidden from.
			const badParent = "parent_id does not name a category in this guild you can use"

			parent, err := q.GetChannel(ctx, int64(*in.ParentID))
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				return httpx.Errorf(httpx.ErrBadRequest, "%s", badParent)
			case err != nil:
				return fmt.Errorf("guilds: get parent channel: %w", err)
			case parent.GuildID == nil || snowflake.ID(*parent.GuildID) != guildID:
				return httpx.Errorf(httpx.ErrBadRequest, "%s", badParent)
			}

			if _, err := authorizeWith(
				ctx, q, actor, guildID, *in.ParentID, roles.PermViewChannel,
			); err != nil {
				return httpx.Errorf(httpx.ErrBadRequest, "%s", badParent)
			}

			switch {
			case parent.Type != ChannelGuildCategory:
				return httpx.Errorf(httpx.ErrBadRequest, "%s", badParent)
			case in.Type == ChannelGuildCategory:
				// A category inside a category. The reciprocal of the check above and easy to omit,
				// because the parent is valid — it is the *child* that is wrong. The channel list is a
				// flat position-ordered list with one level of nesting, and the TUI's channel pane has no
				// shape for a deeper tree, so this would produce data no client can render.
				return httpx.Errorf(httpx.ErrBadRequest, "a category cannot be nested inside another")
			}

			// Authorized against the category as well as against the guild.
			//
			// The check above resolves PermManageChannels at guild level, which is all there was to
			// resolve while nothing could write an overwrite. It is not all there is now: a member denied
			// PermManageChannels *inside* a category would otherwise create channels in it on a
			// guild-wide grant, and then own them — PATCH and DELETE resolve against the new channel's own
			// overwrite set, which is whatever this creation gives it. UpdateChannel and DeleteChannel
			// have always resolved per-channel; creation was the one that could not, because there was no
			// channel yet. Its parent is the answer.
			//
			// Third of three steps, and the order is the whole of it: authorize at guild level, load and
			// validate the parent, then authorize against it. Authorizing against the parent *instead*
			// would load a caller-supplied id before any membership check, and the refusals above name a
			// channel — so a stranger could tell "not a category in this guild" from a plain 404.
			if _, err := authorizeWith(
				ctx, q, actor, guildID, *in.ParentID, roles.PermManageChannels,
			); err != nil {
				return err
			}
		}

		guild := int64(guildID)
		var parent *int64
		if in.ParentID != nil {
			v := int64(*in.ParentID)
			parent = &v
		}

		row, err := q.CreateChannel(ctx, db.CreateChannelParams{
			ID:        int64(channelID),
			GuildID:   &guild,
			Type:      in.Type,
			ParentID:  parent,
			Name:      &in.Name,
			Topic:     in.Topic,
			Position:  in.Position,
			Nsfw:      in.NSFW,
			Bitrate:   in.Bitrate,
			UserLimit: in.UserLimit,
		})
		if err != nil {
			return fmt.Errorf("guilds: create channel: %w", err)
		}

		// Inheritance, and it is a copy rather than a lookup at resolution time.
		//
		// Discord's model, confirmed: a channel does not follow its category's later permission changes,
		// so "sync permissions with category" is a client re-copying through the overwrite endpoints
		// rather than a flag anything stores. roles.Resolve therefore keeps reading exactly one channel's
		// rows and never walks to a parent, which would put a second query on the check that runs before
		// every mutation.
		//
		// Without this a channel created inside a locked-down category is readable by everyone the moment
		// it exists — the category's deny simply does not apply to it. Unreachable at M12 because no
		// overwrite could exist; live from the moment this milestone's PUT endpoint shipped, which is
		// why a security review found it here rather than in a later milestone.
		//
		// Not escalation-checked, deliberately, and it is the one place in this package where copying
		// bits the caller does not hold is right: the rows replicate a configuration the guild already
		// authored rather than authoring a new one, and refusing them would make inheritance fail exactly
		// under the locked-down category it is most wanted under. What is checked is the creator's
		// authority over that parent, immediately above.
		if in.ParentID != nil {
			if err := q.CopyChannelOverwrites(ctx, db.CopyChannelOverwritesParams{
				ChannelID:       int64(channelID),
				SourceChannelID: int64(*in.ParentID),
				GuildID:         int64(guildID),
			}); err != nil {
				return fmt.Errorf("guilds: copy category overwrites: %w", err)
			}
		}

		if err := s.writeAudit(ctx, q, guildID, actor.UserID, ActionChannelCreate, &channelID, map[string]any{
			"name": in.Name,
			"type": in.Type,
		}); err != nil {
			return err
		}

		out = channelFromRow(row)

		// The copied rows, so the response carries what the channel actually has rather than an empty
		// array a client would cache as the truth.
		if in.ParentID != nil {
			copied, err := q.ListChannelPermissionOverwrites(ctx, db.ListChannelPermissionOverwritesParams{
				ChannelID: int64(channelID),
				GuildID:   int64(guildID),
			})
			if err != nil {
				return fmt.Errorf("guilds: list copied overwrites: %w", err)
			}
			for _, ow := range copied {
				out.PermissionOverwrites = append(out.PermissionOverwrites, overwriteFromRow(ow))
			}
		}

		return nil
	})
	if err != nil {
		return Channel{}, err
	}

	return out, nil
}

// UpdateChannelInput is a partial update. A nil field is left alone.
type UpdateChannelInput struct {
	Name       *string
	Topic      *string
	ClearTopic bool
	Position   *int32
	NSFW       *bool
	Bitrate    *int32
	UserLimit  *int32
}

// UpdateChannel changes a channel's own fields.
//
// # The cross-scope check rule 1 names
//
// PATCH /channels/{channel_id} carries no guild in its path. So the guild this is authorized against is
// read from the channel row itself, never from anything the caller supplied — there is nothing the caller
// supplied to read. Passing a guild id in the body and trusting it would be precisely the
// "client-supplied ID without verifying it belongs to the actor's claimed context" rule 1 forbids: a
// member with PermManageChannels in guild A could rename a channel in guild B by naming A.
//
// The load-then-authorize ordering is the only one available, and it is why GetChannel takes no guild
// parameter. Scoping that read by a caller-supplied guild would be trusting the value the check exists to
// verify.
func (s *Service) UpdateChannel(
	ctx context.Context, actor auth.Actor, channelID snowflake.ID, in UpdateChannelInput,
) (Channel, error) {
	var out Channel

	err := s.inTx(ctx, func(q *db.Queries) error {
		// The same resolve-and-authorize the overwrite endpoints use, and it carries the refusal that
		// matters here: a member who cannot *see* this channel is answered as though it were not there.
		// The listing hides channels now, so a 403 for a hidden one against a 404 for a nonexistent one
		// is an oracle confirming exactly what the filter withholds — and that reasoning applies to these
		// routes as much as to the permission ones, which had it and these did not.
		// One read. authorizeChannel loads the row to find its guild and hands it back, so nothing here
		// reads it twice — DeleteChannel said that in a comment before this function did it in code.
		existing, guildID, _, err := s.authorizeChannel(ctx, q, actor, channelID, roles.PermManageChannels)
		if err != nil {
			return err
		}

		// The same voice-only rule as creation, checked against the type the channel actually has rather
		// than one the request could claim — and checked *after* authorization, which is the ordering that
		// matters.
		//
		// It ran before, and that made this 400 a report about a row the caller had no permission to see:
		// a text channel in somebody else's guild answered "bitrate and user_limit apply only to voice
		// channels" while a voice channel in the same guild answered 404, so the pair disclosed both that
		// a channel id was live and whether it carried voice. Found by a security review of this
		// milestone. Nothing in the check depends on running early; it reads the loaded row and the body.
		if existing.Type != ChannelGuildVoice && (in.Bitrate != nil || in.UserLimit != nil) {
			return httpx.Errorf(httpx.ErrBadRequest,
				"bitrate and user_limit apply only to voice channels")
		}

		guild := int64(guildID)
		row, err := q.UpdateChannel(ctx, db.UpdateChannelParams{
			ID:         int64(channelID),
			GuildID:    &guild,
			Name:       in.Name,
			Topic:      in.Topic,
			ClearTopic: in.ClearTopic,
			Position:   in.Position,
			Nsfw:       in.NSFW,
			Bitrate:    in.Bitrate,
			UserLimit:  in.UserLimit,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: update channel: %w", err)
		}

		changes := map[string]any{}
		if in.Name != nil {
			changes["name"] = *in.Name
		}
		if in.ClearTopic {
			changes["topic"] = nil
		} else if in.Topic != nil {
			changes["topic"] = *in.Topic
		}
		if in.Position != nil {
			changes["position"] = *in.Position
		}
		if in.NSFW != nil {
			changes["nsfw"] = *in.NSFW
		}
		if in.Bitrate != nil {
			changes["bitrate"] = *in.Bitrate
		}
		if in.UserLimit != nil {
			changes["user_limit"] = *in.UserLimit
		}

		if err := s.writeAudit(
			ctx, q, guildID, actor.UserID, ActionChannelUpdate, &channelID, changes,
		); err != nil {
			return err
		}

		out = channelFromRow(row)

		// The overwrites, so this response carries the same shape the listing does. One indexed read on a
		// cold path, and the alternative is a field that is populated on one route and empty on another —
		// which is the bug M12 shipped on a member's `roles`, where a client refreshing its cache from a
		// 200 dropped every role the member held.
		rows, err := q.ListChannelPermissionOverwrites(ctx, db.ListChannelPermissionOverwritesParams{
			ChannelID: int64(channelID),
			GuildID:   int64(guildID),
		})
		if err != nil {
			return fmt.Errorf("guilds: list channel overwrites: %w", err)
		}
		for _, ow := range rows {
			out.PermissionOverwrites = append(out.PermissionOverwrites, overwriteFromRow(ow))
		}

		return nil
	})
	if err != nil {
		return Channel{}, err
	}

	return out, nil
}

// DeleteChannel removes a channel. Its children, if it is a category, are orphaned to the top level
// rather than deleted — see channels.parent_id ON DELETE SET NULL in migration 000015.
func (s *Service) DeleteChannel(ctx context.Context, actor auth.Actor, channelID snowflake.ID) error {
	return s.inTx(ctx, func(q *db.Queries) error {
		// The same resolve-and-authorize the overwrite endpoints use, and it carries the refusal that
		// matters here: a member who cannot *see* this channel is answered as though it were not there.
		// The listing hides channels now, so a 403 for a hidden one against a 404 for a nonexistent one
		// is an oracle confirming exactly what the filter withholds — and that reasoning applies to these
		// routes as much as to the permission ones, which had it and these did not.
		// One read, not two: authorizeChannel loads the row to find its guild, and the guild is the only
		// thing this operation wanted it for.
		_, guildID, _, err := s.authorizeChannel(ctx, q, actor, channelID, roles.PermManageChannels)
		if err != nil {
			return err
		}

		// Unlike a guild deletion, this entry survives: audit_log_entries cascades from guilds, not from
		// channels, so deleting a channel leaves its record in place. That is the whole reason target_id
		// is not a foreign key — see migration 000016.
		if err := s.writeAudit(ctx, q, guildID, actor.UserID, ActionChannelDelete, &channelID, nil); err != nil {
			return err
		}

		guild := int64(guildID)
		affected, err := q.DeleteChannel(ctx, db.DeleteChannelParams{ID: int64(channelID), GuildID: &guild})
		if err != nil {
			return fmt.Errorf("guilds: delete channel: %w", err)
		}
		if affected == 0 {
			return httpx.ErrNotFound
		}

		return nil
	})
}

// guildOf returns the guild a channel belongs to, refusing one that belongs to none.
//
// A DM or group DM has a NULL guild_id and no permission model at all — ADR 0008's hierarchy is
// guild-scoped, and a DM's access rule is membership in channel_recipients, which is M57's table. So the
// guild-channel endpoints refuse them rather than resolving against a guild that is not there.
func guildOf(row db.Channel) (snowflake.ID, error) {
	if row.GuildID == nil {
		// 404 rather than 400: whether a channel id names a DM is not something a caller who cannot see it
		// should learn, and these routes simply do not serve that channel.
		return 0, httpx.ErrNotFound
	}
	return snowflake.ID(*row.GuildID), nil
}
