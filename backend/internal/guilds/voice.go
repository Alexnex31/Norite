// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"context"
	"fmt"
	"net/http"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/guildauth"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/voice/voicestate"
)

// ListVoiceStates returns who is in a voice channel, to anybody who can see it: the audience
// VOICE_STATE_UPDATE reaches. A channel of another type has nobody in it.
func (s *Service) ListVoiceStates(ctx context.Context, actor auth.Actor, channelID snowflake.ID) ([]voicestate.State, error) {
	row, _, _, err := guildauth.AuthorizeChannelUnlocked(ctx, s.queries, actor, channelID, 0)
	if err != nil {
		return nil, err
	}
	out := []voicestate.State{}
	if row.Type != ChannelGuildVoice {
		return out, nil
	}
	rows, err := s.queries.ListVoiceStatesInChannel(ctx, int64(channelID))
	if err != nil {
		return nil, fmt.Errorf("guilds: list voice states: %w", err)
	}
	for _, r := range rows {
		out = append(out, voicestate.Of(r))
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
