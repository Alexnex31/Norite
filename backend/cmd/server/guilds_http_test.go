// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// guildFixture is one instance with a guild, its owner, a plain member and a stranger, all signed in.
type guildFixture struct {
	api *api

	ownerToken    string
	memberToken   string
	strangerToken string

	ownerID    string
	memberID   string
	strangerID string

	guildID    string
	everyoneID string
}

func newGuildFixture(t *testing.T) *guildFixture {
	t.Helper()

	a := newAPI(t, auth.RegistrationOpen)
	f := &guildFixture{api: a}

	owner := a.newAccount("owner", "owner@example.com", "owner-device")
	member := a.newAccount("member", "member@example.com", "member-device")
	stranger := a.newAccount("stranger", "stranger@example.com", "stranger-device")

	f.ownerToken, f.ownerID = owner.Tokens.AccessToken, owner.ID
	f.memberToken, f.memberID = member.Tokens.AccessToken, member.ID
	f.strangerToken, f.strangerID = stranger.Tokens.AccessToken, stranger.ID

	created := a.call(http.MethodPost, "/api/v1/guilds",
		map[string]any{"name": "Test Guild"}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, created.Code, created)
	f.guildID = created.field(t, "id")

	// The member joins by direct insert: there is no join endpoint until invites exist, and this test
	// suite is about what the guild endpoints do once somebody is in.
	a.mustExec(t, `INSERT INTO guild_members (guild_id, user_id) VALUES ($1, $2)`,
		mustID(t, f.guildID), mustID(t, f.memberID))

	roleList := a.call(http.MethodGet,
		fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID), nil, withToken(f.ownerToken))
	require.Equal(t, http.StatusOK, roleList.Code, roleList)

	var rs []map[string]any
	require.NoError(t, json.Unmarshal(roleList.Body, &rs))
	require.Len(t, rs, 1, "a new guild has exactly one role, @everyone")
	f.everyoneID = rs[0]["id"].(string)

	return f
}

// TestCreatingAGuildCreatesItsDefaultRoleAndOwnerMembership pins the three rows guild creation writes.
//
// A guild without @everyone has no permission floor and every member resolves to nothing; a guild without
// the owner's membership looks fine until the member list comes back without them. Neither is repairable
// by a client, so neither is a state the code can produce — all three rows share one transaction.
func TestCreatingAGuildCreatesItsDefaultRoleAndOwnerMembership(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)
	guildID := mustID(t, f.guildID)

	var roleCount, memberCount, defaultCount int
	f.api.mustQueryRow(t,
		`SELECT (SELECT count(*) FROM roles WHERE guild_id = $1),
		        (SELECT count(*) FROM guild_members WHERE guild_id = $1 AND user_id = $2),
		        (SELECT count(*) FROM roles WHERE guild_id = $1 AND is_default)`,
		[]any{guildID, mustID(t, f.ownerID)}, &roleCount, &memberCount, &defaultCount)

	require.Equal(t, 1, roleCount, "exactly one role")
	require.Equal(t, 1, memberCount, "the owner is a member of their own guild")
	require.Equal(t, 1, defaultCount, "and that role is @everyone")
}

// TestEveryMutatingGuildRouteRefusesANonMember is the structural test.
//
// It exercises every non-GET route the guild handler registers with an actor who is in no guild, and
// requires 404 from all of them. A handler added later that assembles its own permission check instead of
// calling authorize will fail here — which is the point, since authorize being the only path to a
// decision is a property no single-endpoint test can assert.
//
// 404 rather than 403 throughout: a non-member must not learn the guild exists.
func TestEveryMutatingGuildRouteRefusesANonMember(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)

	channel := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/channels", f.guildID),
		map[string]any{"name": "general", "type": 0}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, channel.Code, channel)
	channelID := channel.field(t, "id")

	role := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID),
		map[string]any{"name": "mods"}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, role.Code, role)
	roleID := role.field(t, "id")

	for _, tc := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPatch, "/api/v1/guilds/" + f.guildID, map[string]any{"name": "hijacked"}},
		{http.MethodDelete, "/api/v1/guilds/" + f.guildID, nil},
		{http.MethodPost, "/api/v1/guilds/" + f.guildID + "/channels",
			map[string]any{"name": "x", "type": 0}},
		{http.MethodPatch, "/api/v1/channels/" + channelID, map[string]any{"name": "hijacked"}},
		{http.MethodDelete, "/api/v1/channels/" + channelID, nil},
		{http.MethodPost, "/api/v1/guilds/" + f.guildID + "/roles", map[string]any{"name": "x"}},
		{http.MethodPatch, "/api/v1/guilds/" + f.guildID + "/roles",
			map[string]any{"roles": []map[string]any{{"id": roleID, "position": 1}}}},
		{http.MethodPatch, "/api/v1/guilds/" + f.guildID + "/roles/" + roleID,
			map[string]any{"name": "hijacked"}},
		{http.MethodDelete, "/api/v1/guilds/" + f.guildID + "/roles/" + roleID, nil},
		{http.MethodPatch, "/api/v1/guilds/" + f.guildID + "/members/" + f.memberID,
			map[string]any{"nickname": "hijacked"}},
		{http.MethodDelete, "/api/v1/guilds/" + f.guildID + "/members/" + f.memberID, nil},
		// The overwrite pair. Added when a security review pointed out that the test whose stated value is
		// that it "cannot miss a route" had missed two — they were covered at service level, so nothing
		// was broken, but the guarantee this table advertises stops being true the moment it is partial.
		{http.MethodPut, "/api/v1/channels/" + channelID + "/permissions/" + roleID,
			map[string]any{"type": 0, "allow": "0", "deny": "0"}},
		{http.MethodDelete, "/api/v1/channels/" + channelID + "/permissions/" + roleID,
			map[string]any{"type": 0}},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			resp := f.api.call(tc.method, tc.path, tc.body, withToken(f.strangerToken))
			require.Equal(t, http.StatusNotFound, resp.Code,
				"a non-member must be refused, and must not learn the guild exists: %s", resp)
		})
	}
}

// TestAChannelIsAuthorizedAgainstItsOwnGuild is rule 1's cross-scope check.
//
// PATCH /channels/{id} carries no guild, so the guild it resolves against comes from the channel row. A
// member with MANAGE_CHANNELS in their own guild must not be able to touch a channel in another one.
//
// Confirmed by removal: make the handler trust a guild id from the request instead of the row, and this
// passes.
func TestAChannelIsAuthorizedAgainstItsOwnGuild(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)

	// The stranger owns a guild of their own and holds every permission in it.
	theirs := f.api.call(http.MethodPost, "/api/v1/guilds",
		map[string]any{"name": "Their Guild"}, withToken(f.strangerToken))
	require.Equal(t, http.StatusCreated, theirs.Code, theirs)

	victim := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/channels", f.guildID),
		map[string]any{"name": "private", "type": 0}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, victim.Code, victim)
	victimChannel := victim.field(t, "id")

	rename := f.api.call(http.MethodPatch, "/api/v1/channels/"+victimChannel,
		map[string]any{"name": "hijacked"}, withToken(f.strangerToken))
	require.Equal(t, http.StatusNotFound, rename.Code,
		"owning a guild grants nothing in another one: %s", rename)

	remove := f.api.call(http.MethodDelete, "/api/v1/channels/"+victimChannel, nil,
		withToken(f.strangerToken))
	require.Equal(t, http.StatusNotFound, remove.Code, remove)

	var name string
	f.api.mustQueryRow(t, `SELECT name FROM channels WHERE id = $1`,
		[]any{mustID(t, victimChannel)}, &name)
	require.Equal(t, "private", name, "the channel must be untouched")
}

