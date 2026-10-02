// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GET /users/@me/guilds (M20): the account's own memberships, which the command tree's verbs read because
// they have no gateway connection and so no READY.

// listMyGuilds calls the route and returns the ids it listed, in the order it listed them.
func listMyGuilds(t *testing.T, a *api, token string) []string {
	t.Helper()

	res := a.call(http.MethodGet, "/api/v1/users/@me/guilds", nil, withToken(token))
	require.Equal(t, http.StatusOK, res.Code, res)

	var listed []map[string]any
	require.NoError(t, json.Unmarshal(res.Body, &listed))
	require.NotNil(t, listed, "an empty list is [], never null: %s", res)

	ids := make([]string, 0, len(listed))
	for _, g := range listed {
		ids = append(ids, g["id"].(string))
	}
	return ids
}

// TestTheGuildListIsTheCallersOwn is rule 1's question for a route with no id in it: whose list is it.
//
// Three accounts, so each answer is told apart from the others: the owner and a member see the guild they
// share, the stranger sees only their own, and nobody sees the stranger's. An account in no guild at all
// gets an empty array rather than null.
func TestTheGuildListIsTheCallersOwn(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)

	loner := f.api.newAccount("loner", "loner@example.com", "loner-device")
	assert.Empty(t, listMyGuilds(t, f.api, loner.Tokens.AccessToken), "an account in no guild")

	created := f.api.call(http.MethodPost, "/api/v1/guilds", map[string]any{"name": "Elsewhere"},
		withToken(f.strangerToken))
	require.Equal(t, http.StatusCreated, created.Code, created)
	strangersGuild := created.field(t, "id")

	// A second guild of the owner's, so the order is asserted over more than one row.
	second := f.api.call(http.MethodPost, "/api/v1/guilds", map[string]any{"name": "Second"},
		withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, second.Code, second)
	ownersSecond := second.field(t, "id")

	owners := listMyGuilds(t, f.api, f.ownerToken)
	assert.Equal(t, []string{f.guildID, ownersSecond}, owners, "the owner's two guilds, ordered by id")
	assert.True(t, sort.SliceIsSorted(owners, func(i, j int) bool {
		return mustID(t, owners[i]) < mustID(t, owners[j])
	}))

	assert.Equal(t, []string{f.guildID}, listMyGuilds(t, f.api, f.memberToken),
		"a member who did not create the guild still lists it")
	assert.Equal(t, []string{strangersGuild}, listMyGuilds(t, f.api, f.strangerToken),
		"the stranger lists their own guild and nobody else's")
}

// TestTheGuildListTakesTheReadScope bounds a delegated credential the way every other guild read is.
func TestTheGuildListTakesTheReadScope(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)

	mint := func(t *testing.T, scopes ...string) string {
		t.Helper()
		res := f.api.call(http.MethodPost, "/api/v1/auth/tokens",
			map[string]any{"name": "bot", "scopes": scopes}, withToken(f.ownerToken))
		require.Equal(t, http.StatusCreated, res.Code, res)
		return res.field(t, "value")
	}

	identify := f.api.call(http.MethodGet, "/api/v1/users/@me/guilds", nil, withToken(mint(t, "identify")))
	assert.Equal(t, http.StatusForbidden, identify.Code, identify)

	write := f.api.call(http.MethodGet, "/api/v1/users/@me/guilds", nil, withToken(mint(t, "guilds.write")))
	assert.Equal(t, http.StatusForbidden, write.Code, "write does not imply read: %s", write)

	assert.Equal(t, []string{f.guildID}, listMyGuilds(t, f.api, mint(t, "guilds.read")))

	anonymous := f.api.call(http.MethodGet, "/api/v1/users/@me/guilds", nil)
	assert.Equal(t, http.StatusUnauthorized, anonymous.Code, anonymous)
}

// TestTheGuildListMatchesTheContract reads each item against Guild, exactly, as the other payload tests do.
func TestTheGuildListMatchesTheContract(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)
	schemas := contractSchemas(t)

	res := f.api.call(http.MethodGet, "/api/v1/users/@me/guilds", nil, withToken(f.ownerToken))
	require.Equal(t, http.StatusOK, res.Code, res)

	var listed []map[string]any
	require.NoError(t, json.Unmarshal(res.Body, &listed))
	require.Len(t, listed, 1)

	var got []string
	for k := range listed[0] {
		got = append(got, k)
	}
	sort.Strings(got)

	declared, _ := declaredProperties(t, schemas["Guild"])
	assert.Equal(t, declared, got, "GET /users/@me/guilds sent %v; Guild declares %v", got, declared)
}
