// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package messages

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/database"
	"github.com/Alexnex31/Norite/backend/internal/platform/dbtest"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
	"github.com/Alexnex31/Norite/backend/migrations"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

type fixture struct {
	svc  *Service
	pool *pgxpool.Pool
	ctx  context.Context
	ids  *snowflake.Generator

	guildID, channelID snowflake.ID
	owner, member, mod snowflake.ID
	everyoneRoleID     snowflake.ID
}

// newFixture builds a guild with an owner, an ordinary member and a moderator, by direct SQL.
//
// The guilds service would do it through real endpoints, but this package cannot import it — that is what
// the M15 chokepoint extraction was for — and the setup is a handful of inserts.
func newFixture(t *testing.T) *fixture {
	t.Helper()

	dsn := dbtest.FreshDatabase(t)
	ctx := t.Context()

	require.NoError(t, database.Migrate(ctx, database.MigrateOptions{
		DatabaseURL: dsn, Source: migrations.FS, SourceDir: ".", LockTimeout: 30 * time.Second,
	}))

	pool, err := database.New(ctx, database.PoolOptions{
		DatabaseURL: dsn, MaxConns: 4, MinConns: 1, ConnectTimeout: 10 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	ids, err := snowflake.NewGenerator(0)
	require.NoError(t, err)

	svc, err := NewService(ServiceOptions{Pool: pool, IDs: ids})
	require.NoError(t, err)

	f := &fixture{svc: svc, pool: pool, ctx: ctx, ids: ids}
	next := func() snowflake.ID {
		id, err := ids.Next()
		require.NoError(t, err)
		return id
	}

	f.owner, f.member, f.mod = next(), next(), next()
	f.guildID, f.channelID, f.everyoneRoleID = next(), next(), next()

	for _, u := range []struct {
		id   snowflake.ID
		name string
	}{{f.owner, "owner"}, {f.member, "member"}, {f.mod, "mod"}} {
		f.exec(t, `INSERT INTO users (id, username, email, display_name, created_at, updated_at)
		           VALUES ($1,$2::text,$2::text||'@example.test',$2::text,now(),now())`, int64(u.id), u.name)
	}

	f.exec(t, `INSERT INTO guilds (id, name, owner_id, created_at, updated_at)
	           VALUES ($1,'g',$2,now(),now())`, int64(f.guildID), int64(f.owner))
	f.exec(t, `INSERT INTO channels (id, guild_id, name, type, position, created_at, updated_at)
	           VALUES ($1,$2,'general',0,0,now(),now())`, int64(f.channelID), int64(f.guildID))

	// @everyone carries the default grant, which since M15 includes PermReadMessageHistory.
	everyone := roles.PermViewChannel | roles.PermReadMessageHistory | roles.PermSendMessages
	f.exec(t, `INSERT INTO roles (id, guild_id, name, permissions, position, is_default, created_at, updated_at)
	           VALUES ($1,$2,'@everyone',$3,0,true,now(),now())`,
		int64(f.everyoneRoleID), int64(f.guildID), everyone.Int64())

	for _, u := range []snowflake.ID{f.owner, f.member, f.mod} {
		f.exec(t, `INSERT INTO guild_members (guild_id, user_id, joined_at) VALUES ($1,$2,now())`,
			int64(f.guildID), int64(u))
	}

	// The moderator's own role, carrying PermManageMessages on top of the default.
	modRole := next()
	f.exec(t, `INSERT INTO roles (id, guild_id, name, permissions, position, is_default, created_at, updated_at)
	           VALUES ($1,$2,'mod',$3,1,false,now(),now())`,
		int64(modRole), int64(f.guildID), roles.PermManageMessages.Int64())
	f.exec(t, `INSERT INTO guild_member_roles (guild_id, user_id, role_id) VALUES ($1,$2,$3)`,
		int64(f.guildID), int64(f.mod), int64(modRole))

	return f
}

func (f *fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	_, err := f.pool.Exec(f.ctx, sql, args...)
	require.NoError(t, err)
}

// newChannel adds a second channel of a given type to the fixture's guild.
func (f *fixture) newChannel(t *testing.T, name string, typ int16) snowflake.ID {
	t.Helper()
	// The fixture's generator, never a fresh one: a new generator restarts its sequence at 0, so two
	// calls landing in the same millisecond mint the same id and the second insert fails on the primary
	// key. Uniqueness is a property of one generator, not of the algorithm.
	id, err := f.ids.Next()
	require.NoError(t, err)
	f.exec(t, `INSERT INTO channels (id, guild_id, name, type, position, created_at, updated_at)
	           VALUES ($1,$2,$3::text,$4,1,now(),now())`,
		int64(id), int64(f.guildID), name, typ)
	return id
}

func (f *fixture) auditCount(t *testing.T, action string) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM audit_log_entries WHERE guild_id = $1 AND action = $2`,
		int64(f.guildID), action).Scan(&n))
	return n
}

func actorOf(id snowflake.ID) auth.Actor {
	return auth.Actor{Kind: auth.ActorUser, UserID: id}
}

func (f *fixture) send(t *testing.T, author snowflake.ID, content string) Message {
	t.Helper()
	m, err := f.svc.Send(f.ctx, actorOf(author), SendInput{ChannelID: f.channelID, Content: content})
	require.NoError(t, err)
	return m
}

// TestSendingStoresTheMessageAndAdvancesTheChannelPointer covers the happy path and the denormalized
// pointer 000015 created with no foreign key, which nothing maintained until this milestone.
func TestSendingStoresTheMessageAndAdvancesTheChannelPointer(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	msg := f.send(t, f.member, "hello")

	require.Equal(t, "hello", msg.Content)
	require.NotNil(t, msg.AuthorID)
	require.Equal(t, f.member, *msg.AuthorID)

	var last *int64
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT last_message_id FROM channels WHERE id = $1`, int64(f.channelID)).Scan(&last))
	require.NotNil(t, last, "the send must advance channels.last_message_id in its own transaction")
	require.Equal(t, int64(msg.ID), *last)
}