// TestARoleCannotBeGivenPermissionsTheCallerLacks is the escalation this milestone must refuse.
//
// Without it MANAGE_ROLES is every permission: mint a role carrying ADMINISTRATOR, take it, and the
// hierarchy is gone. It is the check whose absence would look entirely normal.
func TestARoleCannotBeGivenPermissionsTheCallerLacks(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)

	// The member gets MANAGE_ROLES and nothing else.
	granted := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID),
		map[string]any{"name": "role-manager", "permissions": roles.PermManageRoles},
		withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, granted.Code, granted)
	f.api.mustExec(t, `INSERT INTO guild_member_roles (guild_id, user_id, role_id) VALUES ($1, $2, $3)`,
		mustID(t, f.guildID), mustID(t, f.memberID), mustID(t, granted.field(t, "id")))

	t.Run("creating", func(t *testing.T) {
		resp := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID),
			map[string]any{"name": "sneaky", "permissions": roles.PermAdministrator},
			withToken(f.memberToken))
		require.Equal(t, http.StatusForbidden, resp.Code,
			"MANAGE_ROLES must not be a path to ADMINISTRATOR: %s", resp)
	})

	t.Run("updating", func(t *testing.T) {
		harmless := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID),
			map[string]any{"name": "harmless"}, withToken(f.memberToken))
		require.Equal(t, http.StatusCreated, harmless.Code, harmless)

		resp := f.api.call(http.MethodPatch,
			fmt.Sprintf("/api/v1/guilds/%s/roles/%s", f.guildID, harmless.field(t, "id")),
			map[string]any{"permissions": roles.PermBanMembers}, withToken(f.memberToken))
		require.Equal(t, http.StatusForbidden, resp.Code,
			"editing a role must be bounded the same way creating one is: %s", resp)
	})

	t.Run("the owner may grant anything", func(t *testing.T) {
		resp := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID),
			map[string]any{"name": "admins", "permissions": roles.PermAdministrator},
			withToken(f.ownerToken))
		require.Equal(t, http.StatusCreated, resp.Code, resp)
	})
}

// TestTheDefaultRoleCannotBeDeletedOrRenamed guards the permission floor.
func TestTheDefaultRoleCannotBeDeletedOrRenamed(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)
	path := fmt.Sprintf("/api/v1/guilds/%s/roles/%s", f.guildID, f.everyoneID)

	t.Run("delete is refused", func(t *testing.T) {
		resp := f.api.call(http.MethodDelete, path, nil, withToken(f.ownerToken))
		require.Equal(t, http.StatusConflict, resp.Code, resp)

		var n int
		f.api.mustQueryRow(t, `SELECT count(*) FROM roles WHERE id = $1`,
			[]any{mustID(t, f.everyoneID)}, &n)
		require.Equal(t, 1, n, "@everyone must survive")
	})

	t.Run("rename is refused", func(t *testing.T) {
		resp := f.api.call(http.MethodPatch, path, map[string]any{"name": "everybody"},
			withToken(f.ownerToken))
		require.Equal(t, http.StatusConflict, resp.Code, resp)
	})

	// Editing its permissions is how a guild sets its floor, so that stays allowed.
	t.Run("its permissions may be edited", func(t *testing.T) {
		resp := f.api.call(http.MethodPatch, path,
			map[string]any{"permissions": roles.PermViewChannel}, withToken(f.ownerToken))
		require.Equal(t, http.StatusOK, resp.Code, resp)
	})
}

// TestTheOwnerCannotBeRemoved covers both routes to it: a kick and leaving.
//
// The owner is the tier that bypasses permission checks within a guild, so a guild whose owner is not a
// member has no such tier at all.
func TestTheOwnerCannotBeRemoved(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)
	path := fmt.Sprintf("/api/v1/guilds/%s/members/%s", f.guildID, f.ownerID)

	t.Run("by themselves", func(t *testing.T) {
		resp := f.api.call(http.MethodDelete, path, nil, withToken(f.ownerToken))
		require.Equal(t, http.StatusConflict, resp.Code, resp)
	})

	t.Run("by an instance admin", func(t *testing.T) {
		f.api.mustExec(t, `INSERT INTO instance_admins (user_id) VALUES ($1)`, mustID(t, f.strangerID))
		resp := f.api.call(http.MethodDelete, path, nil, withToken(f.strangerToken))
		require.Equal(t, http.StatusConflict, resp.Code, resp)
	})
}

// TestAMemberCanLeaveWithoutAPermission covers the other half of member removal.
func TestAMemberCanLeaveWithoutAPermission(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)

	resp := f.api.call(http.MethodDelete,
		fmt.Sprintf("/api/v1/guilds/%s/members/%s", f.guildID, f.memberID), nil,
		withToken(f.memberToken))
	require.Equal(t, http.StatusNoContent, resp.Code,
		"nobody should need a moderation permission to walk out of a room: %s", resp)

	// And cannot kick somebody else with the same absence of permission.
	f.api.mustExec(t, `INSERT INTO guild_members (guild_id, user_id) VALUES ($1, $2)`,
		mustID(t, f.guildID), mustID(t, f.memberID))
	// 403 rather than 404, and the difference is the ordering: the caller is a member, so they already
	// know the guild exists, and the permission is checked before anything about the target is looked at.
	// A 404 here would be reporting on the target instead, which is a fact the caller has not earned.
	kick := f.api.call(http.MethodDelete,
		fmt.Sprintf("/api/v1/guilds/%s/members/%s", f.guildID, f.strangerID), nil,
		withToken(f.memberToken))
	require.Equal(t, http.StatusForbidden, kick.Code,
		"leaving needs no permission; kicking needs KICK_MEMBERS: %s", kick)
}

// TestMemberListPaginatesByCursor covers the page shape and the clamp.
func TestMemberListPaginatesByCursor(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)
	guildID := mustID(t, f.guildID)

	// Enough members that a default page cannot hold them all.
	for i := range 120 {
		id := int64(900000000000000000 + i)
		f.api.mustExec(t, `INSERT INTO users (id, username, email, display_name)
		                   VALUES ($1, $2, $3, $4)`,
			id, fmt.Sprintf("bulk%d", i), fmt.Sprintf("bulk%d@example.test", i), "Bulk")
		f.api.mustExec(t, `INSERT INTO guild_members (guild_id, user_id) VALUES ($1, $2)`, guildID, id)
	}

	base := fmt.Sprintf("/api/v1/guilds/%s/members", f.guildID)

	first := f.api.call(http.MethodGet, base+"?limit=100", nil, withToken(f.ownerToken))
	require.Equal(t, http.StatusOK, first.Code, first)

	var page1 []map[string]any
	require.NoError(t, json.Unmarshal(first.Body, &page1))
	require.Len(t, page1, 100)

	// Ascending by user id, which is what makes the cursor stable.
	for i := 1; i < len(page1); i++ {
		require.Less(t, page1[i-1]["user_id"].(string), page1[i]["user_id"].(string),
			"the page must be ordered by user id, or the cursor means nothing")
	}

	last := page1[len(page1)-1]["user_id"].(string)
	second := f.api.call(http.MethodGet, base+"?after="+last+"&limit=100", nil, withToken(f.ownerToken))
	require.Equal(t, http.StatusOK, second.Code, second)

	var page2 []map[string]any
	require.NoError(t, json.Unmarshal(second.Body, &page2))
	require.NotEmpty(t, page2)
	require.Greater(t, page2[0]["user_id"].(string), last, "a page must resume after the cursor")

	// Above the ceiling is clamped rather than refused.
	over := f.api.call(http.MethodGet, base+"?limit=5000", nil, withToken(f.ownerToken))
	require.Equal(t, http.StatusOK, over.Code, over)

	var clamped []map[string]any
	require.NoError(t, json.Unmarshal(over.Body, &clamped))
	require.Len(t, clamped, 100, "limit must be clamped to the ceiling, not honored")
}

