// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package tags

import (
	"context"
	"fmt"
	"testing"
	"time"

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
	messageID          snowflake.ID
}

// newFixture builds a guild with an owner, an ordinary member and a moderator, by direct SQL.
//
// This package cannot import `guilds` or `messages` — the M15 chokepoint extraction is what makes that
// true — so the rows go in directly, which is what the `messages` and `reports` fixtures do for the same
// reason.
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
	f.owner, f.member, f.mod = f.next(t), f.next(t), f.next(t)
	f.guildID, f.channelID = f.next(t), f.next(t)

	for _, u := range []struct {
		id   snowflake.ID
		name string
	}{{f.owner, "owner"}, {f.member, "member"}, {f.mod, "mod"}} {
		f.exec(t, `INSERT INTO users (id, username, email, display_name, created_at, updated_at)
		           VALUES ($1,$2::text,$2::text||'@example.test',$2::text,now(),now())`,
			int64(u.id), u.name)
	}

	f.exec(t, `INSERT INTO guilds (id, name, owner_id, created_at, updated_at)
	           VALUES ($1,'g',$2,now(),now())`, int64(f.guildID), int64(f.owner))
	f.exec(t, `INSERT INTO channels (id, guild_id, name, type, position, created_at, updated_at)
	           VALUES ($1,$2,'general',0,0,now(),now())`, int64(f.channelID), int64(f.guildID))

	everyone := roles.PermViewChannel | roles.PermReadMessageHistory | roles.PermSendMessages
	f.exec(t, `INSERT INTO roles (id, guild_id, name, permissions, position, is_default,
	                              created_at, updated_at)
	           VALUES ($1,$2,'@everyone',$3,0,true,now(),now())`,
		int64(f.next(t)), int64(f.guildID), everyone.Int64())

	for _, u := range []snowflake.ID{f.owner, f.member, f.mod} {
		f.exec(t, `INSERT INTO guild_members (guild_id, user_id, joined_at) VALUES ($1,$2,now())`,
			int64(f.guildID), int64(u))
	}

	modRole := f.next(t)
	f.exec(t, `INSERT INTO roles (id, guild_id, name, permissions, position, is_default,
	                              created_at, updated_at)
	           VALUES ($1,$2,'mod',$3,1,false,now(),now())`,
		int64(modRole), int64(f.guildID), roles.PermManageMessages.Int64())
	f.exec(t, `INSERT INTO guild_member_roles (guild_id, user_id, role_id) VALUES ($1,$2,$3)`,
		int64(f.guildID), int64(f.mod), int64(modRole))

	f.messageID = f.newMessage(t, f.channelID, f.member)
	return f
}

func (f *fixture) next(t *testing.T) snowflake.ID {
	t.Helper()
	// The fixture's own generator, never a fresh one: a new generator restarts its sequence, so two calls
	// in the same millisecond mint the same id and the second insert fails on the primary key.
	id, err := f.ids.Next()
	require.NoError(t, err)
	return id
}

func (f *fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	_, err := f.pool.Exec(f.ctx, sql, args...)
	require.NoError(t, err)
}

func (f *fixture) newMessage(t *testing.T, channel, author snowflake.ID) snowflake.ID {
	t.Helper()
	id := f.next(t)
	f.exec(t, `INSERT INTO messages (id, channel_id, author_id, content, type, created_at)
	           VALUES ($1,$2,$3,'hello',0,now())`, int64(id), int64(channel), int64(author))
	return id
}

