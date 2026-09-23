// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package messages

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// record turns a fixture's guild into one that records, or back.
func (f *fixture) record(t *testing.T, on bool) {
	t.Helper()
	f.exec(t, `UPDATE guilds SET message_audit_enabled = $2 WHERE id = $1`, int64(f.guildID), on)
}

// recorded reads the whole recording log straight out of the table, bypassing the service.
//
// Deliberately not through GuildMessageAudit: the writer's tests must not depend on the reader's
// permission check being right, or a bug that refused everybody would make "writes nothing" pass.
func (f *fixture) recorded(t *testing.T) []struct {
	Action  string
	Content *string
} {
	t.Helper()
	rows, err := f.pool.Query(f.ctx,
		`SELECT action, content FROM message_audit_entries WHERE guild_id = $1 ORDER BY id`,
		int64(f.guildID))
	require.NoError(t, err)
	defer rows.Close()

	var out []struct {
		Action  string
		Content *string
	}
	for rows.Next() {
		var e struct {
			Action  string
			Content *string
		}
		require.NoError(t, rows.Scan(&e.Action, &e.Content))
		out = append(out, e)
	}
	require.NoError(t, rows.Err())
	return out
}

// grantMessageAudit gives a member PermViewMessageAudit through a role of their own.
func (f *fixture) grantMessageAudit(t *testing.T, user snowflake.ID) {
	t.Helper()
	roleID, err := f.ids.Next()
	require.NoError(t, err)
	f.exec(t, `INSERT INTO roles (id, guild_id, name, permissions, position, is_default, created_at, updated_at)
	           VALUES ($1,$2,'auditor',$3,2,false,now(),now())`,
		int64(roleID), int64(f.guildID), roles.PermViewMessageAudit.Int64())
	f.exec(t, `INSERT INTO guild_member_roles (guild_id, user_id, role_id) VALUES ($1,$2,$3)`,
		int64(f.guildID), int64(user), int64(roleID))
}

// TestAGuildWithRecordingOffWritesNothing is the milestone's first done-when clause.
//
// Every message mutation in a guild that never opted in, and the table stays empty. This is the case that
// describes every guild on every instance until somebody turns the switch on, which is why it is also the
// one the write-path measurement in 000023 is about.
func TestAGuildWithRecordingOffWritesNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	msg := f.send(t, f.member, "not recorded")
	_, err := f.svc.Update(f.ctx, actorOf(f.member), UpdateInput{
		ChannelID: f.channelID, MessageID: msg.ID, Content: "still not recorded",
	})
	require.NoError(t, err)
	require.NoError(t, f.svc.Delete(f.ctx, actorOf(f.member), f.channelID, msg.ID))

	require.Empty(t, f.recorded(t),
		"a guild that never opted in must record nothing at all — not an empty-content row, not a row "+
			"with the flag on it, nothing")
}

// TestAGuildWithRecordingOnRecordsEveryMutation is the second clause, and it counts rows rather than
// checking for absence.
//
// That matters more than it looks. The insert carries the opt-in as a join predicate and writes no rows
// when it does not match, so a statement broken in any way — a wrong parameter, a predicate that never
// matches — fails *silently* and looks exactly like a guild that did not opt in. The off case above
// cannot tell those apart; only this one can, which is why "wrote nothing" is never the whole test.
func TestAGuildWithRecordingOnRecordsEveryMutation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.record(t, true)

	msg := f.send(t, f.member, "first")
	_, err := f.svc.Update(f.ctx, actorOf(f.member), UpdateInput{
		ChannelID: f.channelID, MessageID: msg.ID, Content: "second",
	})
	require.NoError(t, err)
	require.NoError(t, f.svc.Delete(f.ctx, actorOf(f.member), f.channelID, msg.ID))

	entries := f.recorded(t)
	require.Len(t, entries, 3, "create, edit and delete each record one row")

	require.Equal(t, AuditCreate, entries[0].Action)
	require.Equal(t, "first", *entries[0].Content,
		"the create records what was posted, read back off the row rather than passed in")

	require.Equal(t, AuditEdit, entries[1].Action)
	require.Equal(t, "second", *entries[1].Content,
		"the edit records what the message now says; the text it replaced went to message_edit_history, "+
			"which is M16a's surface under a different permission")

	// The delete records the content it removed, which §2 left open and this milestone answered. Without
	// it a message posted before the switch went on and deleted after would have its text recorded
	// nowhere, which is the case an investigation is most likely to be about.
	require.Equal(t, AuditDelete, entries[2].Action)
	require.Equal(t, "second", *entries[2].Content)
}

