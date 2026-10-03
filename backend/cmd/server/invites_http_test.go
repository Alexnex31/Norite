// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/auth"
)

// TestTheInviteResponsesMatchTheContract reads every invite response, and the names they and messages now
// nest, against the schemas declared for them (M20a). Each schema is additionalProperties: false with a full
// required list, so a field sent and not declared, or declared and not sent, fails here — the gap M15
// through M17 each found for their own shapes when nothing read them.
func TestTheInviteResponsesMatchTheContract(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	schemas := contractSchemas(t)

	owner := a.newAccount("owner", "owner@example.com", "laptop")
	joiner := a.newAccount("joiner", "joiner@example.com", "phone")
	ownerToken, joinerToken := withToken(owner.Tokens.AccessToken), withToken(joiner.Tokens.AccessToken)

	guild := a.call(http.MethodPost, "/api/v1/guilds", map[string]any{"name": "Guild"}, ownerToken)
	require.Equal(t, http.StatusCreated, guild.Code, guild)
	guildID := guild.field(t, "id")
	channel := a.call(http.MethodPost, "/api/v1/guilds/"+guildID+"/channels",
		map[string]any{"name": "general", "type": 0}, ownerToken)
	require.Equal(t, http.StatusCreated, channel.Code, channel)
	channelID := channel.field(t, "id")

	created := a.call(http.MethodPost, "/api/v1/channels/"+channelID+"/invites",
		map[string]any{"max_uses": 5, "expires_in_seconds": 3600}, ownerToken)
	require.Equal(t, http.StatusCreated, created.Code, created)
	code := created.field(t, "code")

	listed := a.call(http.MethodGet, "/api/v1/guilds/"+guildID+"/invites", nil, ownerToken)
	require.Equal(t, http.StatusOK, listed.Code, listed)
	preview := a.call(http.MethodPost, "/api/v1/invites/preview", map[string]any{"code": code}, joinerToken)
	require.Equal(t, http.StatusOK, preview.Code, preview)
	redeemed := a.call(http.MethodPost, "/api/v1/invites/redeem", map[string]any{"code": code}, joinerToken)
	require.Equal(t, http.StatusOK, redeemed.Code, redeemed)
	sent := a.call(http.MethodPost, "/api/v1/channels/"+channelID+"/messages",
		map[string]any{"content": "hello"}, joinerToken)
	require.Equal(t, http.StatusCreated, sent.Code, sent)

	var listedBody []map[string]any
	require.NoError(t, json.Unmarshal(listed.Body, &listedBody))
	require.Len(t, listedBody, 1)
	var previewBody, sentBody map[string]any
	require.NoError(t, json.Unmarshal(preview.Body, &previewBody))
	require.NoError(t, json.Unmarshal(sent.Body, &sentBody))

	nested := func(object map[string]any, key string) map[string]any {
		inner, ok := object[key].(map[string]any)
		require.True(t, ok, "%s is an object here: %v", key, object[key])
		return inner
	}

	for _, tc := range []struct {
		what   string
		schema string
		body   []byte
		object map[string]any
	}{
		{what: "POST an invite", schema: "GuildInvite", body: created.Body},
		{what: "GET a guild's invites", schema: "GuildInvite", object: listedBody[0]},
		{what: "an invite's inviter", schema: "PublicUser", object: nested(listedBody[0], "inviter")},
		{what: "POST a preview", schema: "GuildInvitePreview", object: previewBody},
		{what: "a preview's inviter", schema: "PublicUser", object: nested(previewBody, "inviter")},
		{what: "POST a redemption", schema: "Guild", body: redeemed.Body},
		{what: "a message's author", schema: "PublicUser", object: nested(sentBody, "author")},
	} {
		t.Run(tc.what, func(t *testing.T) {
			object := tc.object
			if object == nil {
				require.NoError(t, json.Unmarshal(tc.body, &object))
			}
			var got []string
			for k := range object {
				got = append(got, k)
			}
			sort.Strings(got)
			declared, required := declaredProperties(t, schemas[tc.schema])
			assert.Equal(t, declared, got, "%s sent %v; %s declares %v", tc.what, got, tc.schema, declared)
			assert.Equal(t, declared, required,
				"%s: every field is always present, so the contract should require all of them", tc.schema)
		})
	}
}

