// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package messages

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// denyView writes the member-tier overwrite that removes PermViewChannel from one person in the fixture's
// channel. This is the state M13's bug and M16's manual pass both needed and no happy path builds: somebody
// holding a moderation bit who cannot see the channel they hold it in.
func (f *fixture) denyView(t *testing.T, user snowflake.ID) {
	t.Helper()
	f.exec(t, `INSERT INTO permission_overwrites (channel_id, target_type, target_id, allow, deny)
	           VALUES ($1,$2,$3,0,$4)`,
		int64(f.channelID), roles.OverwriteTargetMember, int64(user), roles.PermViewChannel.Int64())
}

// addMember creates a user and joins them to the fixture's guild with nothing but the default grant.
func (f *fixture) addMember(t *testing.T, name string) snowflake.ID {
	t.Helper()
	id, err := f.ids.Next()
	require.NoError(t, err)
	f.exec(t, `INSERT INTO users (id, username, email, display_name, created_at, updated_at)
	           VALUES ($1,$2::text,$2::text||'@example.test',$2::text,now(),now())`, int64(id), name)
	f.exec(t, `INSERT INTO guild_members (guild_id, user_id, joined_at) VALUES ($1,$2,now())`,
		int64(f.guildID), int64(id))
	return id
}

// edit rewrites a message as its author, which is the only way history rows are ever written.
func (f *fixture) edit(t *testing.T, author snowflake.ID, id snowflake.ID, content string) {
	t.Helper()
	_, err := f.svc.Update(f.ctx, actorOf(author),
		UpdateInput{ChannelID: f.channelID, MessageID: id, Content: content})
	require.NoError(t, err)
}

func (f *fixture) history(author snowflake.ID, id snowflake.ID) (MessageHistory, error) {
	return f.svc.History(f.ctx, actorOf(author),
		HistoryInput{ChannelID: f.channelID, MessageID: id})
}

// TestAModeratorReadsEveryPriorVersionNewestFirst is the happy path, and it checks the ordering rather than
// only the count: the whole reason 000022 replaces 000020's index is that this list is ordered by id.
func TestAModeratorReadsEveryPriorVersionNewestFirst(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	msg := f.send(t, f.member, "first")
	f.edit(t, f.member, msg.ID, "second")
	f.edit(t, f.member, msg.ID, "third")

	got, err := f.history(f.mod, msg.ID)
	require.NoError(t, err)

	require.Equal(t, msg.ID, got.MessageID)
	require.NotNil(t, got.CurrentContent)
	require.Equal(t, "third", *got.CurrentContent, "the envelope must carry what the message says now")
	require.False(t, got.IsE2E)
	require.Nil(t, got.DeletedAt)
	require.NotNil(t, got.EditedAt)

	require.Len(t, got.Versions, 2)
	require.Equal(t, "second", got.Versions[0].Content, "newest prior version first")
	require.Equal(t, "first", got.Versions[1].Content)
	require.Greater(t, got.Versions[0].ID, got.Versions[1].ID, "ordered by id, which is what the index serves")
}

// TestANeverEditedMessageIsAnEmptyHistoryAndNotA404 pins the answer for the commonest message there is.
//
// A 404 would make "has this been edited" a separate question with its own answer, and messages.edited_at
// already publishes it to anybody who can read the backlog.
func TestANeverEditedMessageIsAnEmptyHistoryAndNotA404(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	msg := f.send(t, f.member, "never touched")

	got, err := f.history(f.mod, msg.ID)
	require.NoError(t, err)
	require.Empty(t, got.Versions)
	require.NotNil(t, got.CurrentContent)
	require.Equal(t, "never touched", *got.CurrentContent)
	require.Nil(t, got.EditedAt, "a message that was never edited has no edited_at to report")
}

// TestReadingSomebodyElsesHistoryNeedsManageMessages is the gate, from both sides of the refusal split.
func TestReadingSomebodyElsesHistoryNeedsManageMessages(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	msg := f.send(t, f.member, "first")
	f.edit(t, f.member, msg.ID, "second")

	// A member with the full default grant — view, history and send — and not the moderation bit. They can
	// read this message in the backlog; they cannot read what it used to say.
	_, err := f.history(f.owner, msg.ID)
	require.NoError(t, err, "the owner holds every permission by layer 2")

	other := f.addMember(t, "other")
	_, err = f.history(other, msg.ID)
	require.ErrorIs(t, err, httpx.ErrForbidden,
		"a member who can see the channel and lacks MANAGE_MESSAGES is refused 403 — they already know "+
			"the channel and the message exist")

	stranger, err := f.ids.Next()
	require.NoError(t, err)
	_, err = f.history(stranger, msg.ID)
	require.ErrorIs(t, err, httpx.ErrNotFound,
		"a non-member must be refused as not-found, or the 403 confirms the guild exists")
}

