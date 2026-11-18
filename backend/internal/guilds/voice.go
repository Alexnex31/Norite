// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/guildauth"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// VoiceState is a voice connection as a channel's viewers are told about it (M33). The voice coordinator
// writes the rows and sends the events; this is the read a client connecting later starts from, so the
// shape is the coordinator's, held to the same contract schema by the payload tests.
type VoiceState struct {
	ConnectionID  snowflake.ID `json:"connection_id"`
	GuildID       snowflake.ID `json:"guild_id"`
	ChannelID     snowflake.ID `json:"channel_id"`
	UserID        snowflake.ID `json:"user_id"`
	Mute          bool         `json:"mute"`
	Deaf          bool         `json:"deaf"`
	Suppress      bool         `json:"suppress"`
	SupportsVideo bool         `json:"supports_video"`
	JoinedAt      time.Time    `json:"joined_at"`
}

// ListVoiceStates returns who is in a voice channel, to anybody who can see it: the audience
// VOICE_STATE_UPDATE reaches. A channel of another type has nobody in it.
func (s *Service) ListVoiceStates(ctx context.Context, actor auth.Actor, channelID snowflake.ID) ([]VoiceState, error) {
	row, _, _, err := guildauth.AuthorizeChannelUnlocked(ctx, s.queries, actor, channelID, 0)
	if err != nil {
		return nil, err
	}
	out := []VoiceState{}
	if row.Type != ChannelGuildVoice {
		return out, nil
	}
	rows, err := s.queries.ListVoiceStatesInChannel(ctx, int64(channelID))
	if err != nil {
		return nil, fmt.Errorf("guilds: list voice states: %w", err)
	}
	for _, r := range rows {
		out = append(out, VoiceState{
			ConnectionID: snowflake.ID(r.ConnectionID), GuildID: snowflake.ID(r.GuildID),
			ChannelID: snowflake.ID(r.ChannelID), UserID: snowflake.ID(r.UserID),
			Mute: r.Mute, Deaf: r.Deaf, Suppress: r.Suppress, SupportsVideo: r.SupportsVideo,
			JoinedAt: r.CreatedAt.Time.UTC(),
		})
	}
	return out, nil
}

func (h *Handler) listVoiceStates(w http.ResponseWriter, r *http.Request) {
	actor, channelID, ok := h.actorAndID(w, r, "channel_id")
	if !ok {
		return
	}
	states, err := h.svc.ListVoiceStates(r.Context(), actor, channelID)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, states)
}