// TestAnAuthorDeletingTheirOwnMessageIsStillRecorded is the property that separates this table from the
// guild audit log, and it would read as a bug against rule 2's wording.
//
// Rule 2 covers authority exercised over somebody else, so an author deleting their own message writes no
// `audit_log_entries` row — asserted elsewhere, and correct. A guild that switched recording on asked a
// different question: what was said here. A recording that skipped self-deletions would be one anybody
// could evade by deleting their own messages, which is precisely what the guild opted in to prevent.
func TestAnAuthorDeletingTheirOwnMessageIsStillRecorded(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.record(t, true)

	msg := f.send(t, f.member, "mine to delete")
	require.NoError(t, f.svc.Delete(f.ctx, actorOf(f.member), f.channelID, msg.ID))

	entries := f.recorded(t)
	require.Len(t, entries, 2)
	require.Equal(t, AuditDelete, entries[1].Action)

	// And the guild audit log still records nothing, because the two tables answer different questions.
	require.Zero(t, f.auditCount(t, ActionMessageDelete),
		"an author deleting their own message is authority over nobody — recording it is M16b's job and "+
			"auditing it is not rule 2's")
}

// TestTheRecordedActorIsWhoActedNotWhoWrote covers the one field whose obvious reading is wrong.
func TestTheRecordedActorIsWhoActedNotWhoWrote(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.record(t, true)

	msg := f.send(t, f.member, "written by the member")
	require.NoError(t, f.svc.Delete(f.ctx, actorOf(f.mod), f.channelID, msg.ID))

	var createActor, deleteActor int64
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT actor_id FROM message_audit_entries WHERE guild_id = $1 AND action = $2`,
		int64(f.guildID), AuditCreate).Scan(&createActor))
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT actor_id FROM message_audit_entries WHERE guild_id = $1 AND action = $2`,
		int64(f.guildID), AuditDelete).Scan(&deleteActor))

	require.Equal(t, int64(f.member), createActor)
	require.Equal(t, int64(f.mod), deleteActor,
		"actor_id is who performed the action; for a moderator deletion that is the moderator, and "+
			"recording the author there would make the log say the member deleted their own message")
}

// TestSwitchingRecordingOnIsNotRetroactive pins both halves of the toggle's meaning.
//
// Turning it on records nothing about what came before — there is no backfill and there should not be
// one, since copying content from `messages` would manufacture a record of a period nobody was told
// about. Turning it off stops new rows and removes none, which is what makes the toggle's own audit entry
// load-bearing rather than decorative.
func TestSwitchingRecordingOnIsNotRetroactive(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	before := f.send(t, f.member, "said before the switch")
	require.Empty(t, f.recorded(t))

	f.record(t, true)
	f.send(t, f.member, "said while recording")

	f.record(t, false)
	f.send(t, f.member, "said after the switch")

	entries := f.recorded(t)
	require.Len(t, entries, 1, "only the message sent while the switch was on is recorded")
	require.Equal(t, "said while recording", *entries[0].Content)

	// And editing the earlier message while recording is on records the edit but not the original: the
	// row that would have carried it was never written.
	f.record(t, true)
	_, err := f.svc.Update(f.ctx, actorOf(f.member), UpdateInput{
		ChannelID: f.channelID, MessageID: before.ID, Content: "edited while recording",
	})
	require.NoError(t, err)

	entries = f.recorded(t)
	require.Len(t, entries, 2)
	require.Equal(t, AuditEdit, entries[1].Action)
	require.Equal(t, "edited while recording", *entries[1].Content)
}