// TestAModeratorDeniedViewStillReadsTheHistory is M16a's central decision, and the reason
// AuthorizeChannelIgnoringVisibility exists.
//
// M16 settled that a moderator reads a reported message's content whether or not they can currently view
// the channel it came from. This is the same content reached by the same moderator one step later, so a
// refusal here would mean they can read what a reported message says now and not what it said before —
// inside the flow this milestone was scheduled to serve.
//
// If somebody later "simplifies" History to call AuthorizeChannelUnlocked, this is the test that fails.
func TestAModeratorDeniedViewStillReadsTheHistory(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	msg := f.send(t, f.member, "first")
	f.edit(t, f.member, msg.ID, "second")
	f.denyView(t, f.mod)

	// The state is real: the same moderator cannot read the backlog at all.
	_, err := f.svc.List(f.ctx, actorOf(f.mod), ListInput{ChannelID: f.channelID})
	require.ErrorIs(t, err, httpx.ErrNotFound, "the channel is hidden from them, which is the premise")

	got, err := f.history(f.mod, msg.ID)
	require.NoError(t, err, "a MANAGE_MESSAGES holder reads the history of a channel they cannot open")
	require.Len(t, got.Versions, 1)
	require.Equal(t, "first", got.Versions[0].Content)
}

// TestAMemberWhoCanNeitherViewNorModerateGetsNotFound is the other half of the decision above, and the one
// that would be lost by a plausible simplification.
//
// Lifting the view *requirement* and lifting the refusal *downgrade* are separate things. If both went,
// this member would get 403 where an id naming nothing gets 404 — a probe for hidden channels in their own
// guild, which is the oracle M14 closed.
func TestAMemberWhoCanNeitherViewNorModerateGetsNotFound(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	msg := f.send(t, f.member, "first")
	f.edit(t, f.member, msg.ID, "second")

	other := f.addMember(t, "other")
	f.denyView(t, other)

	_, err := f.history(other, msg.ID)
	require.ErrorIs(t, err, httpx.ErrNotFound,
		"a member who can neither view the channel nor moderate it must not be able to tell a real "+
			"message id in a hidden channel from one that names nothing")
}

// TestAnAuthorReadsTheirOwnHistoryWithoutTheModerationBit is the carve-out, decided explicitly rather than
// by omission because the roadmap entry asked for that.
//
// It discloses nothing to anybody new — the author wrote every version — and refusing somebody their own
// prior words is hard to defend.
func TestAnAuthorReadsTheirOwnHistoryWithoutTheModerationBit(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	msg := f.send(t, f.member, "first")
	f.edit(t, f.member, msg.ID, "second")

	got, err := f.history(f.member, msg.ID)
	require.NoError(t, err, "the author holds no moderation bit and reads their own history")
	require.Len(t, got.Versions, 1)
	require.Equal(t, "first", got.Versions[0].Content)

	// And the carve-out is exactly that wide: it does not spill onto anybody else's message.
	other := f.addMember(t, "other")
	theirs := f.send(t, other, "theirs")
	f.edit(t, other, theirs.ID, "theirs, edited")

	_, err = f.history(f.member, theirs.ID)
	require.ErrorIs(t, err, httpx.ErrForbidden,
		"being an author of one message must not grant reading another author's history")
}

// TestASoftDeletedMessagesHistoryIsStillReadable — 000020 made the delete soft partly so this would work,
// and says so in as many words. Deleting fast must not be how a reported message's history vanishes.
func TestASoftDeletedMessagesHistoryIsStillReadable(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	msg := f.send(t, f.member, "first")
	f.edit(t, f.member, msg.ID, "second")
	require.NoError(t, f.svc.Delete(f.ctx, actorOf(f.mod), f.channelID, msg.ID))

	got, err := f.history(f.mod, msg.ID)
	require.NoError(t, err)
	require.NotNil(t, got.DeletedAt, "the envelope says the message is gone")
	require.Len(t, got.Versions, 1)
	require.NotNil(t, got.CurrentContent, "what it said when it was deleted is still the current version")
}

// TestAMessageFromAnotherChannelIsNotReachable — the channel in the route is what was authorized, so
// reaching a message through the wrong one would mean the permission check covered one channel and the
// read landed in another. loadInChannel's rule, and the same 404.
func TestAMessageFromAnotherChannelIsNotReachable(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	other := f.newChannel(t, "other", ChannelGuildText)

	msg, err := f.svc.Send(f.ctx, actorOf(f.member), SendInput{ChannelID: other, Content: "elsewhere"})
	require.NoError(t, err)

	_, err = f.history(f.mod, msg.ID)
	require.ErrorIs(t, err, httpx.ErrNotFound)
}