// TestUpdatingAMemberNeedsThePermissionForTheFieldSent pins the per-field permission split.
//
// Collapsing mute and deafen into MANAGE_GUILD would let anybody who can rename people also silence them.
func TestUpdatingAMemberNeedsThePermissionForTheFieldSent(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)

	// The member gets MANAGE_GUILD, and neither MUTE_MEMBERS nor DEAFEN_MEMBERS.
	role := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID),
		map[string]any{"name": "namer", "permissions": roles.PermManageGuild},
		withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, role.Code, role)
	f.api.mustExec(t, `INSERT INTO guild_member_roles (guild_id, user_id, role_id) VALUES ($1, $2, $3)`,
		mustID(t, f.guildID), mustID(t, f.memberID), mustID(t, role.field(t, "id")))

	f.api.mustExec(t, `INSERT INTO guild_members (guild_id, user_id) VALUES ($1, $2)`,
		mustID(t, f.guildID), mustID(t, f.strangerID))
	target := fmt.Sprintf("/api/v1/guilds/%s/members/%s", f.guildID, f.strangerID)

	nick := f.api.call(http.MethodPatch, target, map[string]any{"nickname": "Renamed"},
		withToken(f.memberToken))
	require.Equal(t, http.StatusOK, nick.Code, "MANAGE_GUILD covers a nickname: %s", nick)

	muted := f.api.call(http.MethodPatch, target, map[string]any{"mute": true}, withToken(f.memberToken))
	require.Equal(t, http.StatusForbidden, muted.Code,
		"renaming somebody must not also let you silence them: %s", muted)

	deafened := f.api.call(http.MethodPatch, target, map[string]any{"deaf": true},
		withToken(f.memberToken))
	require.Equal(t, http.StatusForbidden, deafened.Code, deafened)
}

// TestOnlyTheOwnerOrAnInstanceAdminDeletesAGuild separates deletion from MANAGE_GUILD.
func TestOnlyTheOwnerOrAnInstanceAdminDeletesAGuild(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)

	role := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID),
		map[string]any{"name": "manager", "permissions": roles.PermManageGuild},
		withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, role.Code, role)
	f.api.mustExec(t, `INSERT INTO guild_member_roles (guild_id, user_id, role_id) VALUES ($1, $2, $3)`,
		mustID(t, f.guildID), mustID(t, f.memberID), mustID(t, role.field(t, "id")))

	// MANAGE_GUILD renames but does not delete.
	rename := f.api.call(http.MethodPatch, "/api/v1/guilds/"+f.guildID,
		map[string]any{"name": "Renamed"}, withToken(f.memberToken))
	require.Equal(t, http.StatusOK, rename.Code, rename)

	refused := f.api.call(http.MethodDelete, "/api/v1/guilds/"+f.guildID, nil, withToken(f.memberToken))
	require.Equal(t, http.StatusForbidden, refused.Code,
		"deleting a guild is an ownership decision, not a delegable permission: %s", refused)

	deleted := f.api.call(http.MethodDelete, "/api/v1/guilds/"+f.guildID, nil, withToken(f.ownerToken))
	require.Equal(t, http.StatusNoContent, deleted.Code, deleted)
}

// TestOnlyGuildChannelTypesCanBeCreated keeps the DM and reserved types out of the guild endpoint.
func TestOnlyGuildChannelTypesCanBeCreated(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)
	path := fmt.Sprintf("/api/v1/guilds/%s/channels", f.guildID)

	for _, ct := range []int{1, 3, 5, 6, 7, 42} {
		resp := f.api.call(http.MethodPost, path,
			map[string]any{"name": "x", "type": ct}, withToken(f.ownerToken))
		require.Equal(t, http.StatusBadRequest, resp.Code,
			"channel type %d must be refused in a guild: %s", ct, resp)
	}

	for _, ct := range []int{0, 2, 4} {
		resp := f.api.call(http.MethodPost, path,
			map[string]any{"name": fmt.Sprintf("ch%d", ct), "type": ct}, withToken(f.ownerToken))
		require.Equal(t, http.StatusCreated, resp.Code, "channel type %d must be allowed: %s", ct, resp)
	}
}

// TestAParentMustBeACategoryInTheSameGuild covers the other cross-scope hole.
//
// Without the check a channel could nest under a category in a guild the caller has no permissions in,
// putting its children behind that guild's overwrites.
func TestAParentMustBeACategoryInTheSameGuild(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)

	theirs := f.api.call(http.MethodPost, "/api/v1/guilds",
		map[string]any{"name": "Their Guild"}, withToken(f.strangerToken))
	require.Equal(t, http.StatusCreated, theirs.Code, theirs)

	theirCategory := f.api.call(http.MethodPost,
		fmt.Sprintf("/api/v1/guilds/%s/channels", theirs.field(t, "id")),
		map[string]any{"name": "theirs", "type": 4}, withToken(f.strangerToken))
	require.Equal(t, http.StatusCreated, theirCategory.Code, theirCategory)

	resp := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/channels", f.guildID),
		map[string]any{"name": "x", "type": 0, "parent_id": theirCategory.field(t, "id")},
		withToken(f.ownerToken))
	require.Equal(t, http.StatusBadRequest, resp.Code,
		"a parent in another guild must be refused: %s", resp)

	// A non-category in the right guild is refused too.
	text := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/channels", f.guildID),
		map[string]any{"name": "plain", "type": 0}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, text.Code, text)

	nested := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/channels", f.guildID),
		map[string]any{"name": "y", "type": 0, "parent_id": text.field(t, "id")},
		withToken(f.ownerToken))
	require.Equal(t, http.StatusBadRequest, nested.Code, nested)
}

// --- helpers ---

// field pulls one top-level string field out of a JSON response.
func (r *response) field(t *testing.T, name string) string {
	t.Helper()

	var body map[string]any
	require.NoError(t, json.Unmarshal(r.Body, &body), "decoding: %s", r.Body)

	v, ok := body[name].(string)
	require.Truef(t, ok, "response has no string field %q: %s", name, r.Body)
	return v
}

// mustExec runs a statement directly against the test database.
//
// Used for the setup M12 has no endpoint for — joining a guild, granting a role, becoming an Instance
// Admin. Those arrive at M13 and with invites; going through the API would mean this milestone could not
// test what it built.
func (a *api) mustExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	_, err := a.pool.Exec(t.Context(), sql, args...)
	require.NoError(t, err)
}