// TestARecordingGuildsLogIsBoundedByItsOwnBit is the disclosure boundary, asserted from three sides.
//
// This is the widest read in the product — every message in the guild, including channels the caller
// cannot view — so the bit is the whole of what stands between a member and the conversation. A member
// with the default grant is refused; one holding PermManageMessages is refused too, which is the
// assertion that would fail if somebody "simplified" the gate to M16a's; and the holder reads it.
func TestARecordingGuildsLogIsBoundedByItsOwnBit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.record(t, true)
	f.send(t, f.member, "recorded")

	_, err := f.svc.GuildMessageAudit(f.ctx, actorOf(f.member), GuildAuditInput{GuildID: f.guildID})
	require.ErrorIs(t, err, httpx.ErrForbidden,
		"@everyone's default grant must not reach a guild's recording log")

	// The moderator holds PermManageMessages, which gates M16a's edit history. It must not gate this:
	// that bit means "delete somebody else's message", and a guild wanting spam removed has not decided
	// that moderator reads every private channel.
	_, err = f.svc.GuildMessageAudit(f.ctx, actorOf(f.mod), GuildAuditInput{GuildID: f.guildID})
	require.ErrorIs(t, err, httpx.ErrForbidden,
		"PermManageMessages is M16a's gate and deliberately not this one — if this passes, the read has "+
			"been widened to every moderator in every recording guild")

	f.grantMessageAudit(t, f.member)
	entries, err := f.svc.GuildMessageAudit(f.ctx, actorOf(f.member), GuildAuditInput{GuildID: f.guildID})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "recorded", *entries[0].Content)
}

// TestANonMemberIsRefusedTheRecordingLogAsNotFound is the anti-enumeration half.
//
// A stranger must not learn that a guild exists, let alone that it records — guildauth answers 404 before
// any row is read, and the 404/403 split is the oracle M11 closed for session ids.
func TestANonMemberIsRefusedTheRecordingLogAsNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.record(t, true)
	f.send(t, f.member, "recorded")

	stranger, err := f.ids.Next()
	require.NoError(t, err)
	f.exec(t, `INSERT INTO users (id, username, email, display_name, created_at, updated_at)
	           VALUES ($1,'stranger','stranger@example.test','stranger',now(),now())`, int64(stranger))

	_, err = f.svc.GuildMessageAudit(f.ctx, actorOf(stranger), GuildAuditInput{GuildID: f.guildID})
	require.ErrorIs(t, err, httpx.ErrNotFound,
		"a non-member must be refused as not-found; a 403 would confirm the guild exists")
}

// TestAnEncryptedMessageIsNeverRecorded is rule 13, driven rather than argued.
//
// Unreachable today by shape — E2E is DM-only, a DM has no guild, and this table's guild_id is NOT NULL —
// which is exactly why it is driven anyway. M11a's lesson is that "closed by construction" stops being
// true quietly, and this milestone *stores* content rather than reading it, so the cost of the claim
// going stale is a copy of a conversation the instance promised it could not read.
func TestAnEncryptedMessageIsNeverRecorded(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.record(t, true)

	msg := f.send(t, f.member, "about to be marked encrypted")

	// Set by hand, because nothing sets it until M97 and there is no path that would produce this state.
	// That is the point: the exclusion has to hold for a row the service did not create.
	f.exec(t, `UPDATE messages SET is_e2e = true WHERE id = $1`, int64(msg.ID))
	f.exec(t, `DELETE FROM message_audit_entries WHERE guild_id = $1`, int64(f.guildID))

	_, err := f.svc.Update(f.ctx, actorOf(f.member), UpdateInput{
		ChannelID: f.channelID, MessageID: msg.ID, Content: "edited while encrypted",
	})
	require.NoError(t, err)
	require.NoError(t, f.svc.Delete(f.ctx, actorOf(f.member), f.channelID, msg.ID))

	require.Empty(t, f.recorded(t),
		"an E2E message must produce no row at all — not a row with null content, which would be the "+
			"exclusion written in two places")
}