// TestAnEditWritesExactlyOneHistoryRowCarryingThePreviousContent is the property M16a will read.
//
// The history row holds what the message said *before* the edit, not after — getting that backwards makes
// the whole table useless in a way no other test would catch, since both versions are real strings.
func TestAnEditWritesExactlyOneHistoryRowCarryingThePreviousContent(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	msg := f.send(t, f.member, "first")

	updated, err := f.svc.Update(f.ctx, actorOf(f.member), UpdateInput{
		ChannelID: f.channelID, MessageID: msg.ID, Content: "second",
	})
	require.NoError(t, err)
	require.Equal(t, "second", updated.Content)
	require.NotNil(t, updated.EditedAt)

	rows, err := f.pool.Query(f.ctx,
		`SELECT content FROM message_edit_history WHERE message_id = $1 ORDER BY edited_at`, int64(msg.ID))
	require.NoError(t, err)
	defer rows.Close()

	var history []string
	for rows.Next() {
		var c string
		require.NoError(t, rows.Scan(&c))
		history = append(history, c)
	}
	require.NoError(t, rows.Err())

	require.Equal(t, []string{"first"}, history,
		"the history row must carry the content before the edit, and there must be exactly one")
}

// TestOnlyTheAuthorMayEditEvenWithManageMessages pins the deliberate departure.
//
// PermManageMessages lets a moderator delete somebody else's message; it must never let them rewrite it.
// An audit entry recording that it happened would not make the message honest.
func TestOnlyTheAuthorMayEditEvenWithManageMessages(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	msg := f.send(t, f.member, "mine")

	_, err := f.svc.Update(f.ctx, actorOf(f.mod), UpdateInput{
		ChannelID: f.channelID, MessageID: msg.ID, Content: "words in your mouth",
	})
	require.ErrorIs(t, err, httpx.ErrForbidden,
		"a moderator must not be able to rewrite somebody else's message")

	var content string
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT content FROM messages WHERE id = $1`, int64(msg.ID)).Scan(&content))
	require.Equal(t, "mine", content, "the refusal must also not have written anything")
}

// TestAnAuthorDeletingTheirOwnMessageWritesNoAuditEntry is the property rule 2's narrowing created, and it
// has no other home.
//
// Before M15 the rule was "every guild-scoped mutation writes an entry", and a test asserting that a
// mutation writes *nothing* would have been a bug report. It is the design now: deleting your own message
// exercises authority over nobody, and auditing it would put the product's highest-volume write on the
// table the audit-log reader pages through.
func TestAnAuthorDeletingTheirOwnMessageWritesNoAuditEntry(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	msg := f.send(t, f.member, "mine to remove")

	require.NoError(t, f.svc.Delete(f.ctx, actorOf(f.member), f.channelID, msg.ID))
	require.Equal(t, 0, f.auditCount(t, ActionMessageDelete),
		"an author deleting their own message is not administrative and must write no audit entry")

	// Soft, so M16 can still carry a report against the row.
	var deletedAt *time.Time
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT deleted_at FROM messages WHERE id = $1`, int64(msg.ID)).Scan(&deletedAt))
	require.NotNil(t, deletedAt, "the delete must be soft")
}

