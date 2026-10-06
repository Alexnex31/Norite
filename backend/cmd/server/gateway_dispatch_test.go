// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// dispatched is one op 0 frame, as a test sees it.
type dispatched struct {
	t   string
	s   int64
	raw json.RawMessage
}

func (d dispatched) field(t *testing.T, name string) any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(d.raw, &m))
	return m[name]
}

// nextDispatch reads until the next dispatch, skipping heartbeat acks. Every frame is schema-checked on the
// way (gatewayClient.read), so every event these tests receive is also a contract test.
func (g *gatewayClient) nextDispatch() dispatched {
	g.t.Helper()
	for {
		f := g.read()
		if f.Op != gatewayproto.OpDispatch {
			continue
		}
		return dispatched{t: *f.T, s: *f.S, raw: f.D}
	}
}

// expect asserts the next dispatch's type, which is how the negative assertions below are made
// deterministic: events reach a connection in the order they were published, so if a sentinel event
// arrives next, whatever was published before it and not delivered was never going to be.
func (g *gatewayClient) expect(eventType string) dispatched {
	g.t.Helper()
	d := g.nextDispatch()
	require.Equal(g.t, eventType, d.t, "payload: %s", d.raw)
	return d
}

// connected opens an identified connection for token and consumes READY.
func connected(t *testing.T, url, token string) *gatewayClient {
	t.Helper()
	c := dialGateway(t, url, nil)
	c.hello()
	c.identify(token, "dev")
	c.ready()
	return c
}

func createChannel(t *testing.T, f *guildFixture, body map[string]any) string {
	t.Helper()
	res := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/channels", f.guildID), body, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, res.Code, res)
	return res.field(t, "id")
}

func denyView(t *testing.T, f *guildFixture, channelID string, targetType int, targetID string) {
	t.Helper()
	res := f.api.call(http.MethodPut, fmt.Sprintf("/api/v1/channels/%s/permissions/%s", channelID, targetID),
		map[string]any{"type": targetType, "allow": "0", "deny": fmt.Sprint(roles.PermViewChannel.Int64())},
		withToken(f.ownerToken))
	require.Equal(t, http.StatusOK, res.Code, res)
}

func send(t *testing.T, f *guildFixture, token, channelID, content string) string {
	t.Helper()
	res := f.api.call(http.MethodPost, "/api/v1/channels/"+channelID+"/messages",
		map[string]any{"content": content}, withToken(token))
	require.Equal(t, http.StatusCreated, res.Code, res)
	return res.field(t, "id")
}

// assertAuthor checks a message event names who sent it (M20a). The schema allows a null author, for a
// deleted account, so a frame carrying null would pass every schema check and still name nobody.
func assertAuthor(t *testing.T, d dispatched, id, username string) {
	t.Helper()
	author, ok := d.field(t, "author").(map[string]any)
	require.True(t, ok, "the message names its author: %s", d.raw)
	assert.Equal(t, id, author["id"])
	assert.Equal(t, username, author["username"])
	assert.NotEmpty(t, author["display_name"])
}

func rename(t *testing.T, f *guildFixture, name string) {
	t.Helper()
	res := f.api.call(http.MethodPatch, "/api/v1/guilds/"+f.guildID, map[string]any{"name": name}, withToken(f.ownerToken))
	require.Equal(t, http.StatusOK, res.Code, res)
}