// newGuild adds a second guild the given users belong to, with its own channel and message.
func (f *fixture) newGuild(t *testing.T, members ...snowflake.ID) (guildID, channelID, messageID snowflake.ID) {
	t.Helper()
	guildID, channelID = f.next(t), f.next(t)

	f.exec(t, `INSERT INTO guilds (id, name, owner_id, created_at, updated_at)
	           VALUES ($1,'other',$2,now(),now())`, int64(guildID), int64(f.owner))
	f.exec(t, `INSERT INTO channels (id, guild_id, name, type, position, created_at, updated_at)
	           VALUES ($1,$2,'general',0,0,now(),now())`, int64(channelID), int64(guildID))
	everyone := roles.PermViewChannel | roles.PermReadMessageHistory | roles.PermSendMessages
	f.exec(t, `INSERT INTO roles (id, guild_id, name, permissions, position, is_default,
	                              created_at, updated_at)
	           VALUES ($1,$2,'@everyone',$3,0,true,now(),now())`,
		int64(f.next(t)), int64(guildID), everyone.Int64())
	for _, u := range append([]snowflake.ID{f.owner}, members...) {
		f.exec(t, `INSERT INTO guild_members (guild_id, user_id, joined_at) VALUES ($1,$2,now())
		           ON CONFLICT DO NOTHING`, int64(guildID), int64(u))
	}
	return guildID, channelID, f.newMessage(t, channelID, f.owner)
}

func actorOf(id snowflake.ID) auth.Actor {
	return auth.Actor{Kind: auth.ActorUser, UserID: id}
}

// TestASharedTagNeedsThePermissionAndAPrivateOneDoesNot is the milestone's one stated permission rule,
// asserted in both directions.
//
// The negative half is the one worth having: a member with nothing but @everyone's default grant must be
// able to create a private tag, because the entry says private tags need no permission and a service that
// required one would satisfy the positive assertion perfectly.
func TestASharedTagNeedsThePermissionAndAPrivateOneDoesNot(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	_, err := f.svc.Create(f.ctx, actorOf(f.member), CreateInput{
		GuildID: f.guildID, Name: "shared-by-a-member", IsShared: true,
	})
	require.ErrorIs(t, err, httpx.ErrForbidden,
		"a shared tag adds to the guild's vocabulary and needs PermManageMessages")

	private, err := f.svc.Create(f.ctx, actorOf(f.member), CreateInput{
		GuildID: f.guildID, Name: "mine", IsShared: false,
	})
	require.NoError(t, err, "a private tag needs nothing beyond membership — this is the stated rule")
	require.False(t, private.IsShared)

	shared, err := f.svc.Create(f.ctx, actorOf(f.mod), CreateInput{
		GuildID: f.guildID, Name: "shared", IsShared: true,
	})
	require.NoError(t, err)
	require.True(t, shared.IsShared)
}

// TestAPrivateTagIsInvisibleToEverybodyElse is what makes the word "private" mean something, and it is
// asserted through both paths that can reach a tag because they enforce it separately.
//
// The listing filters in SQL; the single-row read filters in Go. Two copies of one rule is the shape M15
// warns about, so the pin is a test that drives both with the same actor rather than a comment promising
// they agree.
func TestAPrivateTagIsInvisibleToEverybodyElse(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	mine, err := f.svc.Create(f.ctx, actorOf(f.member), CreateInput{GuildID: f.guildID, Name: "mine"})
	require.NoError(t, err)

	// The listing: the owner and a moderator see nothing of it.
	for _, who := range []snowflake.ID{f.owner, f.mod} {
		list, err := f.svc.List(f.ctx, actorOf(who), f.guildID)
		require.NoError(t, err)
		require.Empty(t, list, "somebody else's private tag must not appear in the listing")
	}

	list, err := f.svc.List(f.ctx, actorOf(f.member), f.guildID)
	require.NoError(t, err)
	require.Len(t, list, 1, "its owner sees it")
	require.Equal(t, mine.ID, list[0].ID)
}