func (a *api) mustQueryRow(t *testing.T, sql string, args []any, dest ...any) {
	t.Helper()
	require.NoError(t, a.pool.QueryRow(t.Context(), sql, args...).Scan(dest...))
}

// next mints an id that names nothing, for the "no such object" reference answer.
func (f *guildFixture) next(t *testing.T) string {
	t.Helper()
	ids, err := snowflake.NewGenerator(3)
	require.NoError(t, err)
	id, err := ids.Next()
	require.NoError(t, err)
	return id.String()
}

// mustID converts a snowflake string from a response into the bigint the database stores.
func mustID(t *testing.T, s string) int64 {
	t.Helper()
	id, err := snowflake.Parse(s)
	require.NoError(t, err, "not a snowflake: %q", s)
	return int64(id)
}

// TestEveryGuildMutationWritesExactlyOneAuditEntry is rule 2, and the test that makes pulling the audit
// table forward from M14 worth anything.
//
// Table-driven over every mutating verb, because the failure mode is a single handler forgetting — and a
// per-endpoint test is exactly the shape that lets the next handler be added without one. M14 inherits
// this test and extends it to the `changes` diff.
//
// The guild delete is absent on purpose and is not an omission: audit_log_entries.guild_id cascades from
// guilds, so a guild's own deletion entry is removed by the cascade it records. The entry is still written
// in the mutation's transaction, satisfying rule 2, but nothing can read it afterwards — the durable
// record of a deleted guild belongs in instance_audit_log, which is rule 14's table. Asserted below as the
// state it actually is, rather than left to look like a gap.
func TestEveryGuildMutationWritesExactlyOneAuditEntry(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)
	guildID := mustID(t, f.guildID)

	channel := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/channels", f.guildID),
		map[string]any{"name": "general", "type": 0}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, channel.Code, channel)
	channelID := channel.field(t, "id")

	role := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID),
		map[string]any{"name": "mods"}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, role.Code, role)
	roleID := role.field(t, "id")

	// Two entries so far: guild.create from the fixture, plus channel.create and role.create.
	for _, tc := range []struct {
		action string
		method string
		path   string
		body   any
		want   int
	}{
		{"guild.create", "", "", nil, http.StatusCreated},
		{"channel.create", "", "", nil, http.StatusCreated},
		{"role.create", "", "", nil, http.StatusCreated},
		{"guild.update", http.MethodPatch, "/api/v1/guilds/" + f.guildID,
			map[string]any{"name": "Renamed"}, http.StatusOK},
		{"channel.update", http.MethodPatch, "/api/v1/channels/" + channelID,
			map[string]any{"name": "renamed"}, http.StatusOK},
		{"role.update", http.MethodPatch,
			fmt.Sprintf("/api/v1/guilds/%s/roles/%s", f.guildID, roleID),
			map[string]any{"name": "renamed"}, http.StatusOK},
		{"member.update", http.MethodPatch,
			fmt.Sprintf("/api/v1/guilds/%s/members/%s", f.guildID, f.memberID),
			map[string]any{"nickname": "Nick"}, http.StatusOK},
		{"member.remove", http.MethodDelete,
			fmt.Sprintf("/api/v1/guilds/%s/members/%s", f.guildID, f.memberID), nil,
			http.StatusNoContent},
		{"role.delete", http.MethodDelete,
			fmt.Sprintf("/api/v1/guilds/%s/roles/%s", f.guildID, roleID), nil, http.StatusNoContent},
		{"channel.delete", http.MethodDelete, "/api/v1/channels/" + channelID, nil,
			http.StatusNoContent},
	} {
		t.Run(tc.action, func(t *testing.T) {
			if tc.method != "" {
				resp := f.api.call(tc.method, tc.path, tc.body, withToken(f.ownerToken))
				require.Equal(t, tc.want, resp.Code, resp)
			}

			var n int
			f.api.mustQueryRow(t,
				`SELECT count(*) FROM audit_log_entries WHERE guild_id = $1 AND action = $2`,
				[]any{guildID, tc.action}, &n)
			require.Equalf(t, 1, n, "%s must write exactly one audit entry (rule 2)", tc.action)

			var actor int64
			f.api.mustQueryRow(t,
				`SELECT actor_id FROM audit_log_entries WHERE guild_id = $1 AND action = $2`,
				[]any{guildID, tc.action}, &actor)
			require.Equal(t, mustID(t, f.ownerID), actor, "the entry must name who did it")
		})
	}
}

// TestTheAuditEntryAndTheMutationShareATransaction is the other half of rule 2.
//
// Writing an entry is not enough if the two can come apart. A BEFORE INSERT trigger on
// audit_log_entries raises, so the audit write fails inside the transaction the mutation is running in,
// and the mutation must go with it.
//
// A trigger rather than a Go-side stub, because the property under test is transactional and a fake would
// only assert that the test author's model of transactions matches itself.
func TestTheAuditEntryAndTheMutationShareATransaction(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)

	f.api.mustExec(t, `
		CREATE FUNCTION refuse_audit() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'audit write refused by test';
		END;
		$$ LANGUAGE plpgsql;

		CREATE TRIGGER refuse_audit_trigger
		BEFORE INSERT ON audit_log_entries
		FOR EACH ROW EXECUTE FUNCTION refuse_audit();`)

	resp := f.api.call(http.MethodPatch, "/api/v1/guilds/"+f.guildID,
		map[string]any{"name": "Renamed"}, withToken(f.ownerToken))
	require.Equal(t, http.StatusInternalServerError, resp.Code,
		"a refused audit write must fail the request: %s", resp)

	var name string
	f.api.mustQueryRow(t, `SELECT name FROM guilds WHERE id = $1`, []any{mustID(t, f.guildID)}, &name)
	require.Equal(t, "Test Guild", name,
		"the mutation must roll back with its audit entry — they share one transaction (rule 2)")
}

// TestDeletingAGuildTakesItsAuditTrailWithIt pins the consequence described above, so that it is a
// recorded state rather than a surprise for whoever builds M14.
func TestDeletingAGuildTakesItsAuditTrailWithIt(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)
	guildID := mustID(t, f.guildID)

	var before int
	f.api.mustQueryRow(t, `SELECT count(*) FROM audit_log_entries WHERE guild_id = $1`,
		[]any{guildID}, &before)
	require.Positive(t, before, "creating the guild wrote an entry")

	resp := f.api.call(http.MethodDelete, "/api/v1/guilds/"+f.guildID, nil, withToken(f.ownerToken))
	require.Equal(t, http.StatusNoContent, resp.Code, resp)

	var after int
	f.api.mustQueryRow(t, `SELECT count(*) FROM audit_log_entries WHERE guild_id = $1`,
		[]any{guildID}, &after)
	require.Zero(t, after,
		"audit_log_entries cascades from guilds, so a deleted guild's trail goes with it — the durable "+
			"record of an instance-level action belongs in instance_audit_log (rule 14)")
}

