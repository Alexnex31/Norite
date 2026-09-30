// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRefusalsCarryOnlyTheirOwnMessage asserts the whole public message of the refusals the guild handler
// maps from its own sentinels, not a phrase inside it.
//
// From M12 to the M13a manual pass, every one of them ended in the sentinel's internal text:
// "you cannot act on a member above you: guilds: the target stands at or above the actor". The status and
// code were right, and every test asserted those or a key phrase, so none could see it — the M13a pass
// found it by reading a response. The transfer's ceiling refusal was the case that made it more than
// untidy: its tail said the guild was at its limit, when the limit belongs to the recipient's account.
//
// Equality rather than Contains, because Contains is what let it through.
func TestRefusalsCarryOnlyTheirOwnMessage(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	guildID := mustID(t, f.guildID)

	decode := func(body []byte) string {
		t.Helper()
		var env struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(body, &env), "%s", body)
		return env.Error.Message
	}

	t.Run("the owner leaving", func(t *testing.T) {
		res := f.api.call(http.MethodDelete, fmt.Sprintf("/api/v1/guilds/%s/members/%s", f.guildID, f.ownerID), nil,
			withToken(f.ownerToken))
		require.Equal(t, http.StatusConflict, res.Code, res)
		require.Equal(t, "transfer ownership before leaving a guild you own", decode(res.Body))
	})

	t.Run("a transfer to a member at the owned-guild ceiling", func(t *testing.T) {
		ceiling := testConfig().MaxGuildsPerAccount
		f.api.mustExec(t, `INSERT INTO guilds (id, name, owner_id)
		                   SELECT $1::bigint + g, 'owned', $2 FROM generate_series(1, $3::int) g`,
			int64(910000000000000000), mustID(t, f.memberID), ceiling)

		res := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/owner", f.guildID),
			map[string]any{"user_id": f.memberID}, withToken(f.ownerToken))
		require.Equal(t, http.StatusConflict, res.Code, res)
		require.Equal(t, fmt.Sprintf("that member already owns the most guilds an account may own (%d)", ceiling),
			decode(res.Body))
	})

	t.Run("a kick of somebody ranked higher", func(t *testing.T) {
		// Roles are created at the bottom, so the one created first ends up above the one created second.
		high := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID),
			map[string]any{"name": "high"}, withToken(f.ownerToken))
		require.Equal(t, http.StatusCreated, high.Code, high)
		kick := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID),
			map[string]any{"name": "kick", "permissions": "33"}, withToken(f.ownerToken)) // view + kick
		require.Equal(t, http.StatusCreated, kick.Code, kick)

		f.api.mustExec(t, `INSERT INTO guild_members (guild_id, user_id) VALUES ($1, $2)`,
			guildID, mustID(t, f.strangerID))
		for _, grant := range []struct{ user, role string }{
			{f.memberID, kick.field(t, "id")}, {f.strangerID, high.field(t, "id")},
		} {
			res := f.api.call(http.MethodPut,
				fmt.Sprintf("/api/v1/guilds/%s/members/%s/roles/%s", f.guildID, grant.user, grant.role), nil,
				withToken(f.ownerToken))
			require.Less(t, res.Code, 300, res)
		}

		res := f.api.call(http.MethodDelete, fmt.Sprintf("/api/v1/guilds/%s/members/%s", f.guildID, f.strangerID),
			nil, withToken(f.memberToken))
		require.Equal(t, http.StatusForbidden, res.Code, res)
		require.Equal(t, "you cannot act on a member above you", decode(res.Body))
	})
}