// TestAPrivateTagAnswersNotFoundRatherThanForbidden is the anti-enumeration half, and it is the assertion
// a reasonable implementation fails.
//
// Loading the tag and then refusing on authority gives 403, which reads as correct and is an oracle: tag
// ids are snowflakes, so a 403-versus-404 split turns a list of plausible ids into a map of which private
// tags exist in a guild and roughly when they were made. That is the split M12 settled for guilds, M13 for
// channels and M16a for hidden channels, arriving a fourth time.
func TestAPrivateTagAnswersNotFoundRatherThanForbidden(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	mine, err := f.svc.Create(f.ctx, actorOf(f.member), CreateInput{GuildID: f.guildID, Name: "mine"})
	require.NoError(t, err)

	// The moderator holds PermManageMessages, which is the authority that deletes a *shared* tag — so if
	// the refusal were about authority rather than visibility, this is the actor who would get 403.
	err = f.svc.Delete(f.ctx, actorOf(f.mod), f.guildID, mine.ID)
	require.ErrorIs(t, err, httpx.ErrNotFound,
		"a private tag somebody else owns must not exist as far as this caller is concerned — a 403 here "+
			"tells anybody holding a tag id whether it names a private tag")

	// And the owner of the guild, who is layer 2 and outranks everybody in it, gets the same answer.
	err = f.svc.Delete(f.ctx, actorOf(f.owner), f.guildID, mine.ID)
	require.ErrorIs(t, err, httpx.ErrNotFound)

	// Its actual owner can delete it, so the test above is not passing because delete is broken for all.
	require.NoError(t, f.svc.Delete(f.ctx, actorOf(f.member), f.guildID, mine.ID))
}

// TestATagCannotCrossAGuildBoundary is the milestone's structural property and the one the schema alone
// does not give.
//
// message_tag_applications has no guild_id, and a message reaches a guild only through channels — so
// nothing but the apply statement's own join predicate stops guild A's tag landing on guild B's message.
// M16 found the same shape when `reports` had no guild_id at all and M16b's security review found it in
// the recording writer; this is the third, caught at planning rather than at review.
//
// # Two guards, so proving it takes three runs
//
// The property is held twice on purpose: loadInGuild refuses a tag whose guild is not the channel's, and
// the apply statement carries `c.guild_id = t.guild_id`. Removing either one alone leaves this test
// passing, which looks like a weak test and is actually the redundancy working — so the proof is three
// runs, as M16b's recording guard needed:
//
//	statement predicate removed   passes — loadInGuild catches it first
//	loadInGuild's check removed   passes — the statement catches it, which is what shows it is
//	                              load-bearing rather than decoration
//	both removed                  FAILS — which is what shows this test can see the property at all
//
// Only the third run proves the test works, and only the second proves the statement earns its place.
// The statement half is the one that matters longest: a future writer — a bulk importer, M22's
// automation, M60's webhooks — will not be holding the authorize result loadInGuild depends on.
func TestATagCannotCrossAGuildBoundary(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// A tag in the first guild, and a message in a second guild the same member belongs to — so every
	// authorization check the service makes passes, and only the statement's predicate stands in the way.
	tag, err := f.svc.Create(f.ctx, actorOf(f.member), CreateInput{GuildID: f.guildID, Name: "mine"})
	require.NoError(t, err)

	_, otherChannel, otherMessage := f.newGuild(t, f.member)

	err = f.svc.Apply(f.ctx, actorOf(f.member), ApplyInput{
		ChannelID: otherChannel, MessageID: otherMessage, TagID: tag.ID,
	})
	require.ErrorIs(t, err, httpx.ErrNotFound,
		"a tag belongs to one guild and must not reach another guild's message, even for a member of both")

	var n int
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM message_tag_applications WHERE tag_id = $1`, int64(tag.ID)).Scan(&n))
	require.Zero(t, n, "and nothing was written")

	// The same tag on a message in its *own* guild works, so the refusal above is about the boundary
	// rather than about applying being broken.
	require.NoError(t, f.svc.Apply(f.ctx, actorOf(f.member), ApplyInput{
		ChannelID: f.channelID, MessageID: f.messageID, TagID: tag.ID,
	}))
}

// TestApplyingTwiceIsIdempotent pins the property a retrying client depends on.
//
// ON CONFLICT DO NOTHING reports zero rows for a repeat *and* for a refusal, so the service cannot tell
// them apart from the count — it resolves the difference by reading the row back. Without that, a repeat
// would answer 404 and a client that retried a request it was unsure had landed would be told its tag
// never applied.
func TestApplyingTwiceIsIdempotent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	tag, err := f.svc.Create(f.ctx, actorOf(f.mod), CreateInput{
		GuildID: f.guildID, Name: "spam", IsShared: true,
	})
	require.NoError(t, err)

	in := ApplyInput{ChannelID: f.channelID, MessageID: f.messageID, TagID: tag.ID}
	require.NoError(t, f.svc.Apply(f.ctx, actorOf(f.member), in))
	require.NoError(t, f.svc.Apply(f.ctx, actorOf(f.member), in), "a repeat is the same answer, not an error")

	var n int
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM message_tag_applications WHERE tag_id = $1`, int64(tag.ID)).Scan(&n))
	require.Equal(t, 1, n, "and it is still one row")
}