// TestTheRecordingTableCannotHoldADMsContent is the structural half of rule 13, pinned in the schema.
//
// Modeled on TestAGuildScopedTableCannotHoldADMsAudit, which does the same for audit_log_entries. E2E is
// DM-only and a DM has no guild, so a NOT NULL guild_id is what makes "no DM's content can reach this
// table" a fact about the schema rather than a fact about the current writer. The day somebody makes it
// nullable for a DM-scoped recording, this fails and says why.
func TestTheRecordingTableCannotHoldADMsContent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	var nullable string
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT is_nullable FROM information_schema.columns
		  WHERE table_name = 'message_audit_entries' AND column_name = 'guild_id'`,
	).Scan(&nullable))

	require.Equal(t, "NO", nullable,
		"guild_id must stay NOT NULL: it is what makes rule 13's exclusion structural here rather than "+
			"dependent on the writer remembering, and this table stores content rather than reading it")
}

// TestTheRecordingLogPagesNewestFirst covers the cursor, which is an id and never created_at (M14).
func TestTheRecordingLogPagesNewestFirst(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.record(t, true)
	f.grantMessageAudit(t, f.member)

	for _, body := range []string{"one", "two", "three"} {
		f.send(t, f.member, body)
	}

	page, err := f.svc.GuildMessageAudit(f.ctx, actorOf(f.member),
		GuildAuditInput{GuildID: f.guildID, Limit: 2})
	require.NoError(t, err)
	require.Len(t, page, 2)
	require.Equal(t, "three", *page[0].Content, "newest first")
	require.Equal(t, "two", *page[1].Content)

	next, err := f.svc.GuildMessageAudit(f.ctx, actorOf(f.member),
		GuildAuditInput{GuildID: f.guildID, Limit: 2, Before: &page[1].ID})
	require.NoError(t, err)
	require.Len(t, next, 1)
	require.Equal(t, "one", *next[0].Content,
		"the cursor is exclusive, so the row it names must not repeat on the next page")
}

// TestTheBitDoesNotCarryAcrossGuilds is the cross-scope case: holding PermViewMessageAudit in one guild
// must say nothing about another, even for somebody who is a member of both.
//
// The existing cases cover a non-member (404) and a member without the bit (403). This is the third
// shape, and it is the one a permission bit could plausibly get wrong — `roles.Resolve` is per guild by
// construction, so the property holds because of where role rows live rather than because of a check
// somebody wrote, which is exactly the kind of claim worth pinning before it becomes folklore.
//
// The refusal must also be a refusal rather than an empty page. A guild that records nothing legitimately
// answers 200 with `[]`, so "no rows" is a real answer here — and if an out-of-scope read fell through to
// the query it would be indistinguishable from that, which is how a leak hides in a surface whose empty
// case is normal.
func TestTheBitDoesNotCarryAcrossGuilds(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.record(t, true)
	f.grantMessageAudit(t, f.member)
	f.send(t, f.member, "recorded in the first guild")

	// A second guild the same account belongs to, recording, where they hold nothing beyond @everyone.
	other, err := f.ids.Next()
	require.NoError(t, err)
	otherEveryone, err := f.ids.Next()
	require.NoError(t, err)
	otherChannel, err := f.ids.Next()
	require.NoError(t, err)

	f.exec(t, `INSERT INTO guilds (id, name, owner_id, message_audit_enabled, created_at, updated_at)
	           VALUES ($1,'other',$2,true,now(),now())`, int64(other), int64(f.owner))
	f.exec(t, `INSERT INTO roles (id, guild_id, name, permissions, position, is_default,
	                              created_at, updated_at)
	           VALUES ($1,$2,'@everyone',$3,0,true,now(),now())`,
		int64(otherEveryone), int64(other),
		(roles.PermViewChannel | roles.PermReadMessageHistory | roles.PermSendMessages).Int64())
	f.exec(t, `INSERT INTO channels (id, guild_id, name, type, position, created_at, updated_at)
	           VALUES ($1,$2,'general',0,0,now(),now())`, int64(otherChannel), int64(other))
	for _, u := range []snowflake.ID{f.owner, f.member} {
		f.exec(t, `INSERT INTO guild_members (guild_id, user_id, joined_at) VALUES ($1,$2,now())`,
			int64(other), int64(u))
	}

	// Something to leak, so an empty answer cannot be mistaken for a correct refusal.
	_, err = f.svc.Send(f.ctx, actorOf(f.owner), SendInput{
		ChannelID: otherChannel, Content: "recorded in the second guild",
	})
	require.NoError(t, err)
	require.Equal(t, 1, func() int {
		var n int
		require.NoError(t, f.pool.QueryRow(f.ctx,
			`SELECT count(*) FROM message_audit_entries WHERE guild_id = $1`, int64(other)).Scan(&n))
		return n
	}(), "the second guild must actually hold a row, or this test proves nothing")

	_, err = f.svc.GuildMessageAudit(f.ctx, actorOf(f.member), GuildAuditInput{GuildID: other})
	require.ErrorIs(t, err, httpx.ErrForbidden,
		"the bit is granted in the first guild and must confer nothing in the second — and the answer "+
			"must be a refusal, not the empty page a non-recording guild legitimately returns")

	// And it still works where it was granted, so the test above is not passing because the bit stopped
	// working everywhere.
	entries, err := f.svc.GuildMessageAudit(f.ctx, actorOf(f.member), GuildAuditInput{GuildID: f.guildID})
	require.NoError(t, err)
	require.Len(t, entries, 1)
}
