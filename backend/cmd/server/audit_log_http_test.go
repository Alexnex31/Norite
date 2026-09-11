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

// TestTheAuditLogRouteAnswersEachCallerAsItShould is the route wired end to end.
//
// The three answers are the package's standing anti-enumeration split, and this route is the first place
// they are asserted over HTTP rather than over the service: a stranger cannot learn the guild exists, and
// a member who lacks the permission gets a different answer because they already know it does.
func TestTheAuditLogRouteAnswersEachCallerAsItShould(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)
	path := fmt.Sprintf("/api/v1/guilds/%s/audit-log", f.guildID)

	stranger := f.api.call(http.MethodGet, path, nil, withToken(f.strangerToken))
	require.Equal(t, http.StatusNotFound, stranger.Code, stranger)

	member := f.api.call(http.MethodGet, path, nil, withToken(f.memberToken))
	require.Equal(t, http.StatusForbidden, member.Code, member)

	// The owner resolves to every bit, this one included.
	owner := f.api.call(http.MethodGet, path, nil, withToken(f.ownerToken))
	require.Equal(t, http.StatusOK, owner.Code, owner)

	var entries []map[string]any
	require.NoError(t, json.Unmarshal(owner.Body, &entries))
	require.NotEmpty(t, entries, "the guild was created through the API, so its creation is recorded")

	first := entries[0]
	require.Equal(t, "guild.create", first["action"])
	require.Equal(t, f.ownerID, first["actor_id"], "ids cross the wire as strings, as snowflakes do")
	require.Contains(t, first, "changes")
	require.NotContains(t, first, "guild_id", "the route is guild-scoped; the field would be the path")
}

// TestTheAuditLogRouteRefusesAQueryItCannotAnswer covers the three parameters the handler parses.
//
// The action case is the one that is not merely input validation: an action this instance never writes
// would return an empty page, which is indistinguishable from a guild that has taken no such action — so
// a typo would read as evidence of absence rather than as a mistake.
func TestTheAuditLogRouteRefusesAQueryItCannotAnswer(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)
	base := fmt.Sprintf("/api/v1/guilds/%s/audit-log", f.guildID)

	for _, tc := range []struct{ name, query string }{
		{"a cursor that is not an id", "?before=yesterday"},
		{"an actor that is not an id", "?actor_id=someone"},
		{"an action nobody writes", "?action=guild.nuke"},
		{"an action with the wrong case", "?action=GUILD.CREATE"},
		{"a limit of zero", "?limit=0"},
		{"a negative limit", "?limit=-1"},
		{"a limit that is not a number", "?limit=lots"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := f.api.call(http.MethodGet, base+tc.query, nil, withToken(f.ownerToken))
			require.Equal(t, http.StatusBadRequest, got.Code, got)
		})
	}

	// And a query it can answer, so the refusals above are about the values and not the parameters.
	ok := f.api.call(http.MethodGet, base+"?action=guild.create&limit=1", nil, withToken(f.ownerToken))
	require.Equal(t, http.StatusOK, ok.Code, ok)

	var entries []map[string]any
	require.NoError(t, json.Unmarshal(ok.Body, &entries))
	require.Len(t, entries, 1)
	require.Equal(t, "guild.create", entries[0]["action"])
}