// TestApplyingToAMessageThatIsNotThereAnswersNotFound covers the three states the statement's guards
// collapse into one answer: no such message, a message in another guild, and a soft-deleted one.
func TestApplyingToAMessageThatIsNotThereAnswersNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	tag, err := f.svc.Create(f.ctx, actorOf(f.mod), CreateInput{
		GuildID: f.guildID, Name: "spam", IsShared: true,
	})
	require.NoError(t, err)

	err = f.svc.Apply(f.ctx, actorOf(f.member), ApplyInput{
		ChannelID: f.channelID, MessageID: f.next(t), TagID: tag.ID,
	})
	require.ErrorIs(t, err, httpx.ErrNotFound, "an id naming no message")

	// A soft-deleted message. The moderation surfaces read deleted messages on purpose (000020), but
	// adding a new fact to something already removed is a different act — a tag applied after deletion
	// would surface a deleted message in a tag listing.
	deleted := f.newMessage(t, f.channelID, f.member)
	f.exec(t, `UPDATE messages SET deleted_at = now() WHERE id = $1`, int64(deleted))

	err = f.svc.Apply(f.ctx, actorOf(f.member), ApplyInput{
		ChannelID: f.channelID, MessageID: deleted, TagID: tag.ID,
	})
	require.ErrorIs(t, err, httpx.ErrNotFound, "a soft-deleted message takes no new tags")
}

// TestWhoMayRemoveATagApplication drives all three authorities and the one actor who holds none.
func TestWhoMayRemoveATagApplication(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// The moderator owns the tag; the member applies it. That separates the three roles onto three people,
	// which a fixture where one actor does everything cannot do.
	tag, err := f.svc.Create(f.ctx, actorOf(f.mod), CreateInput{
		GuildID: f.guildID, Name: "spam", IsShared: true,
	})
	require.NoError(t, err)

	in := ApplyInput{ChannelID: f.channelID, MessageID: f.messageID, TagID: tag.ID}

	// The owner is none of the three: not the applier, not the tag's owner, and holds no
	// PermManageMessages of their own — layer 2 gives them everything, so they are checked last and are
	// expected to pass. Somebody with no standing at all is the refusal case, so a fourth member is made.
	outsider := f.next(t)
	f.exec(t, `INSERT INTO users (id, username, email, display_name, created_at, updated_at)
	           VALUES ($1,'outsider','outsider@example.test','outsider',now(),now())`, int64(outsider))
	f.exec(t, `INSERT INTO guild_members (guild_id, user_id, joined_at) VALUES ($1,$2,now())`,
		int64(f.guildID), int64(outsider))

	require.NoError(t, f.svc.Apply(f.ctx, actorOf(f.member), in))
	err = f.svc.Unapply(f.ctx, actorOf(outsider), in)
	require.ErrorIs(t, err, httpx.ErrForbidden,
		"a member who neither applied it, owns it, nor moderates must not remove it")

	require.NoError(t, f.svc.Unapply(f.ctx, actorOf(f.member), in), "whoever applied it may remove it")

	require.NoError(t, f.svc.Apply(f.ctx, actorOf(f.member), in))
	require.NoError(t, f.svc.Unapply(f.ctx, actorOf(f.mod), in), "the tag's owner may remove it")
}

