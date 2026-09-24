// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package messages

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// tag inserts a tag and applies it, by direct SQL: this package cannot import `tags`, for the reason it
// cannot import `guilds`.
func (f *fixture) tag(t *testing.T, name string, shared bool, owner, message snowflake.ID) snowflake.ID {
	t.Helper()
	id, err := f.ids.Next()
	require.NoError(t, err)
	f.exec(t, `INSERT INTO message_tags (id, guild_id, name, created_by, is_shared) VALUES ($1,$2,$3,$4,$5)`,
		int64(id), int64(f.guildID), name, int64(owner), shared)
	f.exec(t, `INSERT INTO message_tag_applications (tag_id, message_id, applied_by) VALUES ($1,$2,$3)`,
		int64(id), int64(message), int64(owner))
	return id
}

func tagNames(m Message) []string {
	out := []string{}
	for _, tg := range m.Tags {
		out = append(out, tg.Name)
	}
	return out
}

// TestTheListingCarriesEachMessagesTags is M17's optimization-review finding: the only way to read a
// message's tags was a request per message, 50 requests to draw a 50-message page. The listing now
// carries them, resolved for the page in one statement, with the private-tag filter the SQL carries.
func TestTheListingCarriesEachMessagesTags(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	tagged := f.send(t, f.member, "tagged")
	plain := f.send(t, f.member, "plain")
	f.tag(t, "spam", true, f.mod, tagged.ID)
	f.tag(t, "mine", false, f.member, tagged.ID)
	f.tag(t, "theirs", false, f.owner, tagged.ID)

	page, err := f.svc.List(f.ctx, actorOf(f.member), ListInput{ChannelID: f.channelID})
	require.NoError(t, err)
	require.Len(t, page, 2)

	byID := map[snowflake.ID]Message{}
	for _, m := range page {
		byID[m.ID] = m
	}
	require.ElementsMatch(t, []string{"spam", "mine"}, tagNames(byID[tagged.ID]),
		"shared tags and the caller's own private ones — never somebody else's private tag")
	require.NotNil(t, byID[plain.ID].Tags, "an untagged message carries an empty array, not null")
	require.Empty(t, byID[plain.ID].Tags)
}

// TestACredentialWithoutTagsReadSeesNullNotEmpty pins why the field is nullable. A token scoped to read
// messages and not tags must not see tags through the listing — a scope bounds a delegated credential —
// and answering [] would claim the message has none.
func TestACredentialWithoutTagsReadSeesNullNotEmpty(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	m := f.send(t, f.member, "tagged")
	f.tag(t, "spam", true, f.mod, m.ID)

	token := auth.Actor{
		Kind: auth.ActorAPIToken, UserID: f.member,
		Scopes: []auth.Scope{auth.ScopeMessagesRead, auth.ScopeMessagesWrite},
	}
	page, err := f.svc.List(f.ctx, token, ListInput{ChannelID: f.channelID})
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Nil(t, page[0].Tags, "without tags.read the listing must not reveal tags")

	sent, err := f.svc.Send(f.ctx, token, SendInput{ChannelID: f.channelID, Content: "new"})
	require.NoError(t, err)
	require.Nil(t, sent.Tags)

	withTags := token
	withTags.Scopes = append(withTags.Scopes, auth.ScopeTagsRead)
	page, err = f.svc.List(f.ctx, withTags, ListInput{ChannelID: f.channelID})
	require.NoError(t, err)
	require.Equal(t, []string{}, tagNames(page[0]))
	require.Equal(t, []string{"spam"}, tagNames(page[1]), "and with it, the tags arrive")
}

// TestSendAndEditCarryTheTagsTheMessageHas: a client replaces its copy of a message with what these
// return, so an edit must not answer with the message's tags missing.
func TestSendAndEditCarryTheTagsTheMessageHas(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	m := f.send(t, f.member, "before")
	require.NotNil(t, m.Tags, "a new message has an empty array")
	require.Empty(t, m.Tags)

	f.tag(t, "spam", true, f.mod, m.ID)
	edited, err := f.svc.Update(f.ctx, actorOf(f.member), UpdateInput{
		ChannelID: f.channelID, MessageID: m.ID, Content: "after",
	})
	require.NoError(t, err)
	require.Equal(t, []string{"spam"}, tagNames(edited))
}