// TestAScopedTokenIsBoundedOnTheGuildSurface is the check M12 shipped without.
//
// M12 is the first milestone to put a mutating surface within reach of a delegated credential, and its
// routes were mounted bare — auth guards even the read-only GET /users/@me with RequireScope, while every
// guild route had none. An `identify`-only token deleted a guild and answered 204; reproduced before the
// scopes were added, which is why this test asserts the guild survives rather than only the status.
//
// Confirmed by removal: drop the `write` middleware from the delete route and this fails.
func TestAScopedTokenIsBoundedOnTheGuildSurface(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)

	mint := func(t *testing.T, scopes ...string) string {
		t.Helper()
		res := f.api.call(http.MethodPost, "/api/v1/auth/tokens",
			map[string]any{"name": "bot", "scopes": scopes}, withToken(f.ownerToken))
		require.Equal(t, http.StatusCreated, res.Code, res)
		return res.field(t, "value")
	}

	t.Run("an identify-only token reaches nothing here", func(t *testing.T) {
		token := mint(t, "identify")

		read := f.api.call(http.MethodGet, "/api/v1/guilds/"+f.guildID, nil, withToken(token))
		require.Equal(t, http.StatusForbidden, read.Code, read)

		del := f.api.call(http.MethodDelete, "/api/v1/guilds/"+f.guildID, nil, withToken(token))
		require.Equal(t, http.StatusForbidden, del.Code, del)

		var n int
		f.api.mustQueryRow(t, `SELECT count(*) FROM guilds WHERE id = $1`,
			[]any{mustID(t, f.guildID)}, &n)
		require.Equal(t, 1, n, "the guild must survive a token that was never granted guilds.write")
	})

	// Read does not imply write, and the reverse holds too — a scope bounds a credential, and holding one
	// is not a reason to be granted another.
	t.Run("guilds.read reads but does not write", func(t *testing.T) {
		token := mint(t, "guilds.read")

		read := f.api.call(http.MethodGet, "/api/v1/guilds/"+f.guildID, nil, withToken(token))
		require.Equal(t, http.StatusOK, read.Code, read)

		write := f.api.call(http.MethodPatch, "/api/v1/guilds/"+f.guildID,
			map[string]any{"name": "Renamed"}, withToken(token))
		require.Equal(t, http.StatusForbidden, write.Code, write)
	})

	t.Run("guilds.write writes but does not read", func(t *testing.T) {
		token := mint(t, "guilds.write")

		write := f.api.call(http.MethodPatch, "/api/v1/guilds/"+f.guildID,
			map[string]any{"name": "Renamed"}, withToken(token))
		require.Equal(t, http.StatusOK, write.Code, write)

		read := f.api.call(http.MethodGet, "/api/v1/guilds/"+f.guildID, nil, withToken(token))
		require.Equal(t, http.StatusForbidden, read.Code, read)
	})

	// A user's own access token is unrestricted, which is what makes scopes a restriction on delegation
	// rather than a permission system of their own.
	t.Run("a user token needs no scope", func(t *testing.T) {
		read := f.api.call(http.MethodGet, "/api/v1/guilds/"+f.guildID, nil, withToken(f.ownerToken))
		require.Equal(t, http.StatusOK, read.Code, read)
	})
}

// TestANewRolesPositionSurvivesADeletion covers the collision a deletion used to cause.
//
// M12 took a new role's position from the role count, so deleting a role left a gap and the next creation
// collided with an existing position — or landed below one. There is no unique constraint to catch it
// (deliberately: see migration 000015), so the corruption was silent.
//
// **The direction of the placement assertion is inverted from M12's**, and the inversion is the point
// rather than a weakened test. M12 asserted each new role went *above* the last, which was right for a
// milestone with no hierarchy. M13 places a new role at the *bottom*, immediately above @everyone,
// because a role created at the top is one its own creator cannot then edit, delete, assign or
// reposition. What has to survive both rules — and does — is that no two roles ever share a position,
// because two roles at one position are neither above nor below each other and that dissolves the
// ordering the hierarchy rests on.
func TestANewRolesPositionSurvivesADeletion(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)
	rolesPath := fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID)

	create := func(t *testing.T, name string) (id string, position float64) {
		t.Helper()
		res := f.api.call(http.MethodPost, rolesPath, map[string]any{"name": name},
			withToken(f.ownerToken))
		require.Equal(t, http.StatusCreated, res.Code, res)

		var body map[string]any
		require.NoError(t, json.Unmarshal(res.Body, &body))
		return body["id"].(string), body["position"].(float64)
	}

	position := func(t *testing.T, id string) float64 {
		t.Helper()
		res := f.api.call(http.MethodGet, rolesPath, nil, withToken(f.ownerToken))
		require.Equal(t, http.StatusOK, res.Code, res)

		var listed []map[string]any
		require.NoError(t, json.Unmarshal(res.Body, &listed))
		for _, r := range listed {
			if r["id"] == id {
				return r["position"].(float64)
			}
		}
		t.Fatalf("role %s is not in the listing", id)
		return 0
	}

	mods, modsPos := create(t, "mods")
	require.Equal(t, float64(1), modsPos, "the first role goes just above @everyone")

	admins, adminsPos := create(t, "admins")
	require.Equal(t, float64(1), adminsPos, "and so does the next")
	require.Greater(t, position(t, mods), adminsPos, "the earlier role shifted up to make room")

	// Remove the middle one, leaving a gap the old count-based placement would have collided into.
	del := f.api.call(http.MethodDelete, rolesPath+"/"+mods, nil, withToken(f.ownerToken))
	require.Equal(t, http.StatusNoContent, del.Code, del)

	_, newPos := create(t, "after-the-gap")
	require.Equal(t, float64(1), newPos, "still the bottom, whatever the churn above it")
	require.Greater(t, position(t, admins), newPos, "and the survivor is still above it")

	// The property that survives both placement rules, and the one that actually matters.
	var dupes int
	f.api.mustQueryRow(t,
		`SELECT count(*) FROM (SELECT position FROM roles WHERE guild_id = $1
		                       GROUP BY position HAVING count(*) > 1) d`,
		[]any{mustID(t, f.guildID)}, &dupes)
	require.Zero(t, dupes, "two roles must not share a position")
}

// TestVoiceOnlyFieldsAreRefusedElsewhere keeps bitrate and user_limit where the schema says they live.
func TestVoiceOnlyFieldsAreRefusedElsewhere(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)
	path := fmt.Sprintf("/api/v1/guilds/%s/channels", f.guildID)

	text := f.api.call(http.MethodPost, path,
		map[string]any{"name": "general", "type": 0, "bitrate": 96000}, withToken(f.ownerToken))
	require.Equal(t, http.StatusBadRequest, text.Code,
		"a text channel must not accept a bitrate: %s", text)

	voice := f.api.call(http.MethodPost, path,
		map[string]any{"name": "lounge", "type": 2, "bitrate": 96000, "user_limit": 10},
		withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, voice.Code, voice)

	plain := f.api.call(http.MethodPost, path,
		map[string]any{"name": "plain", "type": 0}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, plain.Code, plain)

	patched := f.api.call(http.MethodPatch, "/api/v1/channels/"+plain.field(t, "id"),
		map[string]any{"user_limit": 5}, withToken(f.ownerToken))
	require.Equal(t, http.StatusBadRequest, patched.Code,
		"nor may an update add one: %s", patched)
}