// TestAModeratorDeletingSomebodyElsesMessageIsAudited is the other half, and the one the route-surface
// test's exemption promises is covered here.
//
// It also pins rule 13's answer: the entry carries ids and never content, so there is no message text in
// the audit log to have to exclude E2E DMs from.
func TestAModeratorDeletingSomebodyElsesMessageIsAudited(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	msg := f.send(t, f.member, "something a moderator removes")

	require.NoError(t, f.svc.Delete(f.ctx, actorOf(f.mod), f.channelID, msg.ID))
	require.Equal(t, 1, f.auditCount(t, ActionMessageDelete),
		"a moderator acting on somebody else's message is administrative and must be audited")

	var actor int64
	var changes []byte
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT actor_id, changes FROM audit_log_entries WHERE guild_id = $1 AND action = $2`,
		int64(f.guildID), ActionMessageDelete).Scan(&actor, &changes))

	require.Equal(t, int64(f.mod), actor, "the entry must name who did it")
	require.NotContains(t, string(changes), "something a moderator removes",
		"the audit payload must carry ids, never message content — rule 13 applies to this table the "+
			"moment it holds content, and the cheapest way to satisfy it is to store none")
}

// TestAMemberWithoutManageMessagesCannotDeleteAnothersMessage is the refusal the case above depends on.
func TestAMemberWithoutManageMessagesCannotDeleteAnothersMessage(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	msg := f.send(t, f.member, "not yours")

	err := f.svc.Delete(f.ctx, actorOf(f.owner), f.channelID, msg.ID)
	require.NoError(t, err, "the owner holds every permission and may delete it")

	other := f.send(t, f.owner, "owner's own")
	require.ErrorIs(t,
		f.svc.Delete(f.ctx, actorOf(f.member), f.channelID, other.ID), httpx.ErrForbidden,
		"an ordinary member must not delete somebody else's message")
}

// TestReadingTheBacklogNeedsPermReadMessageHistory pins the bit M15 added.
//
// Seeing a channel and reading what was said in it before you arrived are separate grants, as they are in
// Discord. A member holding view and send but not history sees the channel and cannot read it.
func TestReadingTheBacklogNeedsPermReadMessageHistory(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.send(t, f.owner, "history")

	msgs, err := f.svc.List(f.ctx, actorOf(f.member), ListInput{ChannelID: f.channelID})
	require.NoError(t, err)
	require.Len(t, msgs, 1, "the default grant includes PermReadMessageHistory")

	// Drop the bit from @everyone and the same member loses the backlog while keeping the channel.
	without := roles.PermViewChannel | roles.PermSendMessages
	f.exec(t, `UPDATE roles SET permissions = $2 WHERE id = $1`,
		int64(f.everyoneRoleID), without.Int64())

	_, err = f.svc.List(f.ctx, actorOf(f.member), ListInput{ChannelID: f.channelID})
	require.ErrorIs(t, err, httpx.ErrForbidden,
		"without PermReadMessageHistory the backlog is refused, though the channel is still visible")

	// Still able to post, which is what makes the two bits genuinely separate rather than nested.
	_, err = f.svc.Send(f.ctx, actorOf(f.member), SendInput{ChannelID: f.channelID, Content: "still here"})
	require.NoError(t, err, "PermSendMessages is unaffected by losing history")
}

// TestAReplyMustNameAMessageInTheSameChannel is the disclosure guard, not a data-integrity one.
func TestAReplyMustNameAMessageInTheSameChannel(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	secondChannel := f.channelID + 1000
	f.exec(t, `INSERT INTO channels (id, guild_id, name, type, position, created_at, updated_at)
	           VALUES ($1,$2,'other',0,1,now(),now())`, int64(secondChannel), int64(f.guildID))

	away, err := f.svc.Send(f.ctx, actorOf(f.owner), SendInput{ChannelID: secondChannel, Content: "far"})
	require.NoError(t, err)

	_, err = f.svc.Send(f.ctx, actorOf(f.member), SendInput{
		ChannelID: f.channelID, Content: "reply", ReplyToID: &away.ID,
	})
	require.ErrorIs(t, err, httpx.ErrBadRequest,
		"a reply must not reach across channels; the refusal must not confirm the target exists")
}

// TestAMessageFromAnotherChannelIsNotReachableThroughThisOne is the same guard on the mutation paths.
//
// The channel in the path is what was authorized, so a message id from elsewhere must answer 404 — the
// permission check would otherwise cover one channel while the write landed in another.
func TestAMessageFromAnotherChannelIsNotReachableThroughThisOne(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	secondChannel := f.channelID + 2000
	f.exec(t, `INSERT INTO channels (id, guild_id, name, type, position, created_at, updated_at)
	           VALUES ($1,$2,'other',0,1,now(),now())`, int64(secondChannel), int64(f.guildID))

	away, err := f.svc.Send(f.ctx, actorOf(f.member), SendInput{ChannelID: secondChannel, Content: "far"})
	require.NoError(t, err)

	_, err = f.svc.Update(f.ctx, actorOf(f.member), UpdateInput{
		ChannelID: f.channelID, MessageID: away.ID, Content: "edited from the wrong channel",
	})
	require.ErrorIs(t, err, httpx.ErrNotFound)

	require.ErrorIs(t,
		f.svc.Delete(f.ctx, actorOf(f.member), f.channelID, away.ID), httpx.ErrNotFound)
}

// TestTheListingExcludesDeletedMessagesAndPagesNewestFirst covers the read shape.
func TestTheListingExcludesDeletedMessagesAndPagesNewestFirst(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	first := f.send(t, f.member, "one")
	second := f.send(t, f.member, "two")
	third := f.send(t, f.member, "three")

	require.NoError(t, f.svc.Delete(f.ctx, actorOf(f.member), f.channelID, second.ID))

	msgs, err := f.svc.List(f.ctx, actorOf(f.member), ListInput{ChannelID: f.channelID})
	require.NoError(t, err)
	require.Len(t, msgs, 2, "a soft-deleted message must not appear in the backlog")
	require.Equal(t, third.ID, msgs[0].ID, "newest first")
	require.Equal(t, first.ID, msgs[1].ID)

	// The cursor is exclusive and travels as an id, so paging cannot repeat the boundary row.
	page, err := f.svc.List(f.ctx, actorOf(f.member), ListInput{ChannelID: f.channelID, Before: &third.ID})
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, first.ID, page[0].ID)
}

// TestAMutedMemberCannotEditAroundTheMute is the security audit's first finding, pinned.
//
// A member-tier overwrite denying PermSendMessages is the mute every guild uses. Before the fix, Update
// authorized on the view bit alone, so the mute stopped new posts and left the author free to rewrite
// every message they had already sent — one fresh publish per rewrite, and a MESSAGE_UPDATE fan-out each
// once M18 lands. Deleting stays allowed on purpose: that is redaction, which is the outcome a mute wants.
func TestAMutedMemberCannotEditAroundTheMute(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	msg := f.send(t, f.member, "posted before the mute")

	f.exec(t, `INSERT INTO permission_overwrites (channel_id, target_id, target_type, allow, deny)
	           VALUES ($1,$2,$3,0,$4)`,
		int64(f.channelID), int64(f.member), roles.OverwriteTargetMember,
		roles.PermSendMessages.Int64())

	_, err := f.svc.Send(f.ctx, actorOf(f.member), SendInput{
		ChannelID: f.channelID, Content: "a new post",
	})
	require.ErrorIs(t, err, httpx.ErrForbidden, "the mute must stop a send")

	_, err = f.svc.Update(f.ctx, actorOf(f.member), UpdateInput{
		ChannelID: f.channelID, MessageID: msg.ID, Content: "rewritten after the mute",
	})
	require.ErrorIs(t, err, httpx.ErrForbidden,
		"an edit is a write into the channel, so the mute must stop it too")

	var stored string
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT content FROM messages WHERE id = $1`, int64(msg.ID)).Scan(&stored))
	require.Equal(t, "posted before the mute", stored, "the refused edit must not have landed")

	// Redaction survives the mute, which is the half that must keep working.
	require.NoError(t, f.svc.Delete(f.ctx, actorOf(f.member), f.channelID, msg.ID),
		"a muted member may still remove their own message")
}