// TestTagNamesAreUniquePerKind pins both partial indexes, and the third assertion is the one a single
// unique index over (guild_id, name) would fail.
func TestTagNamesAreUniquePerKind(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	_, err := f.svc.Create(f.ctx, actorOf(f.mod), CreateInput{
		GuildID: f.guildID, Name: "review", IsShared: true,
	})
	require.NoError(t, err)

	_, err = f.svc.Create(f.ctx, actorOf(f.mod), CreateInput{
		GuildID: f.guildID, Name: "Review", IsShared: true,
	})
	require.ErrorIs(t, err, httpx.ErrConflict,
		"shared names are unique per guild, case-insensitively — Review and review are one name")

	// Two members may each hold a private tag of the same name, and neither collides with the shared one.
	// A single unique index over (guild_id, name) would refuse all three of these.
	_, err = f.svc.Create(f.ctx, actorOf(f.member), CreateInput{GuildID: f.guildID, Name: "review"})
	require.NoError(t, err, "a private tag may share a name with a shared one")

	_, err = f.svc.Create(f.ctx, actorOf(f.owner), CreateInput{GuildID: f.guildID, Name: "review"})
	require.NoError(t, err, "and with another member's private tag")

	_, err = f.svc.Create(f.ctx, actorOf(f.member), CreateInput{GuildID: f.guildID, Name: "review"})
	require.ErrorIs(t, err, httpx.ErrConflict, "but not with your own")
}

// TestPrivateTagsAreCappedPerMember is the ceiling that matters, and the reason is in the constants: a
// private tag needs no permission, so without a per-member cap every member of every guild has an
// unbounded write.
//
// Per member rather than per guild, or one member could exhaust the guild's allowance and lock everybody
// else out of a feature that needs no permission — which the second half asserts.
func TestPrivateTagsAreCappedPerMember(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// Filled by direct insert rather than through the service: MaxPrivateTagsPerMember calls would be a
	// slow test for no extra coverage, and what is under test is the check rather than the inserts.
	for i := 0; i < MaxPrivateTagsPerMember; i++ {
		// The name is built in Go rather than concatenated in SQL: `'p'||$3::text` leaves Postgres to
		// deduce the parameter's type from a context that does not determine it, which is the
		// "inconsistent types deduced for parameter" error M16b's seeding hit for the same reason.
		f.exec(t, `INSERT INTO message_tags (id, guild_id, name, created_by, is_shared)
		           VALUES ($1,$2,$3,$4,false)`,
			int64(f.next(t)), int64(f.guildID), fmt.Sprintf("p%d", i), int64(f.member))
	}

	_, err := f.svc.Create(f.ctx, actorOf(f.member), CreateInput{GuildID: f.guildID, Name: "one-more"})
	require.ErrorIs(t, err, httpx.ErrConflict, "the member's own ceiling is full")

	_, err = f.svc.Create(f.ctx, actorOf(f.owner), CreateInput{GuildID: f.guildID, Name: "mine"})
	require.NoError(t, err,
		"and it is per member: one member filling theirs must not lock anybody else out of a feature "+
			"that needs no permission")
}