// TestJoiningAddsTheGuildToTheJoinerAndTheJoinerToTheGuild is the gateway half of a redemption. The
// joiner's open connection receives GUILD_CREATE first, which is what adds the guild to it, and then the
// GUILD_MEMBER_ADD every member receives. That event is a Member and carries no name: no REST read returns
// another member's name, and the gateway never discloses more than REST (/code-review). Every frame is
// schema-checked on the way in, so this is also the event's contract test.
func TestJoiningAddsTheGuildToTheJoinerAndTheJoinerToTheGuild(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	url := serveGateway(t, f.api.handler)
	general := createChannel(t, f, map[string]any{"name": "general", "type": 0})

	invite := f.api.call(http.MethodPost, "/api/v1/channels/"+general+"/invites", map[string]any{},
		withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, invite.Code, invite)

	member, stranger := connected(t, url, f.memberToken), connected(t, url, f.strangerToken)

	joined := f.api.call(http.MethodPost, "/api/v1/invites/redeem",
		map[string]any{"code": invite.field(t, "code")}, withToken(f.strangerToken))
	require.Equal(t, http.StatusOK, joined.Code, joined)

	created := stranger.expect("GUILD_CREATE")
	assert.Equal(t, f.guildID, created.field(t, "id"))

	for _, c := range []*gatewayClient{stranger, member} {
		added := c.expect("GUILD_MEMBER_ADD")
		assert.Equal(t, f.guildID, added.field(t, "guild_id"))
		assert.Equal(t, f.strangerID, added.field(t, "user_id"))
		assert.NotContains(t, string(added.raw), "stranger", "the event carries no name REST would not give")
	}

	// And the joiner's connection now carries the guild's events, with no reconnect.
	send(t, f, f.memberToken, general, "welcome")
	assert.Equal(t, "welcome", stranger.expect("MESSAGE_CREATE").field(t, "content"))
}

// TestTheRoutesTakingACodeCarryTheirOwnRateLimit: preview, redemption and revocation sit in the invites
// bucket as well as the base one, and the bucket counts independently of the base, so a client looping on
// codes is throttled while its ordinary traffic is not.
func TestTheRoutesTakingACodeCarryTheirOwnRateLimit(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	account := a.newAccount("guesser", "guesser@example.com", "laptop")
	const client = "203.0.113.77"
	token := withToken(account.Tokens.AccessToken)

	var throttled *response
	for i := 0; i < 60; i++ {
		resp := a.call(http.MethodPost, "/api/v1/invites/redeem",
			map[string]any{"code": "BBBBBBBBBBBBBBBB"}, token, fromIP(client))
		if resp.Code == http.StatusTooManyRequests {
			throttled = resp
			break
		}
		require.Equal(t, http.StatusNotFound, resp.Code, "request %d: %s", i+1, resp)
	}
	require.NotNil(t, throttled,
		"the invites bucket (%s) must throttle before the base limit (%s)", inviteRateLimit, testConfig().RateLimit)
	assert.Equal(t, "rate_limited", throttled.errorBody().Code)

	preview := a.call(http.MethodPost, "/api/v1/invites/preview",
		map[string]any{"code": "BBBBBBBBBBBBBBBB"}, token, fromIP(client))
	assert.Equal(t, http.StatusTooManyRequests, preview.Code, "preview shares the bucket")

	me := a.call(http.MethodGet, "/api/v1/users/@me", nil, token, fromIP(client))
	assert.Equal(t, http.StatusOK, me.Code, "ordinary traffic is counted in the base bucket alone")
}

// TestListingLiveCodesNeedsTheWriteScope: a guild's invite list is live codes, each a way into the guild,
// so a read-only API token minted by somebody holding MANAGE_GUILD must not be able to fetch them. Found by
// /code-review on M20a's first version, which mounted the listing under guilds.read.
func TestListingLiveCodesNeedsTheWriteScope(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	path := "/api/v1/guilds/" + f.guildID + "/invites"

	for _, tc := range []struct {
		scopes []string
		want   int
	}{
		{[]string{"guilds.read"}, http.StatusForbidden},
		{[]string{"guilds.read", "guilds.write"}, http.StatusOK},
	} {
		minted := f.api.call(http.MethodPost, "/api/v1/auth/tokens", map[string]any{
			"name": "bot", "scopes": tc.scopes,
		}, withToken(f.ownerToken))
		require.Equal(t, http.StatusCreated, minted.Code, minted)
		res := f.api.call(http.MethodGet, path, nil, withToken(minted.field(t, "value")))
		assert.Equal(t, tc.want, res.Code, "%v: %s", tc.scopes, res)
	}
}

// TestEveryMalformedCodeIsTheOneNotFound: the contract promises one 404 for a malformed code as for an
// unknown one, and a validator on the field answered an empty or oversized code with 400 instead
// (/code-review). Over HTTP, because the validator was the handler's and the service never saw them.
func TestEveryMalformedCodeIsTheOneNotFound(t *testing.T) {
	t.Parallel()
	f := newGuildFixture(t)
	for _, code := range []string{"", strings.Repeat("B", 65), strings.Repeat("B", 15), "not a code at all"} {
		for _, route := range []string{"/api/v1/invites/preview", "/api/v1/invites/redeem", "/api/v1/invites/revoke"} {
			res := f.api.call(http.MethodPost, route, map[string]any{"code": code}, withToken(f.memberToken))
			assert.Equal(t, http.StatusNotFound, res.Code, "%s with %q: %s", route, code, res)
		}
	}
}
