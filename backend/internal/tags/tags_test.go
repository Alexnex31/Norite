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
// warns about, so the pin is a test that drives both with the same actors rather than a comment promising
// they agree. It said that from the start and drove only the listing, which is how the two copies came to
// disagree for an Instance Admin without anything noticing — found by /code-review, closed by M17's sweep,
// and the tier is among the actors below for that reason.
func TestAPrivateTagIsInvisibleToEverybodyElse(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	mine, err := f.svc.Create(f.ctx, actorOf(f.member), CreateInput{GuildID: f.guildID, Name: "mine"})
	require.NoError(t, err)

	// Both paths, for everybody but the owner of the tag: the guild's owner, a moderator, and the tier.
	operator := f.newInstanceAdmin(t)
	for _, who := range []snowflake.ID{f.owner, f.mod, operator} {
		list, err := f.svc.List(f.ctx, actorOf(who), f.guildID)
		require.NoError(t, err)
		require.Empty(t, list, "somebody else's private tag must not appear in the listing")

		// The single-row path: Apply loads the tag by id through loadInGuild, and so does every other
		// route naming one.
		err = f.svc.Apply(f.ctx, actorOf(who), ApplyInput{
			ChannelID: f.channelID, MessageID: f.messageID, TagID: mine.ID,
		})
		require.ErrorIs(t, err, httpx.ErrNotFound, "nor be reachable by its id")
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

	// And gives none up either. Before M17's manual pass, removing a tag from a deleted message worked
	// while adding one and reading them both answered 404; the three routes now agree that a deleted
	// message is not there. The row itself stays until the message is hard-deleted and it cascades.
	require.NoError(t, f.svc.Apply(f.ctx, actorOf(f.member), ApplyInput{
		ChannelID: f.channelID, MessageID: f.messageID, TagID: tag.ID,
	}))
	f.exec(t, `UPDATE messages SET deleted_at = now() WHERE id = $1`, int64(f.messageID))
	err = f.svc.Unapply(f.ctx, actorOf(f.member), ApplyInput{
		ChannelID: f.channelID, MessageID: f.messageID, TagID: tag.ID,
	})
	require.ErrorIs(t, err, httpx.ErrNotFound, "a soft-deleted message's tags cannot be removed either")
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

// TestAMessageIsReachedOnlyThroughItsOwnChannel is the manual pass's finding, pinned.
//
// The route names a channel and a message, and the channel is what gets authorized — so a message from
// another channel must answer exactly as a message that does not exist, on every route, or the check
// covered one channel while the act landed in another. M15's loadInChannel, which ForMessage had and
// Apply and Unapply did not.
//
// The message sits in a channel the member cannot view, because that is where it stops being tidiness:
// before the fix, tagging it through a channel they *can* read answered 204 against 404 for an id naming
// nothing — which message ids exist behind a hidden channel — and removing a moderator's tag from it
// answered 403 against 404, which shared tags had been put on something the member cannot read.
//
// The already-applied case is deliberate. A statement predicate alone refuses the insert, and then
// Apply's repeat-detection finds the moderator's row and reports success — the same oracle one step
// later. That is why the channel check is in Go ahead of everything as well as in the statement.
//
// Proved by removal in three legs, M16b's shape. Without the Go check in Apply this fails at the
// already-applied assertion and *passes* the fresh one — the statement predicate holding on its own.
// Without the statement predicate it passes, the Go check covering both. Without both it fails at the
// first assertion. Neutralize the predicate as `$3::bigint = $3::bigint` rather than deleting it: dropping
// the parameter fails the statement with "could not determine data type of parameter $3", which is a
// failure that proves nothing.
func TestAMessageIsReachedOnlyThroughItsOwnChannel(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	hidden := f.next(t)
	f.exec(t, `INSERT INTO channels (id, guild_id, name, type, position, created_at, updated_at)
	           VALUES ($1,$2,'staff',0,1,now(),now())`, int64(hidden), int64(f.guildID))
	// Target type 1 is a member: this one member is denied view, and nobody else is touched.
	f.exec(t, `INSERT INTO permission_overwrites (channel_id, target_type, target_id, allow, deny)
	           VALUES ($1,1,$2,0,$3)`, int64(hidden), int64(f.member), roles.PermViewChannel.Int64())
	secret := f.newMessage(t, hidden, f.owner)

	spam, err := f.svc.Create(f.ctx, actorOf(f.mod), CreateInput{
		GuildID: f.guildID, Name: "spam", IsShared: true,
	})
	require.NoError(t, err)
	keep, err := f.svc.Create(f.ctx, actorOf(f.mod), CreateInput{
		GuildID: f.guildID, Name: "keep", IsShared: true,
	})
	require.NoError(t, err)
	mine, err := f.svc.Create(f.ctx, actorOf(f.member), CreateInput{GuildID: f.guildID, Name: "mine"})
	require.NoError(t, err)

	// The moderator can see the channel and tags the message through its own route — which is also the
	// control that the message is a perfectly ordinary target.
	require.NoError(t, f.svc.Apply(f.ctx, actorOf(f.mod), ApplyInput{
		ChannelID: hidden, MessageID: secret, TagID: spam.ID,
	}))

	count := func() int {
		var n int
		require.NoError(t, f.pool.QueryRow(f.ctx,
			`SELECT count(*) FROM message_tag_applications WHERE message_id = $1`, int64(secret)).Scan(&n))
		return n
	}

	viaVisible := func(tag snowflake.ID) ApplyInput {
		return ApplyInput{ChannelID: f.channelID, MessageID: secret, TagID: tag}
	}
	member := actorOf(f.member)

	require.ErrorIs(t, f.svc.Apply(f.ctx, member, viaVisible(mine.ID)), httpx.ErrNotFound,
		"a fresh tag through the wrong channel")
	require.ErrorIs(t, f.svc.Apply(f.ctx, member, viaVisible(spam.ID)), httpx.ErrNotFound,
		"a tag already on the message, through the wrong channel — the repeat path must not report it")
	require.ErrorIs(t, f.svc.Unapply(f.ctx, member, viaVisible(spam.ID)), httpx.ErrNotFound,
		"removing a tag that is there must answer as removing one that is not")
	require.ErrorIs(t, f.svc.Unapply(f.ctx, member, viaVisible(keep.ID)), httpx.ErrNotFound)
	_, err = f.svc.ForMessage(f.ctx, member, f.channelID, secret)
	require.ErrorIs(t, err, httpx.ErrNotFound)

	require.Equal(t, 1, count(), "the member wrote nothing and removed nothing")

	// The binding is about the channel, not about visibility: the moderator, who can see both channels,
	// is refused through the wrong one too.
	require.ErrorIs(t, f.svc.Unapply(f.ctx, actorOf(f.mod), viaVisible(spam.ID)), httpx.ErrNotFound,
		"even for somebody who could reach the message through its own route")
	require.Equal(t, 1, count())
}

// newInstanceAdmin adds an account holding ADR 0008's layer 1, belonging to no guild.
func (f *fixture) newInstanceAdmin(t *testing.T) snowflake.ID {
	t.Helper()
	id := f.next(t)
	f.exec(t, `INSERT INTO users (id, username, email, display_name, created_at, updated_at)
	           VALUES ($1,'operator','operator@example.test','operator',now(),now())`, int64(id))
	f.exec(t, `INSERT INTO instance_admins (user_id) VALUES ($1)`, int64(id))
	return id
}

// auditEntries returns the actions written to this guild's audit log, oldest first.
func (f *fixture) auditEntries(t *testing.T) []string {
	t.Helper()
	rows, err := f.pool.Query(f.ctx,
		`SELECT action FROM audit_log_entries WHERE guild_id = $1 ORDER BY id`, int64(f.guildID))
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		require.NoError(t, rows.Scan(&a))
		out = append(out, a)
	}
	require.NoError(t, rows.Err())
	return out
}

// TestAnInstanceAdminCannotReachSomebodyElsesPrivateTag is M17's sweep finding, pinned.
//
// The tier passed loadInGuild by layer 1 while both SQL listings filtered the tag out, so an operator
// could apply a member's private tag — which the member then saw on a message they never tagged — and
// remove or delete it, all on a tag they could never enumerate, and with nothing recording it. Every path
// now answers 404, exactly as it does for the guild's owner.
func TestAnInstanceAdminCannotReachSomebodyElsesPrivateTag(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	operator := actorOf(f.newInstanceAdmin(t))

	private, err := f.svc.Create(f.ctx, actorOf(f.member), CreateInput{GuildID: f.guildID, Name: "mine"})
	require.NoError(t, err)
	in := ApplyInput{ChannelID: f.channelID, MessageID: f.messageID, TagID: private.ID}

	require.ErrorIs(t, f.svc.Apply(f.ctx, operator, in), httpx.ErrNotFound, "apply")

	// Applied by its owner, so there is something to remove.
	require.NoError(t, f.svc.Apply(f.ctx, actorOf(f.member), in))
	require.ErrorIs(t, f.svc.Unapply(f.ctx, operator, in), httpx.ErrNotFound, "remove")
	require.ErrorIs(t, f.svc.Delete(f.ctx, operator, f.guildID, private.ID), httpx.ErrNotFound, "delete")

	var n int
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM message_tag_applications WHERE tag_id = $1`, int64(private.ID)).Scan(&n))
	require.Equal(t, 1, n, "the member's own application is untouched")

	// And the tier still reaches everything shared, which is layer 1's ordinary reach.
	shared, err := f.svc.Create(f.ctx, operator, CreateInput{GuildID: f.guildID, Name: "ops", IsShared: true})
	require.NoError(t, err)
	require.NoError(t, f.svc.Delete(f.ctx, operator, f.guildID, shared.ID))
}

// TestAMutedMemberCannotPutASharedTagOnAMessage is the second sweep finding.
//
// Applying needed only the right to read, so a member denied PermSendMessages — the mute every guild
// uses — could not post and could still label other people's messages in front of everybody. A private
// tag is invisible to everybody else and stays open to them, and removing their own shared application
// stays open too, because redaction is what a mute wants.
func TestAMutedMemberCannotPutASharedTagOnAMessage(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	shared, err := f.svc.Create(f.ctx, actorOf(f.mod), CreateInput{
		GuildID: f.guildID, Name: "spam", IsShared: true,
	})
	require.NoError(t, err)
	private, err := f.svc.Create(f.ctx, actorOf(f.member), CreateInput{GuildID: f.guildID, Name: "later"})
	require.NoError(t, err)

	earlier := f.newMessage(t, f.channelID, f.owner)
	require.NoError(t, f.svc.Apply(f.ctx, actorOf(f.member), ApplyInput{
		ChannelID: f.channelID, MessageID: earlier, TagID: shared.ID,
	}), "before the mute, a member may apply a shared tag")

	// Target type 1 is a member: this one member is muted in this one channel.
	f.exec(t, `INSERT INTO permission_overwrites (channel_id, target_type, target_id, allow, deny)
	           VALUES ($1,1,$2,0,$3)`, int64(f.channelID), int64(f.member), roles.PermSendMessages.Int64())

	target := f.newMessage(t, f.channelID, f.owner)
	require.ErrorIs(t, f.svc.Apply(f.ctx, actorOf(f.member), ApplyInput{
		ChannelID: f.channelID, MessageID: target, TagID: shared.ID,
	}), httpx.ErrForbidden, "a muted member must not label a message in front of everybody")

	require.NoError(t, f.svc.Apply(f.ctx, actorOf(f.member), ApplyInput{
		ChannelID: f.channelID, MessageID: target, TagID: private.ID,
	}), "a private tag is a bookmark nobody else sees, and needs only the right to read")

	require.NoError(t, f.svc.Unapply(f.ctx, actorOf(f.member), ApplyInput{
		ChannelID: f.channelID, MessageID: earlier, TagID: shared.ID,
	}), "taking back your own label is redaction, which a mute does not forbid")
}

// newPlainMember adds a member holding nothing but @everyone's grant.
func (f *fixture) newPlainMember(t *testing.T, name string) snowflake.ID {
	t.Helper()
	id := f.next(t)
	f.exec(t, `INSERT INTO users (id, username, email, display_name, created_at, updated_at)
	           VALUES ($1,$2::text,$2::text||'@example.test',$2::text,now(),now())`, int64(id), name)
	f.exec(t, `INSERT INTO guild_members (guild_id, user_id, joined_at) VALUES ($1,$2,now())`,
		int64(f.guildID), int64(id))
	return id
}

// TestASharedTagIsTheGuildsNotItsCreators is the third sweep finding, and the test gap /code-review
// named: nothing reached the PermManageMessages branch of either path, because the moderator in every
// existing test was also the tag's creator.
//
// A demoted creator kept the power to delete the tag — cascading away everybody else's applications of
// it — and to strip other people's applications. Now both need the bit, for the creator as for anyone,
// and a moderator who did not create the tag holds both.
func TestASharedTagIsTheGuildsNotItsCreators(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	bystander := actorOf(f.newPlainMember(t, "bystander"))

	shared, err := f.svc.Create(f.ctx, actorOf(f.mod), CreateInput{
		GuildID: f.guildID, Name: "needs-review", IsShared: true,
	})
	require.NoError(t, err)
	in := ApplyInput{ChannelID: f.channelID, MessageID: f.messageID, TagID: shared.ID}
	require.NoError(t, f.svc.Apply(f.ctx, actorOf(f.member), in))

	// Somebody with no bit, who neither applied nor created it, reaches neither path.
	require.ErrorIs(t, f.svc.Unapply(f.ctx, bystander, in), httpx.ErrForbidden)
	require.ErrorIs(t, f.svc.Delete(f.ctx, bystander, f.guildID, shared.ID), httpx.ErrForbidden)

	// The creator loses the moderation bit.
	f.exec(t, `DELETE FROM guild_member_roles WHERE guild_id = $1 AND user_id = $2`,
		int64(f.guildID), int64(f.mod))

	require.ErrorIs(t, f.svc.Unapply(f.ctx, actorOf(f.mod), in), httpx.ErrForbidden,
		"a demoted creator must not strip a label somebody else applied")
	require.ErrorIs(t, f.svc.Delete(f.ctx, actorOf(f.mod), f.guildID, shared.ID), httpx.ErrForbidden,
		"a demoted creator must not delete a tag, and with it everybody else's applications")

	// A moderator who did not create the tag holds both — the branch nothing reached before.
	other := f.newPlainMember(t, "second-mod")
	role := f.next(t)
	f.exec(t, `INSERT INTO roles (id, guild_id, name, permissions, position, is_default, created_at, updated_at)
	           VALUES ($1,$2,'mod2',$3,2,false,now(),now())`,
		int64(role), int64(f.guildID), roles.PermManageMessages.Int64())
	f.exec(t, `INSERT INTO guild_member_roles (guild_id, user_id, role_id) VALUES ($1,$2,$3)`,
		int64(f.guildID), int64(other), int64(role))

	require.NoError(t, f.svc.Unapply(f.ctx, actorOf(other), in))
	require.NoError(t, f.svc.Delete(f.ctx, actorOf(other), f.guildID, shared.ID))
}

// TestModerationOverSomebodyElsesTaggingIsAudited is the fourth sweep finding: rule 2 counts acting over
// somebody else as administrative whoever does it, and a moderator removing a member's label wrote no
// entry while deleting that member's message wrote one.
//
// Asserted in both directions, as M16 asserted filing: each act on your own tagging writes nothing, each
// act over somebody else's writes exactly one entry, and none of them carries message content.
func TestModerationOverSomebodyElsesTaggingIsAudited(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	shared, err := f.svc.Create(f.ctx, actorOf(f.mod), CreateInput{
		GuildID: f.guildID, Name: "spam", IsShared: true,
	})
	require.NoError(t, err)
	in := ApplyInput{ChannelID: f.channelID, MessageID: f.messageID, TagID: shared.ID}

	// A member applying and removing their own label: nothing.
	require.NoError(t, f.svc.Apply(f.ctx, actorOf(f.member), in))
	require.NoError(t, f.svc.Unapply(f.ctx, actorOf(f.member), in))
	require.Empty(t, f.auditEntries(t), "tagging your own way is not administrative")

	// A moderator removing the member's label: one entry, naming whose it was.
	require.NoError(t, f.svc.Apply(f.ctx, actorOf(f.member), in))
	require.NoError(t, f.svc.Unapply(f.ctx, actorOf(f.mod), in))
	require.Equal(t, []string{ActionTagRemove}, f.auditEntries(t))

	var changes map[string]any
	var target int64
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT target_id, changes FROM audit_log_entries WHERE guild_id = $1 AND action = $2`,
		int64(f.guildID), ActionTagRemove).Scan(&target, &changes))
	require.Equal(t, int64(f.messageID), target)
	require.Equal(t, f.member.String(), changes["applied_by"], "the entry must say whose label was removed")
	require.NotContains(t, changes, "content", "rule 13: ids and a tag name, never message content")

	// A moderator deleting a shared tag only they ever applied: vocabulary, not authority over anybody.
	solo, err := f.svc.Create(f.ctx, actorOf(f.mod), CreateInput{
		GuildID: f.guildID, Name: "solo", IsShared: true,
	})
	require.NoError(t, err)
	require.NoError(t, f.svc.Apply(f.ctx, actorOf(f.mod), ApplyInput{
		ChannelID: f.channelID, MessageID: f.messageID, TagID: solo.ID,
	}))
	require.NoError(t, f.svc.Delete(f.ctx, actorOf(f.mod), f.guildID, solo.ID))
	require.Equal(t, []string{ActionTagRemove}, f.auditEntries(t), "no entry for deleting your own vocabulary")

	// A moderator deleting a shared tag that carries somebody else's label: one entry, with the count.
	require.NoError(t, f.svc.Apply(f.ctx, actorOf(f.member), in))
	require.NoError(t, f.svc.Delete(f.ctx, actorOf(f.mod), f.guildID, shared.ID))
	require.Equal(t, []string{ActionTagRemove, ActionTagDelete}, f.auditEntries(t))

	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT changes FROM audit_log_entries WHERE guild_id = $1 AND action = $2`,
		int64(f.guildID), ActionTagDelete).Scan(&changes))
	require.Equal(t, map[string]any{"from": "spam"}, changes["name"])
	require.EqualValues(t, 1, changes["applications_removed"])
}

// TestANameMustNotCarryANul is the fifth: Postgres cannot store U+0000 in text, JSON decodes it, and the
// insert failed as a 500.
func TestANameMustNotCarryANul(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	_, err := f.svc.Create(f.ctx, actorOf(f.member), CreateInput{GuildID: f.guildID, Name: "a\x00b"})
	require.ErrorIs(t, err, httpx.ErrBadRequest)
}

// TestCreatingInAGuildThatIsGoneIsNotFound covers the foreign-key half of the fifth finding. An Instance
// Admin short-circuits layer 1 without reading the guild row (M72 owns changing that), so a guild id
// naming nothing reached the insert and failed as a 500.
func TestCreatingInAGuildThatIsGoneIsNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	operator := actorOf(f.newInstanceAdmin(t))

	_, err := f.svc.Create(f.ctx, operator, CreateInput{GuildID: f.next(t), Name: "ghost", IsShared: true})
	require.ErrorIs(t, err, httpx.ErrNotFound)
}