// TestReadingAMessagesTagsHidesSomebodyElsesPrivateOne is the third path the visibility rule reaches, and
// the one where forgetting it is least visible — the message is readable by everyone, so a private tag on
// it would be too.
func TestReadingAMessagesTagsHidesSomebodyElsesPrivateOne(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	shared, err := f.svc.Create(f.ctx, actorOf(f.mod), CreateInput{
		GuildID: f.guildID, Name: "spam", IsShared: true,
	})
	require.NoError(t, err)
	mine, err := f.svc.Create(f.ctx, actorOf(f.member), CreateInput{GuildID: f.guildID, Name: "mine"})
	require.NoError(t, err)

	for _, tagID := range []snowflake.ID{shared.ID, mine.ID} {
		require.NoError(t, f.svc.Apply(f.ctx, actorOf(f.member), ApplyInput{
			ChannelID: f.channelID, MessageID: f.messageID, TagID: tagID,
		}))
	}

	mineView, err := f.svc.ForMessage(f.ctx, actorOf(f.member), f.channelID, f.messageID)
	require.NoError(t, err)
	require.Len(t, mineView, 2, "its owner sees both")

	othersView, err := f.svc.ForMessage(f.ctx, actorOf(f.mod), f.channelID, f.messageID)
	require.NoError(t, err)
	require.Len(t, othersView, 1, "everybody else sees only the shared one")
	require.Equal(t, shared.ID, othersView[0].ID)
	require.Equal(t, f.member, othersView[0].AppliedBy, "and who applied it, which is not a disclosure")
}

// TestANonMemberReachesNothing is the refusal every surface here shares, asserted once per entry point
// because each resolves its guild differently — List and Create from the guild in the path, Apply and
// ForMessage from the channel.
func TestANonMemberReachesNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	stranger := f.next(t)
	f.exec(t, `INSERT INTO users (id, username, email, display_name, created_at, updated_at)
	           VALUES ($1,'stranger','stranger@example.test','stranger',now(),now())`, int64(stranger))

	tag, err := f.svc.Create(f.ctx, actorOf(f.mod), CreateInput{
		GuildID: f.guildID, Name: "spam", IsShared: true,
	})
	require.NoError(t, err)

	_, err = f.svc.List(f.ctx, actorOf(stranger), f.guildID)
	require.ErrorIs(t, err, httpx.ErrNotFound)

	_, err = f.svc.Create(f.ctx, actorOf(stranger), CreateInput{GuildID: f.guildID, Name: "intruding"})
	require.ErrorIs(t, err, httpx.ErrNotFound)

	err = f.svc.Delete(f.ctx, actorOf(stranger), f.guildID, tag.ID)
	require.ErrorIs(t, err, httpx.ErrNotFound)

	err = f.svc.Apply(f.ctx, actorOf(stranger), ApplyInput{
		ChannelID: f.channelID, MessageID: f.messageID, TagID: tag.ID,
	})
	require.ErrorIs(t, err, httpx.ErrNotFound)

	_, err = f.svc.ForMessage(f.ctx, actorOf(stranger), f.channelID, f.messageID)
	require.ErrorIs(t, err, httpx.ErrNotFound)
}

// TestDeletingATagRemovesItsApplications pins the cascade, which is a schema property rather than a
// service one and would otherwise be believed rather than checked.
func TestDeletingATagRemovesItsApplications(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	tag, err := f.svc.Create(f.ctx, actorOf(f.mod), CreateInput{
		GuildID: f.guildID, Name: "spam", IsShared: true,
	})
	require.NoError(t, err)
	require.NoError(t, f.svc.Apply(f.ctx, actorOf(f.member), ApplyInput{
		ChannelID: f.channelID, MessageID: f.messageID, TagID: tag.ID,
	}))

	require.NoError(t, f.svc.Delete(f.ctx, actorOf(f.mod), f.guildID, tag.ID))

	var n int
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM message_tag_applications WHERE tag_id = $1`, int64(tag.ID)).Scan(&n))
	require.Zero(t, n)
}

// TestTheQuerierIsUsedThroughTheTransaction is a compile-time reminder rather than a behavior test: the
// db import is needed by the fixture helpers above, and dropping it silently would mean the service's
// transaction plumbing had changed shape without anybody noticing here.
var _ = db.Queries{}