// TestOnlyATextChannelAcceptsAMessage is the audit's second finding.
//
// guildauth refuses a channel belonging to no guild and says nothing about the rest of the vocabulary, so
// before ChannelGuildText a member could post into a category — somewhere no client renders, which makes
// it invisible to moderation while the API keeps serving it. Reading and deleting stay open on every type
// so that anything already stored remains reachable and removable.
func TestOnlyATextChannelAcceptsAMessage(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	for _, tc := range []struct {
		name string
		typ  int16
	}{
		{"GUILD_VOICE", 2},
		{"GUILD_CATEGORY", 4},
		{"GUILD_ANNOUNCEMENT", 5},
		{"GUILD_STAGE_VOICE", 6},
	} {
		id := f.newChannel(t, tc.name, tc.typ)

		_, err := f.svc.Send(f.ctx, actorOf(f.member), SendInput{
			ChannelID: id, Content: "does this land?",
		})
		require.ErrorIs(t, err, httpx.ErrBadRequest, "%s must not accept a message", tc.name)

		var n int
		require.NoError(t, f.pool.QueryRow(f.ctx,
			`SELECT count(*) FROM messages WHERE channel_id = $1`, int64(id)).Scan(&n))
		require.Zero(t, n, "%s must hold no rows", tc.name)

		var last *int64
		require.NoError(t, f.pool.QueryRow(f.ctx,
			`SELECT last_message_id FROM channels WHERE id = $1`, int64(id)).Scan(&last))
		require.Nil(t, last, "%s must not have advanced its unread pointer", tc.name)
	}

	// The one type that does, so the test fails if the check is inverted rather than merely present.
	_, err := f.svc.Send(f.ctx, actorOf(f.member), SendInput{
		ChannelID: f.channelID, Content: "a text channel still works",
	})
	require.NoError(t, err)
}

