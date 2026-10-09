// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package messages

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// tokenOf is the same account acting through an API token that may write messages.
func tokenOf(id snowflake.ID) auth.Actor {
	return auth.Actor{
		Kind: auth.ActorAPIToken, UserID: id,
		Scopes: []auth.Scope{auth.ScopeMessagesRead, auth.ScopeMessagesWrite},
	}
}

func (f *fixture) storedType(t *testing.T, id snowflake.ID) int16 {
	t.Helper()
	var typ int16
	require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT type FROM messages WHERE id = $1`, int64(id)).Scan(&typ))
	return typ
}

// TestAMessageATokenSendsIsMarkedAndAPersonsIsNot is M22's tag, decided by the instance from the actor.
//
// One account, two credentials. The person's own message must stay unmarked, or every message would carry
// the tag and it would say nothing; the token's must be marked without having asked.
func TestAMessageATokenSendsIsMarkedAndAPersonsIsNot(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	typed := f.send(t, f.member, "typed")
	require.Equal(t, TypeDefault, typed.Type)
	require.Equal(t, TypeDefault, f.storedType(t, typed.ID))

	scripted, err := f.svc.Send(f.ctx, tokenOf(f.member), SendInput{ChannelID: f.channelID, Content: "scripted"})
	require.NoError(t, err)
	require.Equal(t, TypeAutomation, scripted.Type, "the response must carry the mark too")
	require.Equal(t, TypeAutomation, f.storedType(t, scripted.ID))

	// And a reader sees the difference, which is the point of storing it.
	page, err := f.svc.List(f.ctx, actorOf(f.member), ListInput{ChannelID: f.channelID})
	require.NoError(t, err)
	require.Len(t, page, 2)
	require.Equal(t, TypeAutomation, page[0].Type)
	require.Equal(t, TypeDefault, page[1].Type)
}

// TestAnEditByATokenMarksTheMessageAndNothingUnmarksIt covers the same question one step later.
//
// A token is its owner's, so it may rewrite what the owner typed. Left unmarked, a script's words would
// stand under a message drawn as a person's. And the person editing afterwards must not clear the mark:
// the message was, at some point, written by automation, and an edit is how that would be hidden.
func TestAnEditByATokenMarksTheMessageAndNothingUnmarksIt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	msg := f.send(t, f.member, "typed")

	// A person's own edit of their own message marks nothing.
	byPerson, err := f.svc.Update(f.ctx, actorOf(f.member), UpdateInput{
		ChannelID: f.channelID, MessageID: msg.ID, Content: "typed, corrected",
	})
	require.NoError(t, err)
	require.Equal(t, TypeDefault, byPerson.Type)

	byToken, err := f.svc.Update(f.ctx, tokenOf(f.member), UpdateInput{
		ChannelID: f.channelID, MessageID: msg.ID, Content: "rewritten by a script",
	})
	require.NoError(t, err)
	require.Equal(t, TypeAutomation, byToken.Type)
	require.Equal(t, TypeAutomation, f.storedType(t, msg.ID))

	again, err := f.svc.Update(f.ctx, actorOf(f.member), UpdateInput{
		ChannelID: f.channelID, MessageID: msg.ID, Content: "typed again",
	})
	require.NoError(t, err)
	require.Equal(t, TypeAutomation, again.Type, "a later edit by the person must not unmark it")
	require.Equal(t, TypeAutomation, f.storedType(t, msg.ID))
}

// TestAnEditNeverLowersTheType is the statement's half: GREATEST, not an assignment.
//
// Values above 1 are reserved for system messages. None is written yet, so one is placed by hand. An edit
// that assigned the actor's type would turn it into an ordinary or an automated message, whoever edited.
func TestAnEditNeverLowersTheType(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	for _, actor := range []auth.Actor{actorOf(f.member), tokenOf(f.member)} {
		msg := f.send(t, f.member, "placeholder")
		f.exec(t, `UPDATE messages SET type = 5 WHERE id = $1`, int64(msg.ID))

		updated, err := f.svc.Update(f.ctx, actor, UpdateInput{
			ChannelID: f.channelID, MessageID: msg.ID, Content: "edited",
		})
		require.NoError(t, err)
		require.Equal(t, int16(5), updated.Type, "actor kind %s", actor.Kind)
		require.Equal(t, int16(5), f.storedType(t, msg.ID))
	}
}