// TestACategoryCannotNestInsideACategory is the reciprocal of the parent check.
//
// The parent was verified to be a category; the child never was. The channel list is flat with one level
// of nesting, so a deeper tree is data no client can render.
func TestACategoryCannotNestInsideACategory(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)
	path := fmt.Sprintf("/api/v1/guilds/%s/channels", f.guildID)

	parent := f.api.call(http.MethodPost, path,
		map[string]any{"name": "top", "type": 4}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, parent.Code, parent)

	nested := f.api.call(http.MethodPost, path,
		map[string]any{"name": "inner", "type": 4, "parent_id": parent.field(t, "id")},
		withToken(f.ownerToken))
	require.Equal(t, http.StatusBadRequest, nested.Code, nested)

	// A text channel under the same category is fine.
	child := f.api.call(http.MethodPost, path,
		map[string]any{"name": "chat", "type": 0, "parent_id": parent.field(t, "id")},
		withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, child.Code, child)
}

// TestAModeratorCanMuteWithoutManagingTheGuild is finding 15.
//
// `need` started at PermManageGuild and the moderation bits were added to it, so mute and deafen were
// undeliverable as standalone grants: the only way to let somebody mute was to also let them rename the
// guild. Two of nineteen defined permission bits were unreachable.
func TestAModeratorCanMuteWithoutManagingTheGuild(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)

	role := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID),
		map[string]any{"name": "voice-mod", "permissions": roles.PermMuteMembers},
		withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, role.Code, role)
	f.api.mustExec(t, `INSERT INTO guild_member_roles (guild_id, user_id, role_id) VALUES ($1, $2, $3)`,
		mustID(t, f.guildID), mustID(t, f.memberID), mustID(t, role.field(t, "id")))

	f.api.mustExec(t, `INSERT INTO guild_members (guild_id, user_id) VALUES ($1, $2)`,
		mustID(t, f.guildID), mustID(t, f.strangerID))
	target := fmt.Sprintf("/api/v1/guilds/%s/members/%s", f.guildID, f.strangerID)

	muted := f.api.call(http.MethodPatch, target, map[string]any{"mute": true}, withToken(f.memberToken))
	require.Equal(t, http.StatusOK, muted.Code,
		"PermMuteMembers alone must be enough to mute: %s", muted)

	// And it still grants nothing else.
	nick := f.api.call(http.MethodPatch, target, map[string]any{"nickname": "Renamed"},
		withToken(f.memberToken))
	require.Equal(t, http.StatusForbidden, nick.Code,
		"muting must not carry the right to rename: %s", nick)

	deaf := f.api.call(http.MethodPatch, target, map[string]any{"deaf": true}, withToken(f.memberToken))
	require.Equal(t, http.StatusForbidden, deaf.Code, deaf)
}

// TestOverwritesAreCleanedUpWithTheirTarget covers the orphan rows.
//
// target_id is polymorphic and so cannot be a foreign key, which means nothing cascades. A leftover member
// overwrite is not clutter: rejoin the guild and applyOverwrites matches it again, silently restoring a
// deny that nothing in the UI or the audit log explains.
func TestOverwritesAreCleanedUpWithTheirTarget(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)
	guildID := mustID(t, f.guildID)

	channel := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/channels", f.guildID),
		map[string]any{"name": "general", "type": 0}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, channel.Code, channel)
	channelID := mustID(t, channel.field(t, "id"))

	role := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID),
		map[string]any{"name": "mods"}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, role.Code, role)
	roleID := mustID(t, role.field(t, "id"))

	// Inserted directly: the overwrite endpoints are M13's.
	f.api.mustExec(t, `INSERT INTO permission_overwrites (channel_id, target_type, target_id, allow, deny)
	                   VALUES ($1, 0, $2, 0, 2), ($1, 1, $3, 0, 2)`,
		channelID, roleID, mustID(t, f.memberID))

	countOverwrites := func(t *testing.T) int {
		t.Helper()
		var n int
		f.api.mustQueryRow(t, `SELECT count(*) FROM permission_overwrites WHERE channel_id = $1`,
			[]any{channelID}, &n)
		return n
	}
	require.Equal(t, 2, countOverwrites(t))

	del := f.api.call(http.MethodDelete,
		fmt.Sprintf("/api/v1/guilds/%s/roles/%s", f.guildID, role.field(t, "id")), nil,
		withToken(f.ownerToken))
	require.Equal(t, http.StatusNoContent, del.Code, del)
	require.Equal(t, 1, countOverwrites(t), "a deleted role must take its overwrites with it")

	kick := f.api.call(http.MethodDelete,
		fmt.Sprintf("/api/v1/guilds/%s/members/%s", f.guildID, f.memberID), nil,
		withToken(f.ownerToken))
	require.Equal(t, http.StatusNoContent, kick.Code, kick)
	require.Zero(t, countOverwrites(t), "a removed member must take theirs too")

	// Nothing else in the guild was touched.
	var channels int
	f.api.mustQueryRow(t, `SELECT count(*) FROM channels WHERE guild_id = $1`, []any{guildID}, &channels)
	require.Equal(t, 1, channels)
}

// TestGuildPayloadsMatchTheContract closes the gap the generated types do not.
//
// backend/oapi-codegen.yaml argues that generated types make drift a compile error — and that only holds
// for a handler that actually decodes into one. These handlers do not: they declare their own request
// structs so they can carry `validate:` tags, and their own response types so ids marshal as snowflakes.
// So the generated package is a checked artifact of the contract rather than a participant in the
// handlers, and nothing was comparing M12's four response shapes against what the document declares.
//
// That is the gap that previously hid an unsatisfiable MintedApiToken schema and eight missing error
// codes. Guild, Channel, Role and Member all carry `additionalProperties: false` and a full `required`
// list, so a key the server sends and the document does not declare is a client that silently drops it,
// and a key the document requires and the server omits is a client that generates a field it never
// receives. Both directions are checked here.
func TestGuildPayloadsMatchTheContract(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)
	schemas := contractSchemas(t)

	channel := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/channels", f.guildID),
		map[string]any{"name": "general", "type": 0}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, channel.Code, channel)

	role := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID),
		map[string]any{"name": "mods"}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, role.Code, role)

	member := f.api.call(http.MethodPatch,
		fmt.Sprintf("/api/v1/guilds/%s/members/%s", f.guildID, f.memberID),
		map[string]any{"nickname": "Nick"}, withToken(f.ownerToken))
	require.Equal(t, http.StatusOK, member.Code, member)

	guild := f.api.call(http.MethodGet, "/api/v1/guilds/"+f.guildID, nil, withToken(f.ownerToken))
	require.Equal(t, http.StatusOK, guild.Code, guild)

	for _, tc := range []struct {
		schema string
		body   []byte
	}{
		{"Guild", guild.Body},
		{"Channel", channel.Body},
		{"Role", role.Body},
		{"Member", member.Body},
	} {
		t.Run(tc.schema, func(t *testing.T) {
			declared, required := declaredProperties(t, schemas[tc.schema])

			var body map[string]any
			require.NoError(t, json.Unmarshal(tc.body, &body), "decoding: %s", tc.body)

			var got []string
			for k := range body {
				got = append(got, k)
			}
			sort.Strings(got)

			assert.Equal(t, declared, got,
				"%s sent %v; the contract declares %v", tc.schema, got, declared)

			for _, name := range required {
				_, present := body[name]
				assert.Truef(t, present, "%s declares %q required and the server omitted it", tc.schema, name)
			}
		})
	}

	// The listing endpoints return the same shapes inside an array, and an empty one must be `[]` rather
	// than null — a client that has to handle both writes the check once and forgets it somewhere.
	t.Run("listings are arrays", func(t *testing.T) {
		empty := f.api.call(http.MethodPost, "/api/v1/guilds",
			map[string]any{"name": "Empty"}, withToken(f.strangerToken))
		require.Equal(t, http.StatusCreated, empty.Code, empty)

		list := f.api.call(http.MethodGet,
			fmt.Sprintf("/api/v1/guilds/%s/channels", empty.field(t, "id")), nil,
			withToken(f.strangerToken))
		require.Equal(t, http.StatusOK, list.Code, list)
		require.JSONEq(t, "[]", string(list.Body), "a guild with no channels must answer with an empty array")
	})
}