// TestTheChannelPointerNeverMovesBackwards is what replaced the channel row lock.
//
// Send stopped taking the channel FOR UPDATE at M15's optimization pass, because it serialized every send
// in a channel behind every other — about 5x on the product's hottest write. The lock was silently
// providing one thing: ordering for this pointer. With a plain assignment, a send that mints a lower id
// but commits *later* overwrites a higher one, and the channel's unread marker points at an older message
// than the newest. Reproduced deterministically in psql with two interleaved transactions — the pointer
// ended at 100 with message 101 present — which is why GREATEST is in the statement.
//
// **Asserted on the statement, not by racing goroutines.** The first version of this test ran eight
// concurrent senders and passed with GREATEST removed, three times out of three: the interleaving it
// needed is real but too rare to provoke by load, so the test looked like a guard and was not one. This
// version drives the out-of-order case directly, and fails the moment GREATEST goes.
func TestTheChannelPointerNeverMovesBackwards(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	newer := f.send(t, f.member, "the newest message")

	pointer := func() int64 {
		var v *int64
		require.NoError(t, f.pool.QueryRow(f.ctx,
			`SELECT last_message_id FROM channels WHERE id = $1`, int64(f.channelID)).Scan(&v))
		require.NotNil(t, v, "the send must have set the pointer")
		return *v
	}
	require.Equal(t, int64(newer.ID), pointer())

	// A send that minted an older id and is only now committing its pointer update — the transaction that
	// lost the race to commit but holds the lower value.
	require.NoError(t, db.New(f.pool).SetChannelLastMessage(f.ctx, db.SetChannelLastMessageParams{
		ID: int64(f.channelID), LastMessageID: ptr(int64(newer.ID) - 1),
	}))
	require.Equal(t, int64(newer.ID), pointer(),
		"a later-committing send with an older id must not walk the pointer backwards")

	// And a genuinely newer one still advances it, so the test fails on a statement that never writes.
	require.NoError(t, db.New(f.pool).SetChannelLastMessage(f.ctx, db.SetChannelLastMessageParams{
		ID: int64(f.channelID), LastMessageID: ptr(int64(newer.ID) + 1),
	}))
	require.Equal(t, int64(newer.ID)+1, pointer(), "a newer id must still advance the pointer")
}

