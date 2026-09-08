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

// guildChannelTypes is what a guild may contain, and the check that a caller cannot create anything else.
//
// DM and GROUP_DM belong to no guild and are M57's. GUILD_ANNOUNCEMENT and GUILD_STAGE_VOICE are reserved
// values that must stay in the schema (rule 10) and are not yet buildable. PUBLIC_MATCHMAKING is created
// by the matchmaking service, not by a guild owner.
//
// A map rather than a range check, because the values are a vocabulary rather than an interval — the
// permitted set is not contiguous and never will be.
var guildChannelTypes = map[int16]struct{}{
	ChannelGuildText:     {},
	ChannelGuildVoice:    {},
	ChannelGuildCategory: {},
}

func channelFromRow(row db.Channel) Channel {
	return Channel{
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

func channelFromListRow(row db.ListGuildChannelsRow) Channel {
	return Channel{
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

// ListChannels returns a guild's channels in position order.
func (s *Service) ListChannels(
	ctx context.Context, actor auth.Actor, guildID snowflake.ID,
) ([]Channel, error) {
	if err := s.authorize(ctx, actor, guildID, 0, roles.PermViewChannel); err != nil {
		return nil, err
	}

	id := int64(guildID)
	rows, err := s.queries.ListGuildChannels(ctx, &id)
	if err != nil {
		return nil, fmt.Errorf("guilds: list channels: %w", err)
	}

	// Per-channel overwrites are deliberately not applied to this listing at M12. Nothing can write an
	// overwrite until M13, so every channel in every guild resolves identically today, and filtering the
	// list by per-channel PermViewChannel is M13's job — landing it here would be untestable code shaped
	// by a guess. Its roadmap entry carries the item.
	out := make([]Channel, 0, len(rows))
	for _, row := range rows {
		out = append(out, channelFromListRow(row))
	}

	return out, nil
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
	if _, ok := guildChannelTypes[in.Type]; !ok {
		return Channel{}, httpx.Errorf(ErrUnsupportedChannelType,
			"channel type %d cannot be created in a guild", in.Type)
	}

	channelID, err := s.ids.Next()
	if err != nil {
		return Channel{}, fmt.Errorf("guilds: mint channel id: %w", err)
	}

	var out Channel

	err = s.inTx(ctx, func(q *db.Queries) error {
		if err := authorizeWith(ctx, q, actor, guildID, 0, roles.PermManageChannels); err != nil {
			return err
		}

		// A parent must be a category *in this guild*. Checked rather than trusted, because parent_id
		// arrives from the caller and a channel is otherwise free to nest under one in a guild the caller
		// has no permissions in — which would put its children behind that guild's overwrites (rule 1).
		if in.ParentID != nil {
			parent, err := q.GetChannel(ctx, int64(*in.ParentID))
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				return httpx.Errorf(httpx.ErrBadRequest, "parent_id does not name a channel in this guild")
			case err != nil:
				return fmt.Errorf("guilds: get parent channel: %w", err)
			case parent.GuildID == nil || snowflake.ID(*parent.GuildID) != guildID:
				return httpx.Errorf(httpx.ErrBadRequest, "parent_id does not name a channel in this guild")
			case parent.Type != ChannelGuildCategory:
				return httpx.Errorf(httpx.ErrBadRequest, "parent_id must name a category channel")
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

		if err := s.writeAudit(ctx, q, guildID, actor.UserID, ActionChannelCreate, &channelID, map[string]any{
			"name": in.Name,
			"type": in.Type,
		}); err != nil {
			return err
		}

		out = channelFromRow(row)
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
		existing, err := q.GetChannel(ctx, int64(channelID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: get channel: %w", err)
		}

		guildID, err := guildOf(existing)
		if err != nil {
			return err
		}

		if err := authorizeWith(ctx, q, actor, guildID, channelID, roles.PermManageChannels); err != nil {
			return err
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

		if err := s.writeAudit(
			ctx, q, guildID, actor.UserID, ActionChannelUpdate, &channelID, changes,
		); err != nil {
			return err
		}

		out = channelFromRow(row)
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
		existing, err := q.GetChannel(ctx, int64(channelID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("guilds: get channel: %w", err)
		}

		guildID, err := guildOf(existing)
		if err != nil {
			return err
		}

		if err := authorizeWith(ctx, q, actor, guildID, channelID, roles.PermManageChannels); err != nil {
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
