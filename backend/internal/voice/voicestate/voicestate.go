// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package voicestate is a voice connection as a channel's viewers are told about it (M33), built once for
// the gateway's VOICE_STATE_UPDATE and VOICE_STATE_DELETE and for GET /channels/{id}/voice-states, from the
// voice_states row. Two hand-kept copies of one wire shape drift (M33's /code-review). It imports nothing
// of the SFU, so the guilds package reads through it too.
package voicestate

import (
	"time"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// State is VoiceState in openapi.yaml.
type State struct {
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

// Of is a row as the channel is told about it.
func Of(r db.VoiceState) State {
	return State{
		ConnectionID: snowflake.ID(r.ConnectionID), GuildID: snowflake.ID(r.GuildID),
		ChannelID: snowflake.ID(r.ChannelID), UserID: snowflake.ID(r.UserID),
		Mute: r.Mute, Deaf: r.Deaf, Suppress: r.Suppress, SupportsVideo: r.SupportsVideo,
		JoinedAt: r.CreatedAt.Time.UTC(),
	}
}

// Deleted is VOICE_STATE_DELETE's payload: which connection left which channel.
type Deleted struct {
	ConnectionID snowflake.ID `json:"connection_id"`
	GuildID      snowflake.ID `json:"guild_id"`
	ChannelID    snowflake.ID `json:"channel_id"`
	UserID       snowflake.ID `json:"user_id"`
}

// DeletedOf is a removed row as the channel is told about it.
func DeletedOf(r db.VoiceState) Deleted {
	return Deleted{ConnectionID: snowflake.ID(r.ConnectionID), GuildID: snowflake.ID(r.GuildID),
		ChannelID: snowflake.ID(r.ChannelID), UserID: snowflake.ID(r.UserID)}
}