// TestPermissionsAreAStringOnTheWire pins the representation end to end, not only in the marshaler's unit
// test — a handler that took an int would produce a number here and nothing else would notice.
func TestPermissionsAreAStringOnTheWire(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)

	res := f.api.call(http.MethodGet, fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID), nil,
		withToken(f.ownerToken))
	require.Equal(t, http.StatusOK, res.Code, res)

	var list []map[string]any
	require.NoError(t, json.Unmarshal(res.Body, &list))
	require.NotEmpty(t, list)

	_, ok := list[0]["permissions"].(string)
	require.Truef(t, ok, "permissions must be a quoted decimal string, got %T: %s",
		list[0]["permissions"], res.Body)
}

// TestANonMemberLearnsNothingFromARefusal covers the two paths a security review found answering a
// non-member with something other than 404.
//
// Both were the same defect: a public error evaluated *before* the authorization call, on a path whose
// entire design is that an unauthorized caller cannot tell "does not exist" from "not yours". The branch's
// own non-member sweep missed both because it targets an ordinary member and always sends
// `{"name": "hijacked"}` — never the owner, and never a voice-only field.
//
// Confirmed by removal: move either check back above its authorizeWith and the matching subtest fails.
func TestANonMemberLearnsNothingFromARefusal(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)

	text := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/channels", f.guildID),
		map[string]any{"name": "general", "type": 0}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, text.Code, text)

	voice := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/channels", f.guildID),
		map[string]any{"name": "lounge", "type": 2}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, voice.Code, voice)

	// The reference answer: a guild id that names nothing at all.
	absent := f.api.call(http.MethodDelete,
		fmt.Sprintf("/api/v1/guilds/%s/members/%s", f.next(t), f.strangerID), nil,
		withToken(f.strangerToken))
	require.Equal(t, http.StatusNotFound, absent.Code, absent)

	// Targeting the *owner* used to answer 409 with a public message, which told a stranger both that the
	// guild was real and who owned it — for any private guild, given one id and a candidate user list.
	t.Run("removing the owner", func(t *testing.T) {
		resp := f.api.call(http.MethodDelete,
			fmt.Sprintf("/api/v1/guilds/%s/members/%s", f.guildID, f.ownerID), nil,
			withToken(f.strangerToken))
		require.Equal(t, http.StatusNotFound, resp.Code,
			"a non-member must not learn who owns a guild they cannot see: %s", resp)
	})

	// The owner themselves still gets the real reason rather than a bare refusal — the property the
	// original ordering was protecting, which survives the fix.
	t.Run("the owner still gets a usable message", func(t *testing.T) {
		resp := f.api.call(http.MethodDelete,
			fmt.Sprintf("/api/v1/guilds/%s/members/%s", f.guildID, f.ownerID), nil,
			withToken(f.ownerToken))
		require.Equal(t, http.StatusConflict, resp.Code, resp)
		require.Contains(t, string(resp.Body), "transfer ownership", resp)
	})

	// A voice-only field on a channel the caller cannot see used to answer 400 for a text channel and 404
	// for a voice one, disclosing both that the id was live and what type it carried.
	t.Run("a voice-only field on a text channel", func(t *testing.T) {
		resp := f.api.call(http.MethodPatch, "/api/v1/channels/"+text.field(t, "id"),
			map[string]any{"bitrate": 64000}, withToken(f.strangerToken))
		require.Equal(t, http.StatusNotFound, resp.Code,
			"a non-member must not learn a channel exists or what type it is: %s", resp)
	})

	t.Run("a voice-only field on a voice channel", func(t *testing.T) {
		resp := f.api.call(http.MethodPatch, "/api/v1/channels/"+voice.field(t, "id"),
			map[string]any{"bitrate": 64000}, withToken(f.strangerToken))
		require.Equal(t, http.StatusNotFound, resp.Code, resp)
	})

	// And a member with the permission still gets the validation error, which is what makes it useful.
	t.Run("a permitted caller still gets the validation error", func(t *testing.T) {
		resp := f.api.call(http.MethodPatch, "/api/v1/channels/"+text.field(t, "id"),
			map[string]any{"bitrate": 64000}, withToken(f.ownerToken))
		require.Equal(t, http.StatusBadRequest, resp.Code, resp)
	})
}

// TestAGuildIsBoundedInChannelsAndRoles pins the ceilings an optimization review asked for.
//
// The member listing is cursor-paginated and clamped at 100; the channel and role listings return
// everything, because a client needs the whole tree to render a sidebar and paginating would make it fetch
// in a loop. So the bound has to live where the rows are created. Measured at 5,010 channels, an unbounded
// listing is 782 kB of row data and a 740 kB sort against 8 buffers for the capped member list on a
// 15,000-member guild — the ceiling is what makes "the channel list is a hot path" a statement with a
// number behind it.
//
// Seeded directly rather than through 500 API calls, which would take minutes and test the rate limiter.
func TestAGuildIsBoundedInChannelsAndRoles(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)
	guildID := mustID(t, f.guildID)

	t.Run("channels", func(t *testing.T) {
		f.api.mustExec(t, `INSERT INTO channels (id, guild_id, type, name, position)
		                   SELECT $1::bigint + g, $2, 0, 'seeded-' || g, g FROM generate_series(1, 500) g`,
			int64(900000000000000000), guildID)

		resp := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/channels", f.guildID),
			map[string]any{"name": "one-too-many", "type": 0}, withToken(f.ownerToken))
		require.Equal(t, http.StatusConflict, resp.Code,
			"a guild at its channel ceiling must refuse another: %s", resp)
		require.Contains(t, string(resp.Body), "at most 500 channels", resp)

		// Deleting one makes room again — the ceiling is on the count, not on ids ever issued.
		f.api.mustExec(t, `DELETE FROM channels WHERE id = $1`, int64(900000000000000001))

		ok := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/channels", f.guildID),
			map[string]any{"name": "room-again", "type": 0}, withToken(f.ownerToken))
		require.Equal(t, http.StatusCreated, ok.Code, ok)
	})

	t.Run("roles", func(t *testing.T) {
		g2 := f.api.call(http.MethodPost, "/api/v1/guilds",
			map[string]any{"name": "Roles"}, withToken(f.ownerToken))
		require.Equal(t, http.StatusCreated, g2.Code, g2)
		id := g2.field(t, "id")

		// 249 seeded plus the @everyone guild creation wrote = 250.
		f.api.mustExec(t, `INSERT INTO roles (id, guild_id, name, position, permissions)
		                   SELECT $1::bigint + g, $2, 'seeded-' || g, g, 0 FROM generate_series(1, 249) g`,
			int64(910000000000000000), mustID(t, id))

		resp := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/roles", id),
			map[string]any{"name": "one-too-many"}, withToken(f.ownerToken))
		require.Equal(t, http.StatusConflict, resp.Code,
			"a guild at its role ceiling must refuse another: %s", resp)
		require.Contains(t, string(resp.Body), "at most 250 roles", resp)
	})

	// The outermost object, which shipped capped and untested — a limit nothing exercises is a limit that
	// can be removed without a red build. Fifty, lowered from the hundred M12 first shipped because M72a's
	// discovery directory makes owning many guilds useful to a spammer in a way nothing did before.
	t.Run("guilds per account", func(t *testing.T) {
		acct := f.api.newAccount("hoarder", "hoarder@example.com", "hoarder-device")

		// 49 seeded plus the one every account gets from the fixture is not the shape here: this account
		// owns nothing yet, so 50 seeded takes it exactly to the ceiling.
		f.api.mustExec(t, `INSERT INTO guilds (id, name, owner_id)
		                   SELECT $1::bigint + g, 'seeded-' || g, $2 FROM generate_series(1, 50) g`,
			int64(920000000000000000), mustID(t, acct.ID))

		resp := f.api.call(http.MethodPost, "/api/v1/guilds",
			map[string]any{"name": "one-too-many"}, withToken(acct.Tokens.AccessToken))
		require.Equal(t, http.StatusConflict, resp.Code,
			"an account at its guild ceiling must be refused: %s", resp)
		require.Contains(t, string(resp.Body), "at most 50 guilds", resp)

		// The ceiling is per account, not instance-wide: somebody else is unaffected.
		other := f.api.call(http.MethodPost, "/api/v1/guilds",
			map[string]any{"name": "unaffected"}, withToken(f.strangerToken))
		require.Equal(t, http.StatusCreated, other.Code, other)
	})

	// The ceiling is checked after authorization, so a stranger cannot use it to learn how full a guild is.
	t.Run("a non-member still gets 404, not the ceiling", func(t *testing.T) {
		resp := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/channels", f.guildID),
			map[string]any{"name": "probe", "type": 0}, withToken(f.strangerToken))
		require.Equal(t, http.StatusNotFound, resp.Code,
			"the ceiling must not become another oracle: %s", resp)
	})
}

