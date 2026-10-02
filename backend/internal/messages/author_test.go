// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package messages

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// TestEveryMessageNamesItsAuthor covers the three statements a Message is built from (M20a): a send, a page
// of the backlog and an edit each carry the author's id, username and display name.
//
// The display name is set apart from the username first, because the fixture makes them equal and a test
// that cannot tell the two columns apart would pass with them swapped.
func TestEveryMessageNamesItsAuthor(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.exec(t, `UPDATE users SET display_name = 'Member Person' WHERE id = $1`, int64(f.member))
	f.exec(t, `UPDATE users SET display_name = 'The Owner' WHERE id = $1`, int64(f.owner))

	member := MessageAuthor{ID: f.member, Username: "member", DisplayName: "Member Person"}
	owner := MessageAuthor{ID: f.owner, Username: "owner", DisplayName: "The Owner"}

	sent := f.send(t, f.member, "hello")
	require.Equal(t, &member, sent.Author, "a send names its author")
	f.send(t, f.owner, "hi")

	page, err := f.svc.List(f.ctx, actorOf(f.member), ListInput{ChannelID: f.channelID})
	require.NoError(t, err)
	require.Len(t, page, 2)
	require.Equal(t, &owner, page[0].Author, "each message on a page names its own author")
	require.Equal(t, &member, page[1].Author)

	edited, err := f.svc.Update(f.ctx, actorOf(f.member), UpdateInput{
		ChannelID: f.channelID, MessageID: sent.ID, Content: "hello again",
	})
	require.NoError(t, err)
	require.Equal(t, &member, edited.Author, "an edit names its author")
}

// TestADeletedAccountsMessagesSurviveWithoutAName is the null M76a will rely on: a soft-deleted account's
// messages keep author_id and lose the name, so the placeholder M76a renames the account to never reaches a
// reader as somebody's name, and a client can tell "deleted account" from "nobody".
func TestADeletedAccountsMessagesSurviveWithoutAName(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	sent := f.send(t, f.member, "before")

	f.exec(t, `UPDATE users SET deleted_at = now(), username = 'deleted-placeholder' WHERE id = $1`, int64(f.member))

	page, err := f.svc.List(f.ctx, actorOf(f.owner), ListInput{ChannelID: f.channelID})
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, sent.ID, page[0].ID, "the message survives its author's deletion")
	require.NotNil(t, page[0].AuthorID)
	require.Equal(t, f.member, *page[0].AuthorID, "and keeps its author's id")
	require.Nil(t, page[0].Author, "but not a name")

	body, err := json.Marshal(page[0])
	require.NoError(t, err)
	require.Contains(t, string(body), `"author":null`, "the field is present and null, never omitted")
	require.NotContains(t, string(body), "deleted-placeholder")
}

// TestAMessageWithNoAuthorHasNoName covers author_id's own null, a system or webhook message (000020). No
// path writes one yet, so it is inserted directly.
func TestAMessageWithNoAuthorHasNoName(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := f.ids.MustNext()
	f.exec(t, `INSERT INTO messages (id, channel_id, author_id, content, type) VALUES ($1, $2, NULL, 'system', 1)`,
		int64(id), int64(f.channelID))

	page, err := f.svc.List(f.ctx, actorOf(f.member), ListInput{ChannelID: f.channelID})
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, snowflake.ID(id), page[0].ID)
	require.Nil(t, page[0].AuthorID)
	require.Nil(t, page[0].Author)
}
