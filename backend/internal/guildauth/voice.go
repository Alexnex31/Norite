// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guildauth

import (
	"context"
	"fmt"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// VoiceRights is what one account may do in a voice channel (M33).
type VoiceRights struct {
	// Connect is membership, PermViewChannel and PermConnectVoice in the channel: what a join needs, and
	// what keeps a call up. Losing it ends the call on the server.
	Connect bool
	// Speak is PermSpeakVoice in the channel. Without it the account is suppressed, which its client
	// applies (M33's D6).
	Speak bool
}

// Voice resolves users' rights in a voice channel against fresh rows, three round trips for any number of
// accounts (roles.ResolveMany).
//
// It is the one answer to "may this account be in this call", asked when a join is authorized and again by
// every voice re-check, so a join and the re-check that would evict it cannot disagree. An account absent
// from the result, or present with Connect false, may not be in the call. Layer 1 is absent, as in Audience:
// an Instance Admin who is not a member resolves to nothing, and joining a call is not an act rule 14 can
// record.
//
// A channel no longer in the guild answers with nobody (roles.ResolveMany).
func Voice(
	ctx context.Context, q db.Querier, guildID, channelID snowflake.ID, users []snowflake.ID,
) (map[snowflake.ID]VoiceRights, error) {
	perms, err := roles.ResolveMany(ctx, q, guildID, channelID, users, nil)
	if err != nil {
		return nil, fmt.Errorf("guildauth: resolving voice rights: %w", err)
	}
	out := make(map[snowflake.ID]VoiceRights, len(perms))
	for id, p := range perms {
		out[id] = VoiceRights{
			Connect: p.Has(roles.PermViewChannel) && p.Has(roles.PermConnectVoice),
			Speak:   p.Has(roles.PermSpeakVoice),
		}
	}
	return out, nil
}