// TestConcurrentRoleCreationsDoNotShareAPosition covers the race the max()-vs-count() fix left open.
//
// Reading a max and then inserting is a read-modify-write, and RunInTx sets no isolation level, so under
// READ COMMITTED neither transaction sees the other's uncommitted row and both take the same position.
// Migration 000015 declines a unique constraint on (guild_id, position) deliberately, so nothing
// downstream catches it — and position is the hierarchy M13 enforces over, where two roles at the same
// position are neither above nor below each other.
//
// Confirmed by removal: drop LockGuildRolePositions from CreateRole and this fails.
func TestConcurrentRoleCreationsDoNotShareAPosition(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)
	path := fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID)

	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)

	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := f.api.call(http.MethodPost, path,
				map[string]any{"name": fmt.Sprintf("race-%d", i)}, withToken(f.ownerToken))
			codes[i] = resp.Code
		}()
	}
	wg.Wait()

	for i, c := range codes {
		require.Equal(t, http.StatusCreated, c, "creation %d", i)
	}

	var dupes int
	f.api.mustQueryRow(t,
		`SELECT count(*) FROM (SELECT position FROM roles WHERE guild_id = $1
		                       GROUP BY position HAVING count(*) > 1) d`,
		[]any{mustID(t, f.guildID)}, &dupes)
	require.Zero(t, dupes,
		"concurrent creations must not land on the same position — it is the hierarchy M13 enforces over")
}

// TestUndefinedPermissionBitsAreRefused closes the input path permAll only guards on the output path.
//
// UnmarshalJSON deliberately preserves unknown high bits so a row written by a newer schema survives a
// round trip through an older binary. Accepting one from a *request* is a different decision: it means a
// later milestone defining that bit finds it already granted, which is verbatim the failure permAll's
// comment rejects ^Permission(0) to prevent. An Instance Admin reaches it because the tier short-circuits
// without consulting any bitfield.
func TestUndefinedPermissionBitsAreRefused(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)
	const bit62 = "4611686018427387904"

	t.Run("from an owner", func(t *testing.T) {
		resp := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID),
			map[string]any{"name": "future", "permissions": bit62}, withToken(f.ownerToken))
		require.Equal(t, http.StatusBadRequest, resp.Code, resp)
	})

	t.Run("from an instance admin", func(t *testing.T) {
		f.api.mustExec(t, `INSERT INTO instance_admins (user_id) VALUES ($1)`, mustID(t, f.strangerID))
		resp := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID),
			map[string]any{"name": "future", "permissions": bit62}, withToken(f.strangerToken))
		require.Equal(t, http.StatusBadRequest, resp.Code,
			"the tier short-circuits the escalation check, so the bit check must precede it: %s", resp)
	})

	t.Run("a defined bit still works", func(t *testing.T) {
		resp := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID),
			map[string]any{"name": "ok", "permissions": roles.PermManageMessages}, withToken(f.ownerToken))
		require.Equal(t, http.StatusCreated, resp.Code, resp)
	})
}

// TestAnEmptyMemberUpdateIsRefused covers the case where `need` is zero and Has(0) is true by design.
//
// Without it any member could PATCH any other member with an empty body, hold no permission at all, and
// append a row to a table migration 000016 says must never be swept.
func TestAnEmptyMemberUpdateIsRefused(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)
	target := fmt.Sprintf("/api/v1/guilds/%s/members/%s", f.guildID, f.memberID)

	before := f.auditCount(t)

	resp := f.api.call(http.MethodPatch, target, map[string]any{}, withToken(f.memberToken))
	require.Equal(t, http.StatusBadRequest, resp.Code,
		"a request that changes nothing must not pass a permission check that asks for nothing: %s", resp)

	require.Equal(t, before, f.auditCount(t), "and must write no audit row")
}

// TestUpdatingAMemberReportsTheirRoles pins the response shape against the schema's promise.
func TestUpdatingAMemberReportsTheirRoles(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)

	role := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID),
		map[string]any{"name": "mods"}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, role.Code, role)
	roleID := role.field(t, "id")

	f.api.mustExec(t, `INSERT INTO guild_member_roles (guild_id, user_id, role_id) VALUES ($1, $2, $3)`,
		mustID(t, f.guildID), mustID(t, f.memberID), mustID(t, roleID))

	resp := f.api.call(http.MethodPatch,
		fmt.Sprintf("/api/v1/guilds/%s/members/%s", f.guildID, f.memberID),
		map[string]any{"nickname": "Nick"}, withToken(f.ownerToken))
	require.Equal(t, http.StatusOK, resp.Code, resp)

	var body map[string]any
	require.NoError(t, json.Unmarshal(resp.Body, &body))
	held, _ := body["roles"].([]any)
	require.Len(t, held, 1,
		"a client refreshing its cache from this body must not lose the member's roles: %s", resp)
	require.Equal(t, roleID, held[0])
}

func (f *guildFixture) auditCount(t *testing.T) int {
	t.Helper()
	var n int
	f.api.mustQueryRow(t, `SELECT count(*) FROM audit_log_entries WHERE guild_id = $1`,
		[]any{mustID(t, f.guildID)}, &n)
	return n
}