// The done-when's second clause: DISPATCH events for guild activity, over the real router. A message reaches
// every member who can view its channel, in order, with sequence numbers counting up from READY's.
func TestGuildActivityIsDispatchedToItsMembers(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)
	general := createChannel(t, f, map[string]any{"name": "general", "type": 0})

	owner, member := connected(t, url, f.ownerToken), connected(t, url, f.memberToken)

	id := send(t, f, f.memberToken, general, "hello")
	for _, c := range []*gatewayClient{owner, member} {
		got := c.expect("MESSAGE_CREATE")
		assert.Equal(t, id, got.field(t, "id"))
		assert.Equal(t, "hello", got.field(t, "content"))
		assert.Nil(t, got.field(t, "tags"), "the gateway never carries tags: private ones differ per reader")
		assert.Equal(t, int64(2), got.s, "READY was 1")
		assertAuthor(t, got, f.memberID, "member")
	}

	res := f.api.call(http.MethodPatch, "/api/v1/channels/"+general+"/messages/"+id,
		map[string]any{"content": "hello, edited"}, withToken(f.memberToken))
	require.Equal(t, http.StatusOK, res.Code, res)
	res = f.api.call(http.MethodDelete, "/api/v1/channels/"+general+"/messages/"+id, nil, withToken(f.memberToken))
	require.Equal(t, http.StatusNoContent, res.Code, res)

	for _, c := range []*gatewayClient{owner, member} {
		edited := c.expect("MESSAGE_UPDATE")
		assert.Equal(t, "hello, edited", edited.field(t, "content"))
		assertAuthor(t, edited, f.memberID, "member")
		assert.Equal(t, id, c.expect("MESSAGE_DELETE").field(t, "id"))
	}
}

// Rule 1 at fan-out: a channel's events reach only those who can view it, resolved now. The member denied
// view receives nothing about the hidden channel, not its messages and not its deletion, while the owner,
// who bypasses overwrites, receives all of it.
func TestAHiddenChannelsEventsReachOnlyThoseWhoCanSeeIt(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)
	general := createChannel(t, f, map[string]any{"name": "general", "type": 0})
	staff := createChannel(t, f, map[string]any{"name": "staff", "type": 0})
	denyView(t, f, staff, 0, f.everyoneID)

	owner, member := connected(t, url, f.ownerToken), connected(t, url, f.memberToken)

	send(t, f, f.ownerToken, staff, "staff only")
	owner.expect("MESSAGE_CREATE")

	res := f.api.call(http.MethodDelete, "/api/v1/channels/"+staff, nil, withToken(f.ownerToken))
	require.Equal(t, http.StatusNoContent, res.Code, res)
	assert.Equal(t, staff, owner.expect("CHANNEL_DELETE").field(t, "id"))

	// The sentinel: a message in a channel the member can see. Arriving next proves the staff message and
	// the staff channel's deletion were not sent to them, rather than merely late.
	send(t, f, f.ownerToken, general, "everyone")
	assert.Equal(t, "everyone", member.expect("MESSAGE_CREATE").field(t, "content"))
}

// A channel created inside a category is announced only to those the category's copied overwrites admit.
func TestANewChannelInALockedCategoryIsAnnouncedOnlyToThoseAdmitted(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)
	category := createChannel(t, f, map[string]any{"name": "locked", "type": 4})
	denyView(t, f, category, 1, f.memberID)

	owner, member := connected(t, url, f.ownerToken), connected(t, url, f.memberToken)

	inside := createChannel(t, f, map[string]any{"name": "inside", "type": 0, "parent_id": category})
	assert.Equal(t, inside, owner.expect("CHANNEL_CREATE").field(t, "id"))

	outside := createChannel(t, f, map[string]any{"name": "outside", "type": 0})
	assert.Equal(t, outside, member.expect("CHANNEL_CREATE").field(t, "id"), "the locked channel was never announced")
}

// Removing a member tells them, and from then on they are not an audience: fan-out asks the database, not
// the connection's memory of READY.
func TestARemovedMemberHearsNothingMoreFromTheGuild(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)
	owner, member := connected(t, url, f.ownerToken), connected(t, url, f.memberToken)

	res := f.api.call(http.MethodDelete, fmt.Sprintf("/api/v1/guilds/%s/members/%s", f.guildID, f.memberID), nil,
		withToken(f.ownerToken))
	require.Equal(t, http.StatusNoContent, res.Code, res)

	assert.Equal(t, f.guildID, member.expect("GUILD_DELETE").field(t, "id"))
	assert.Equal(t, f.memberID, owner.expect("GUILD_MEMBER_REMOVE").field(t, "user_id"))

	rename(t, f, "Renamed")
	owner.expect("GUILD_UPDATE")

	// The sentinel for the removed member is an event about their own account: a guild they create.
	res = f.api.call(http.MethodPost, "/api/v1/guilds", map[string]any{"name": "Theirs"}, withToken(f.memberToken))
	require.Equal(t, http.StatusCreated, res.Code, res)
	assert.Equal(t, "Theirs", member.expect("GUILD_CREATE").field(t, "name"), "the rename never reached them")
}