func ptr(v int64) *int64 { return &v }

// TestSendingIntoADeletedChannelIs404 covers the ordinary case: the channel was already gone when the
// request arrived, so the authorization read finds nothing.
//
// Named for what it actually asserts. Its first version claimed to cover the *race* — a channel deleted
// between the authorization read and the insert — and did not: it passed with the foreign-key mapping
// disabled, because this path never reaches the insert at all. The race is covered by
// TestChannelVanishedMapsOnlyTheForeignKeyThatCanHappen below.
func TestSendingIntoADeletedChannelIs404(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	id := f.newChannel(t, "doomed", ChannelGuildText)
	_, err := f.svc.Send(f.ctx, actorOf(f.member), SendInput{ChannelID: id, Content: "before"})
	require.NoError(t, err)

	f.exec(t, `DELETE FROM channels WHERE id = $1`, int64(id))

	_, err = f.svc.Send(f.ctx, actorOf(f.member), SendInput{ChannelID: id, Content: "after"})
	require.ErrorIs(t, err, httpx.ErrNotFound)
}

// TestChannelVanishedMapsOnlyTheForeignKeyThatCanHappen covers the race the unlocked authorize opened.
//
// The interleaving itself cannot be driven from a test — the delete has to commit inside the service's
// own transaction — so the mapping is tested on the error Postgres actually produces. The negative cases
// are the point: a mapping that swallowed every foreign-key error, or every pg error, would turn a real
// bug into a 404 and hide it.
func TestChannelVanishedMapsOnlyTheForeignKeyThatCanHappen(t *testing.T) {
	t.Parallel()

	require.ErrorIs(t, channelVanished(&pgconn.PgError{
		Code: pgerrcode.ForeignKeyViolation, ConstraintName: "messages_channel_id_fkey",
	}), httpx.ErrNotFound, "the channel disappearing mid-send must read as 404")

	require.Nil(t, channelVanished(&pgconn.PgError{
		Code: pgerrcode.ForeignKeyViolation, ConstraintName: "messages_author_id_fkey",
	}), "a different foreign key is a real bug and must not be reported as a missing channel")

	require.Nil(t, channelVanished(&pgconn.PgError{
		Code: pgerrcode.UniqueViolation, ConstraintName: "messages_pkey",
	}), "a duplicate id is a generator fault, not a missing channel")

	require.Nil(t, channelVanished(errors.New("connection reset")),
		"a non-pg error must fall through")
}