// TestAnEncryptedMessageYieldsNoVersionsAndNoContent is rule 13, and it is written to fail if the shape
// argument ever stops holding rather than to pass because of it.
//
// E2E is DM-only, a DM belongs to no guild, and this route is guild-scoped — so the row below cannot be
// created through any API and is inserted directly. That is exactly the "closed by construction" claim
// M11a made about password reset not bypassing the second factor, and M11a's lesson is that such a claim
// stops being true quietly.
func TestAnEncryptedMessageYieldsNoVersionsAndNoContent(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	msg := f.send(t, f.member, "plaintext")
	f.edit(t, f.member, msg.ID, "still plaintext")

	// Flip the flag on a message that already has history, so the exclusion has something to withhold.
	f.exec(t, `UPDATE messages SET is_e2e = true, content = 'ciphertext-abcdef' WHERE id = $1`, int64(msg.ID))

	got, err := f.history(f.mod, msg.ID)
	require.NoError(t, err, "the report still resolves; it is the content that is withheld")
	require.True(t, got.IsE2E, "and the response says why, rather than looking like a message with no history")
	require.Nil(t, got.CurrentContent)
	require.Empty(t, got.Versions, "the exclusion is in the query, so there are no rows to blank")

	// What a client actually receives is the thing that matters, not which field somebody remembered to
	// omit — M16's argument for marshaling the shape and searching it.
	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "ciphertext",
		"no ciphertext may appear anywhere in the response")
	require.NotContains(t, string(encoded), "plaintext",
		"and neither may a prior version written before the message was encrypted")
}

// TestAGuildScopedHistoryCannotReachADirectMessage is the shape half of rule 13 — the property that makes
// the exclusion above unreachable today. Both are asserted, because the exclusion must survive this
// property changing and this property must be known to have changed.
func TestAGuildScopedHistoryCannotReachADirectMessage(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	dmChannel, err := f.ids.Next()
	require.NoError(t, err)
	dmMessage, err := f.ids.Next()
	require.NoError(t, err)
	f.exec(t, `INSERT INTO channels (id, guild_id, name, type, position, created_at, updated_at)
	           VALUES ($1,NULL,NULL,1,0,now(),now())`, int64(dmChannel))
	f.exec(t, `INSERT INTO messages (id, channel_id, author_id, content, type, is_e2e, created_at)
	           VALUES ($1,$2,$3,'dm ciphertext',0,true,now())`,
		int64(dmMessage), int64(dmChannel), int64(f.member))

	_, err = f.svc.History(f.ctx, actorOf(f.member),
		HistoryInput{ChannelID: dmChannel, MessageID: dmMessage})
	require.ErrorIs(t, err, httpx.ErrNotFound,
		"guildauth.guildOf refuses a channel belonging to no guild, which is what keeps E2E content out "+
			"of this surface by shape as well as by the query")
}

// TestTheHistoryPagesOnIdAndTheCursorExcludesItsBoundary covers the reason this list is paginated at all:
// nothing bounds how many times a message may be edited.
func TestTheHistoryPagesOnIdAndTheCursorExcludesItsBoundary(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	msg := f.send(t, f.member, "v0")
	for i := 1; i <= 5; i++ {
		f.edit(t, f.member, msg.ID, "v"+strings.Repeat("x", i))
	}

	first, err := f.svc.History(f.ctx, actorOf(f.mod),
		HistoryInput{ChannelID: f.channelID, MessageID: msg.ID, Limit: 2})
	require.NoError(t, err)
	require.Len(t, first.Versions, 2)

	cursor := first.Versions[1].ID
	second, err := f.svc.History(f.ctx, actorOf(f.mod),
		HistoryInput{ChannelID: f.channelID, MessageID: msg.ID, Before: &cursor, Limit: 2})
	require.NoError(t, err)
	require.Len(t, second.Versions, 2)

	require.Less(t, second.Versions[0].ID, cursor, "the cursor is exclusive")
	for _, a := range first.Versions {
		for _, b := range second.Versions {
			require.NotEqual(t, a.ID, b.ID, "pages must not overlap")
		}
	}

	// The clamp, which is what stops a caller asking for the whole history of a pathological message.
	all, err := f.svc.History(f.ctx, actorOf(f.mod),
		HistoryInput{ChannelID: f.channelID, MessageID: msg.ID, Limit: maxPageSize + 500})
	require.NoError(t, err)
	require.Len(t, all.Versions, 5, "there are only five, but the limit was clamped rather than honored")
}