// The case the connection's memory gets wrong: a membership removed without any event (here by hand; in
// production, a GUILD_DELETE the at-most-once bus dropped). The connection still lists the guild, and the
// fresh membership check at fan-out is what keeps it from hearing any more.
func TestFanOutAsksTheDatabaseNotTheConnectionsMemory(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)
	owner, member := connected(t, url, f.ownerToken), connected(t, url, f.memberToken)

	f.api.mustExec(t, `DELETE FROM guild_members WHERE guild_id = $1 AND user_id = $2`,
		mustID(t, f.guildID), mustID(t, f.memberID))

	rename(t, f, "Secret plans")
	owner.expect("GUILD_UPDATE")

	res := f.api.call(http.MethodPost, "/api/v1/guilds", map[string]any{"name": "Theirs"}, withToken(f.memberToken))
	require.Equal(t, http.StatusCreated, res.Code, res)
	assert.Equal(t, "Theirs", member.expect("GUILD_CREATE").field(t, "name"),
		"a non-member must not hear the guild's rename, whatever its READY said")
}

// A guild created while connected is announced to its creator and joins their connection, so its later
// events arrive without a reconnect.
func TestANewGuildJoinsItsCreatorsConnection(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)
	stranger := connected(t, url, f.strangerToken)

	res := f.api.call(http.MethodPost, "/api/v1/guilds", map[string]any{"name": "New"}, withToken(f.strangerToken))
	require.Equal(t, http.StatusCreated, res.Code, res)
	created := stranger.expect("GUILD_CREATE")
	guildID := created.field(t, "id").(string)

	res = f.api.call(http.MethodPatch, "/api/v1/guilds/"+guildID, map[string]any{"name": "Newer"}, withToken(f.strangerToken))
	require.Equal(t, http.StatusOK, res.Code, res)
	assert.Equal(t, "Newer", stranger.expect("GUILD_UPDATE").field(t, "name"))
}

// Rule 5: a refused mutation commits nothing and dispatches nothing. The member's rename is refused; the
// owner's that follows is the next thing either connection sees.
func TestARefusedMutationDispatchesNothing(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)
	owner := connected(t, url, f.ownerToken)

	res := f.api.call(http.MethodPatch, "/api/v1/guilds/"+f.guildID, map[string]any{"name": "Hijacked"},
		withToken(f.memberToken))
	require.Equal(t, http.StatusForbidden, res.Code, res)

	rename(t, f, "Legitimate")
	assert.Equal(t, "Legitimate", owner.expect("GUILD_UPDATE").field(t, "name"))
}

// Roles and permissions: a role assignment tells the guild what changed about the member and that
// permissions may have moved; a role edit sends the whole role list.
func TestRoleAndPermissionChangesAreDispatched(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)
	member := connected(t, url, f.memberToken)

	role := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID),
		map[string]any{"name": "helper"}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, role.Code, role)
	roles := member.expect("GUILD_ROLES_UPDATE").field(t, "roles").([]any)
	assert.Len(t, roles, 2, "@everyone and the new role, the whole list")

	res := f.api.call(http.MethodPut, fmt.Sprintf("/api/v1/guilds/%s/members/%s/roles/%s", f.guildID, f.memberID,
		role.field(t, "id")), nil, withToken(f.ownerToken))
	require.Less(t, res.Code, 300, res)
	updated := member.expect("GUILD_MEMBER_UPDATE")
	assert.Equal(t, []any{role.field(t, "id")}, updated.field(t, "roles"))
	assert.Equal(t, f.guildID, member.expect("GUILD_PERMISSIONS_UPDATE").field(t, "guild_id"))
}

// History governs the backlog, and an edit reaches into it: rewriting a message posted before a member could
// read the channel's history would otherwise hand them its new text over the gateway, while REST refuses them
// the same message. So MESSAGE_UPDATE needs the history bit as well as view, and MESSAGE_CREATE, which is new
// rather than backlog, needs view alone.
func TestAnEditDoesNotReachAMemberWhoCannotReadHistory(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)
	desk := createChannel(t, f, map[string]any{"name": "desk", "type": 0})

	old := send(t, f, f.ownerToken, desk, "written before you arrived")
	res := f.api.call(http.MethodPut, fmt.Sprintf("/api/v1/channels/%s/permissions/%s", desk, f.memberID),
		map[string]any{"type": 1, "allow": "0", "deny": fmt.Sprint(roles.PermReadMessageHistory.Int64())},
		withToken(f.ownerToken))
	require.Equal(t, http.StatusOK, res.Code, res)
	listed := f.api.call(http.MethodGet, "/api/v1/channels/"+desk+"/messages", nil, withToken(f.memberToken))
	require.Equal(t, http.StatusForbidden, listed.Code, "REST refuses the backlog: %s", listed)

	member := connected(t, url, f.memberToken)
	res = f.api.call(http.MethodPatch, "/api/v1/channels/"+desk+"/messages/"+old,
		map[string]any{"content": "rewritten"}, withToken(f.ownerToken))
	require.Equal(t, http.StatusOK, res.Code, res)
	send(t, f, f.ownerToken, desk, "new, and so not backlog")

	got := member.expect("MESSAGE_CREATE")
	assert.Equal(t, "new, and so not backlog", got.field(t, "content"), "the edit must not have been delivered")
}

// A transfer moves layer 2 from one member to another, so both are told their permissions changed, as every
// other path that changes permissions tells them.
func TestAnOwnershipTransferTellsMembersTheirPermissionsChanged(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)
	owner, member := connected(t, url, f.ownerToken), connected(t, url, f.memberToken)

	res := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/owner", f.guildID),
		map[string]any{"user_id": f.memberID}, withToken(f.ownerToken))
	require.Equal(t, http.StatusOK, res.Code, res)

	for _, c := range []*gatewayClient{owner, member} {
		assert.Equal(t, f.memberID, c.expect("GUILD_UPDATE").field(t, "owner_id"))
		assert.Equal(t, f.guildID, c.expect("GUILD_PERMISSIONS_UPDATE").field(t, "guild_id"))
	}
}

// Deleting a category leaves its channels at the top level, and a client showing them nested under it is
// told so, each channel only to those who can view it.
func TestDeletingACategoryUpdatesTheChannelsItHeld(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)
	category := createChannel(t, f, map[string]any{"name": "cat", "type": 4})
	open := createChannel(t, f, map[string]any{"name": "open", "type": 0, "parent_id": category})
	hidden := createChannel(t, f, map[string]any{"name": "hidden", "type": 0, "parent_id": category})
	denyView(t, f, hidden, 1, f.memberID)
	member := connected(t, url, f.memberToken)

	res := f.api.call(http.MethodDelete, "/api/v1/channels/"+category, nil, withToken(f.ownerToken))
	require.Equal(t, http.StatusNoContent, res.Code, res)

	assert.Equal(t, category, member.expect("CHANNEL_DELETE").field(t, "id"))
	updated := member.expect("CHANNEL_UPDATE")
	assert.Equal(t, open, updated.field(t, "id"))
	assert.Nil(t, updated.field(t, "parent_id"))

	// The hidden child's update never came: the next event is the sentinel.
	send(t, f, f.ownerToken, open, "sentinel")
	assert.Equal(t, "sentinel", member.expect("MESSAGE_CREATE").field(t, "content"))
}
