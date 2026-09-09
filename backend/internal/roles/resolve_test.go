// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package roles_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/database"
	"github.com/Alexnex31/Norite/backend/internal/platform/dbtest"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
	"github.com/Alexnex31/Norite/backend/migrations"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

// fixture is a migrated database plus the handful of inserts these tests need.
//
// Every row is written with raw SQL rather than through a service, and that is deliberate rather than a
// shortcut: M12 has no endpoint that writes a permission overwrite — those arrive at M13 — so going
// through the API would mean this milestone could not test layer 5 at all. Inserting directly is what
// proves the overwrite code is live rather than a stub reading an empty table.
type fixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	q    *db.Queries
	ids  *snowflake.Generator
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	dsn := dbtest.FreshDatabase(t)
	ctx := t.Context()

	require.NoError(t, database.Migrate(ctx, database.MigrateOptions{
		DatabaseURL: dsn,
		Source:      migrations.FS,
		SourceDir:   ".",
		LockTimeout: 30 * time.Second,
	}))

	pool, err := database.New(ctx, database.PoolOptions{
		DatabaseURL:    dsn,
		MaxConns:       4,
		MinConns:       1,
		ConnectTimeout: 10 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	ids, err := snowflake.NewGenerator(0)
	require.NoError(t, err)

	return &fixture{t: t, pool: pool, q: db.New(pool), ids: ids}
}

func (f *fixture) next() snowflake.ID {
	f.t.Helper()
	id, err := f.ids.Next()
	require.NoError(f.t, err)
	return id
}

func (f *fixture) exec(ctx context.Context, sql string, args ...any) {
	f.t.Helper()
	_, err := f.pool.Exec(ctx, sql, args...)
	require.NoError(f.t, err)
}

func (f *fixture) newUser(ctx context.Context, name string) snowflake.ID {
	id := f.next()
	f.exec(ctx, `INSERT INTO users (id, username, email, display_name) VALUES ($1, $2, $3, $4)`,
		int64(id), name, name+"@example.test", name)
	return id
}

// newGuild creates a guild and its @everyone role, mirroring what guild creation does in one transaction.
func (f *fixture) newGuild(ctx context.Context, owner snowflake.ID, everyonePerms roles.Permission,
) (guildID, everyoneID snowflake.ID) {
	guildID, everyoneID = f.next(), f.next()

	f.exec(ctx, `INSERT INTO guilds (id, name, owner_id) VALUES ($1, 'test guild', $2)`,
		int64(guildID), int64(owner))
	f.exec(ctx, `INSERT INTO roles (id, guild_id, name, position, is_default, permissions)
	             VALUES ($1, $2, '@everyone', 0, true, $3)`,
		int64(everyoneID), int64(guildID), everyonePerms.Int64())
	f.exec(ctx, `INSERT INTO guild_members (guild_id, user_id) VALUES ($1, $2)`,
		int64(guildID), int64(owner))

	return guildID, everyoneID
}

func (f *fixture) join(ctx context.Context, guildID, userID snowflake.ID) {
	f.exec(ctx, `INSERT INTO guild_members (guild_id, user_id) VALUES ($1, $2)`,
		int64(guildID), int64(userID))
}

func (f *fixture) newRole(ctx context.Context, guildID snowflake.ID, position int, perms roles.Permission,
) snowflake.ID {
	id := f.next()
	f.exec(ctx, `INSERT INTO roles (id, guild_id, name, position, permissions) VALUES ($1, $2, $3, $4, $5)`,
		int64(id), int64(guildID), "role", position, perms.Int64())
	return id
}

func (f *fixture) grantRole(ctx context.Context, guildID, userID, roleID snowflake.ID) {
	f.exec(ctx, `INSERT INTO guild_member_roles (guild_id, user_id, role_id) VALUES ($1, $2, $3)`,
		int64(guildID), int64(userID), int64(roleID))
}

func (f *fixture) newChannel(ctx context.Context, guildID snowflake.ID) snowflake.ID {
	id := f.next()
	f.exec(ctx, `INSERT INTO channels (id, guild_id, type, name, position) VALUES ($1, $2, 0, 'general', 0)`,
		int64(id), int64(guildID))
	return id
}

func (f *fixture) overwrite(ctx context.Context, channelID snowflake.ID, targetType int16,
	targetID snowflake.ID, allow, deny roles.Permission,
) {
	f.exec(ctx, `INSERT INTO permission_overwrites (channel_id, target_type, target_id, allow, deny)
	             VALUES ($1, $2, $3, $4, $5)`,
		int64(channelID), targetType, int64(targetID), allow.Int64(), deny.Int64())
}

// TestResolveFollowsTheDocumentedOrder walks ADR 0008's layers 2 through 5 in the order the ADR fixes.
//
// Each case is a whole guild rather than a shared fixture mutated in place, because the layers
// short-circuit: an owner never reaches the role OR, and an administrator never reaches the overwrites.
// A shared fixture would let a case pass for the wrong reason.
func TestResolveFollowsTheDocumentedOrder(t *testing.T) {
	t.Parallel()

	t.Run("layer 2: the owner bypasses everything", func(t *testing.T) {
		f := newFixture(t)
		ctx := t.Context()

		owner := f.newUser(ctx, "owner")
		// @everyone grants nothing at all, so anything the owner resolves comes from the bypass.
		guildID, _ := f.newGuild(ctx, owner, 0)

		got, err := roles.Resolve(ctx, f.q, guildID, owner, 0)
		require.NoError(t, err)
		require.True(t, got.Permissions.Has(roles.PermBanMembers|roles.PermManageGuild),
			"the owner must hold every permission, got %s", got.Permissions)
	})

	t.Run("layer 3: PermAdministrator short-circuits", func(t *testing.T) {
		f := newFixture(t)
		ctx := t.Context()

		owner := f.newUser(ctx, "owner")
		member := f.newUser(ctx, "member")
		guildID, _ := f.newGuild(ctx, owner, 0)
		f.join(ctx, guildID, member)

		admin := f.newRole(ctx, guildID, 1, roles.PermAdministrator)
		f.grantRole(ctx, guildID, member, admin)

		got, err := roles.Resolve(ctx, f.q, guildID, member, 0)
		require.NoError(t, err)
		require.True(t, got.Permissions.Has(roles.PermBanMembers|roles.PermManageGuild),
			"an administrator must hold every permission, got %s", got.Permissions)
	})

	t.Run("layer 4: role bits are OR'd, including @everyone", func(t *testing.T) {
		f := newFixture(t)
		ctx := t.Context()

		owner := f.newUser(ctx, "owner")
		member := f.newUser(ctx, "member")
		guildID, _ := f.newGuild(ctx, owner, roles.PermViewChannel)
		f.join(ctx, guildID, member)

		f.grantRole(ctx, guildID, member, f.newRole(ctx, guildID, 1, roles.PermSendMessages))
		f.grantRole(ctx, guildID, member, f.newRole(ctx, guildID, 2, roles.PermCreateInvite))

		got, err := roles.Resolve(ctx, f.q, guildID, member, 0)
		require.NoError(t, err)

		want := roles.PermViewChannel | roles.PermSendMessages | roles.PermCreateInvite
		require.Equal(t, want, got.Permissions, "expected the union of @everyone and both roles")
		require.False(t, got.Permissions.Has(roles.PermBanMembers), "no role granted PermBanMembers")
	})

	t.Run("layer 4: a role the member does not hold contributes nothing", func(t *testing.T) {
		f := newFixture(t)
		ctx := t.Context()

		owner := f.newUser(ctx, "owner")
		member := f.newUser(ctx, "member")
		guildID, _ := f.newGuild(ctx, owner, roles.PermViewChannel)
		f.join(ctx, guildID, member)

		// Created in the guild, granted to nobody.
		f.newRole(ctx, guildID, 1, roles.PermBanMembers)

		got, err := roles.Resolve(ctx, f.q, guildID, member, 0)
		require.NoError(t, err)
		require.Equal(t, roles.PermViewChannel, got.Permissions)
	})
}

// TestOverwritesApplyMostSpecificLast is ADR 0008 layer 5, and it is the layer no M12 endpoint can write
// to — every row here is inserted directly. Without this test layer 5 ships unexercised and looks fine,
// because it reads an empty table and correctly contributes nothing.
func TestOverwritesApplyMostSpecificLast(t *testing.T) {
	t.Parallel()

	t.Run("an @everyone overwrite denies what the role granted", func(t *testing.T) {
		f := newFixture(t)
		ctx := t.Context()

		owner := f.newUser(ctx, "owner")
		member := f.newUser(ctx, "member")
		guildID, everyoneID := f.newGuild(ctx, owner, roles.PermViewChannel|roles.PermSendMessages)
		f.join(ctx, guildID, member)
		channelID := f.newChannel(ctx, guildID)

		f.overwrite(ctx, channelID, roles.OverwriteTargetRole, everyoneID, 0, roles.PermSendMessages)

		got, err := roles.Resolve(ctx, f.q, guildID, member, channelID)
		require.NoError(t, err)
		require.False(t, got.Permissions.Has(roles.PermSendMessages), "the @everyone deny must apply")
		require.True(t, got.Permissions.Has(roles.PermViewChannel), "it must not remove what it did not deny")
	})

	t.Run("a role overwrite beats the @everyone deny", func(t *testing.T) {
		f := newFixture(t)
		ctx := t.Context()

		owner := f.newUser(ctx, "owner")
		member := f.newUser(ctx, "member")
		guildID, everyoneID := f.newGuild(ctx, owner, roles.PermViewChannel|roles.PermSendMessages)
		f.join(ctx, guildID, member)
		channelID := f.newChannel(ctx, guildID)

		roleID := f.newRole(ctx, guildID, 1, 0)
		f.grantRole(ctx, guildID, member, roleID)

		f.overwrite(ctx, channelID, roles.OverwriteTargetRole, everyoneID, 0, roles.PermSendMessages)
		f.overwrite(ctx, channelID, roles.OverwriteTargetRole, roleID, roles.PermSendMessages, 0)

		got, err := roles.Resolve(ctx, f.q, guildID, member, channelID)
		require.NoError(t, err)
		require.True(t, got.Permissions.Has(roles.PermSendMessages),
			"a role allow is more specific than the @everyone deny and must win")
	})

	t.Run("a member overwrite beats a role deny", func(t *testing.T) {
		f := newFixture(t)
		ctx := t.Context()

		owner := f.newUser(ctx, "owner")
		member := f.newUser(ctx, "member")
		guildID, _ := f.newGuild(ctx, owner, roles.PermViewChannel|roles.PermSendMessages)
		f.join(ctx, guildID, member)
		channelID := f.newChannel(ctx, guildID)

		roleID := f.newRole(ctx, guildID, 1, 0)
		f.grantRole(ctx, guildID, member, roleID)

		f.overwrite(ctx, channelID, roles.OverwriteTargetRole, roleID, 0, roles.PermSendMessages)
		f.overwrite(ctx, channelID, roles.OverwriteTargetMember, member, roles.PermSendMessages, 0)

		got, err := roles.Resolve(ctx, f.q, guildID, member, channelID)
		require.NoError(t, err)
		require.True(t, got.Permissions.Has(roles.PermSendMessages), "the member overwrite is the most specific tier")
	})

	t.Run("two role overwrites are unioned, and an allow beats the other's deny", func(t *testing.T) {
		f := newFixture(t)
		ctx := t.Context()

		owner := f.newUser(ctx, "owner")
		member := f.newUser(ctx, "member")
		guildID, _ := f.newGuild(ctx, owner, roles.PermViewChannel)
		f.join(ctx, guildID, member)
		channelID := f.newChannel(ctx, guildID)

		denying := f.newRole(ctx, guildID, 1, 0)
		allowing := f.newRole(ctx, guildID, 2, 0)
		f.grantRole(ctx, guildID, member, denying)
		f.grantRole(ctx, guildID, member, allowing)

		f.overwrite(ctx, channelID, roles.OverwriteTargetRole, denying, 0, roles.PermSendMessages)
		f.overwrite(ctx, channelID, roles.OverwriteTargetRole, allowing, roles.PermSendMessages, 0)

		got, err := roles.Resolve(ctx, f.q, guildID, member, channelID)
		require.NoError(t, err)
		require.True(t, got.Permissions.Has(roles.PermSendMessages),
			"role overwrites accumulate, and deny is applied before allow")

		// Note what this case does *not* establish. The rows come back in whatever order Postgres
		// finds them, so a sequential implementation would pass or fail here by luck — it did pass,
		// against a deliberately broken build. Order-independence is pinned deterministically in
		// TestRoleOverwritesDoNotDependOnRowOrder, which drives applyOverwrites with both orderings.
	})

	t.Run("an overwrite for a role the member does not hold is ignored", func(t *testing.T) {
		f := newFixture(t)
		ctx := t.Context()

		owner := f.newUser(ctx, "owner")
		member := f.newUser(ctx, "member")
		guildID, _ := f.newGuild(ctx, owner, roles.PermViewChannel|roles.PermSendMessages)
		f.join(ctx, guildID, member)
		channelID := f.newChannel(ctx, guildID)

		other := f.newRole(ctx, guildID, 1, 0)
		f.overwrite(ctx, channelID, roles.OverwriteTargetRole, other, 0, roles.PermSendMessages)

		got, err := roles.Resolve(ctx, f.q, guildID, member, channelID)
		require.NoError(t, err)
		require.True(t, got.Permissions.Has(roles.PermSendMessages),
			"a deny on somebody else's role must not reach this member")
	})

	t.Run("an overwrite for another member is ignored", func(t *testing.T) {
		f := newFixture(t)
		ctx := t.Context()

		owner := f.newUser(ctx, "owner")
		member := f.newUser(ctx, "member")
		stranger := f.newUser(ctx, "stranger")
		guildID, _ := f.newGuild(ctx, owner, roles.PermViewChannel|roles.PermSendMessages)
		f.join(ctx, guildID, member)
		channelID := f.newChannel(ctx, guildID)

		f.overwrite(ctx, channelID, roles.OverwriteTargetMember, stranger, 0, roles.PermSendMessages)

		got, err := roles.Resolve(ctx, f.q, guildID, member, channelID)
		require.NoError(t, err)
		require.True(t, got.Permissions.Has(roles.PermSendMessages))
	})
}

// TestOverwritesFromAnotherGuildAreNotApplied is rule 1's cross-scope check at the query layer.
//
// The handler is expected to load a channel and authorize against its own guild_id, and D2 has a test for
// that. This one covers the case where a caller reaches Resolve with a mismatched pair anyway: the join to
// channels scopes the read, so the overwrite cannot apply. Confirmed by removal — drop `AND c.guild_id`
// from the query and this fails.
func TestOverwritesFromAnotherGuildAreNotApplied(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()

	owner := f.newUser(ctx, "owner")
	member := f.newUser(ctx, "member")

	mine, _ := f.newGuild(ctx, owner, roles.PermViewChannel|roles.PermSendMessages)
	f.join(ctx, mine, member)

	theirs, _ := f.newGuild(ctx, owner, 0)
	theirChannel := f.newChannel(ctx, theirs)
	f.overwrite(ctx, theirChannel, roles.OverwriteTargetMember, member, 0, roles.PermSendMessages)

	got, err := roles.Resolve(ctx, f.q, mine, member, theirChannel)
	require.NoError(t, err)
	require.True(t, got.Permissions.Has(roles.PermSendMessages),
		"an overwrite on a channel belonging to another guild must not be applied")
}

func TestResolveRefusesANonMemberAndAMissingGuild(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()

	owner := f.newUser(ctx, "owner")
	stranger := f.newUser(ctx, "stranger")
	guildID, _ := f.newGuild(ctx, owner, roles.PermViewChannel)

	t.Run("a non-member", func(t *testing.T) {
		_, err := roles.Resolve(ctx, f.q, guildID, stranger, 0)
		require.ErrorIs(t, err, roles.ErrNotAMember)
	})

	// The same error, deliberately. Distinguishing them would turn a list of snowflakes into a map of
	// which guilds exist on the instance — M11 settled this for session ids.
	t.Run("a guild that does not exist", func(t *testing.T) {
		_, err := roles.Resolve(ctx, f.q, f.next(), stranger, 0)
		require.ErrorIs(t, err, roles.ErrNotAMember)
	})
}

// TestAGuildLevelCheckSkipsTheOverwriteQuery pins the channelID == 0 path.
//
// It is behavioral rather than a round-trip count: an overwrite that would change the answer is present,
// and resolving without a channel must not see it.
func TestAGuildLevelCheckSkipsTheOverwriteQuery(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()

	owner := f.newUser(ctx, "owner")
	member := f.newUser(ctx, "member")
	guildID, everyoneID := f.newGuild(ctx, owner, roles.PermViewChannel|roles.PermSendMessages)
	f.join(ctx, guildID, member)
	channelID := f.newChannel(ctx, guildID)

	f.overwrite(ctx, channelID, roles.OverwriteTargetRole, everyoneID, 0, roles.PermSendMessages)

	guildLevel, err := roles.Resolve(ctx, f.q, guildID, member, 0)
	require.NoError(t, err)
	require.True(t, guildLevel.Permissions.Has(roles.PermSendMessages), "a guild-level check must ignore channel overwrites")

	inChannel, err := roles.Resolve(ctx, f.q, guildID, member, channelID)
	require.NoError(t, err)
	require.False(t, inChannel.Permissions.Has(roles.PermSendMessages), "the same check in the channel must see the deny")
}

// TestStandingSurvivesEveryShortCircuit is Milestone M13's half of Resolve, and it exists because two of
// the three early returns are places a position can be silently dropped.
//
// Resolve returns on the owner branch before the role loop runs, on the layer-3 administrator branch after
// it, and on the ordinary path at the end. Only the first of those has no position to report. The
// administrator case is the one worth a test of its own: the loop has already computed the value, so
// building a Resolution from permAll at that return discards a number that was in hand — and the symptom
// is every administrator silently standing on the floor, which reads as a permission bug rather than a
// plumbing one and invites the repair that hands the guild to whoever holds the highest role.
func TestStandingSurvivesEveryShortCircuit(t *testing.T) {
	t.Parallel()

	t.Run("an ordinary member stands at their highest role", func(t *testing.T) {
		f := newFixture(t)
		ctx := t.Context()

		owner := f.newUser(ctx, "owner")
		member := f.newUser(ctx, "member")
		guildID, _ := f.newGuild(ctx, owner, roles.PermViewChannel)
		f.join(ctx, guildID, member)

		low := f.newRole(ctx, guildID, 3, roles.PermSendMessages)
		high := f.newRole(ctx, guildID, 7, roles.PermKickMembers)
		unheld := f.newRole(ctx, guildID, 9, roles.PermBanMembers)
		f.grantRole(ctx, guildID, member, low)
		f.grantRole(ctx, guildID, member, high)

		got, err := roles.Resolve(ctx, f.q, guildID, member, 0)
		require.NoError(t, err)
		require.Equal(t, int32(7), got.Standing(),
			"standing is the highest position held, not the highest that exists")
		require.NotEqual(t, unheld, snowflake.ID(0), "the unheld role exists to not be counted")
	})

	t.Run("a member holding no role stands on the floor", func(t *testing.T) {
		f := newFixture(t)
		ctx := t.Context()

		owner := f.newUser(ctx, "owner")
		member := f.newUser(ctx, "member")
		guildID, _ := f.newGuild(ctx, owner, roles.PermViewChannel)
		f.join(ctx, guildID, member)

		got, err := roles.Resolve(ctx, f.q, guildID, member, 0)
		require.NoError(t, err)
		require.Equal(t, int32(0), got.Standing(),
			"@everyone is position 0 and a member with nothing else is at the floor")
	})

	t.Run("an administrator keeps their position through the layer-3 short-circuit", func(t *testing.T) {
		f := newFixture(t)
		ctx := t.Context()

		owner := f.newUser(ctx, "owner")
		admin := f.newUser(ctx, "admin")
		guildID, _ := f.newGuild(ctx, owner, roles.PermViewChannel)
		f.join(ctx, guildID, admin)

		adminRole := f.newRole(ctx, guildID, 5, roles.PermAdministrator)
		f.grantRole(ctx, guildID, admin, adminRole)

		got, err := roles.Resolve(ctx, f.q, guildID, admin, 0)
		require.NoError(t, err)
		require.True(t, got.Permissions.Has(roles.PermBanMembers),
			"layer 3 short-circuits permissions to everything")
		require.Equal(t, int32(5), got.Standing(),
			"and says nothing about standing — ADR 0008 puts the two in different layers")
		require.False(t, got.IsOwner(), "an administrator is not the owner")
	})

	t.Run("the owner is identified by ownership, never by position", func(t *testing.T) {
		f := newFixture(t)
		ctx := t.Context()

		owner := f.newUser(ctx, "owner")
		guildID, _ := f.newGuild(ctx, owner, roles.PermViewChannel)

		got, err := roles.Resolve(ctx, f.q, guildID, owner, 0)
		require.NoError(t, err)
		require.True(t, got.IsOwner(), "layer 2 is what a caller must ask about first")
		require.Equal(t, int32(0), got.Standing(),
			"the owner's standing is meaningless and left at zero — which is why IsOwner is asked first")
	})
}

// TestInChannelResolvesManyChannelsFromOneQuery covers the seam the channel listing needs.
//
// The listing has to answer "can this account see it" for every channel in a guild, and calling Resolve
// once per channel would be two queries each where the authority half is identical. So it resolves once at
// guild level and applies each channel's overwrites to that result.
//
// The second assertion is the one that matters structurally: InChannel resolves from the *base*, so
// calling it twice cannot compound. Were it to fold overwrites into whatever Permissions currently held,
// a listing loop would apply every channel's denies cumulatively and hide channels nobody denied.
func TestInChannelResolvesManyChannelsFromOneQuery(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()

	owner := f.newUser(ctx, "owner")
	member := f.newUser(ctx, "member")
	guildID, everyoneID := f.newGuild(ctx, owner, roles.PermViewChannel|roles.PermSendMessages)
	f.join(ctx, guildID, member)

	open := f.newChannel(ctx, guildID)
	denied := f.newChannel(ctx, guildID)
	f.overwrite(ctx, denied, roles.OverwriteTargetRole, everyoneID, 0, roles.PermViewChannel)

	res, err := roles.Resolve(ctx, f.q, guildID, member, 0)
	require.NoError(t, err)

	load := func(channelID snowflake.ID) []db.PermissionOverwrite {
		rows, err := f.q.ListChannelPermissionOverwrites(ctx, db.ListChannelPermissionOverwritesParams{
			ChannelID: int64(channelID),
			GuildID:   int64(guildID),
		})
		require.NoError(t, err)
		return rows
	}

	require.True(t, res.InChannel(open, load(open)).Has(roles.PermViewChannel),
		"the open channel is visible")
	require.False(t, res.InChannel(denied, load(denied)).Has(roles.PermViewChannel),
		"the denied channel is not")

	// Applied again, in the other order, on the same resolution. Every answer must be unchanged.
	require.False(t, res.InChannel(denied, load(denied)).Has(roles.PermViewChannel),
		"a second call must not compound")
	require.True(t, res.InChannel(open, load(open)).Has(roles.PermViewChannel),
		"and the deny must not have leaked into a channel that does not carry it")

	// The case the two assertions above cannot see, and the only one that distinguishes resolving from
	// `base` from resolving from `Permissions`.
	//
	// Above, `res` came from a guild-level Resolve, where those two fields are equal — so a compounding
	// implementation returns the identical answer and the test passes against it. Confirmed by making
	// InChannel compound and watching nothing fail. What separates them is a resolution that *already*
	// carries a channel's overwrites, which is what a channel-scoped Resolve returns.
	inDenied, err := roles.Resolve(ctx, f.q, guildID, member, denied)
	require.NoError(t, err)
	require.False(t, inDenied.Permissions.Has(roles.PermViewChannel), "the deny applied, as it should")

	require.True(t, inDenied.InChannel(open, load(open)).Has(roles.PermViewChannel),
		"asking about the open channel must answer from the guild-level base, not from the denied "+
			"channel's result — otherwise a listing loop accumulates every channel's denies and hides "+
			"channels nobody denied")
}

// TestAnOwnerAndAnAdministratorAreNotFilteredByOverwrites pins the half of InChannel that a caller could
// otherwise get wrong by writing the loop themselves.
//
// Layers 2 and 3 sit above layer 5, so no overwrite denies them anything. Putting that check inside
// InChannel rather than at the call site is what stops a channel listing filtering a channel away from the
// one account that must always be able to see it.
func TestAnOwnerAndAnAdministratorAreNotFilteredByOverwrites(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()

	owner := f.newUser(ctx, "owner")
	admin := f.newUser(ctx, "admin")
	guildID, everyoneID := f.newGuild(ctx, owner, roles.PermViewChannel)
	f.join(ctx, guildID, admin)
	adminRole := f.newRole(ctx, guildID, 5, roles.PermAdministrator)
	f.grantRole(ctx, guildID, admin, adminRole)

	channelID := f.newChannel(ctx, guildID)
	f.overwrite(ctx, channelID, roles.OverwriteTargetRole, everyoneID, 0, roles.PermViewChannel)

	rows, err := f.q.ListChannelPermissionOverwrites(ctx, db.ListChannelPermissionOverwritesParams{
		ChannelID: int64(channelID),
		GuildID:   int64(guildID),
	})
	require.NoError(t, err)

	for _, tc := range []struct {
		name string
		who  snowflake.ID
	}{{"owner", owner}, {"administrator", admin}} {
		res, err := roles.Resolve(ctx, f.q, guildID, tc.who, 0)
		require.NoError(t, err)
		require.True(t, res.InChannel(channelID, rows).Has(roles.PermViewChannel),
			"%s must see a channel @everyone is denied", tc.name)
	}
}

// TestOutranksIsStrictlyGreaterAndTheOwnerIsAbove covers ADR 0008 layer 4's second sentence, which is the
// whole of the hierarchy rule and the thing nine call sites in this milestone are about to depend on.
//
// The strictness is the part worth pinning. Equal standing must not permit acting, or two members whose
// highest role is the same role could kick each other; and since @everyone is position 0, the same rule is
// what stops two members holding nothing from acting on one another.
func TestOutranksIsStrictlyGreaterAndTheOwnerIsAbove(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()

	owner := f.newUser(ctx, "owner")
	mod := f.newUser(ctx, "mod")
	peer := f.newUser(ctx, "peer")
	plain := f.newUser(ctx, "plain")

	guildID, _ := f.newGuild(ctx, owner, roles.PermViewChannel)
	for _, u := range []snowflake.ID{mod, peer, plain} {
		f.join(ctx, guildID, u)
	}

	modRole := f.newRole(ctx, guildID, 5, roles.PermKickMembers)
	f.grantRole(ctx, guildID, mod, modRole)
	f.grantRole(ctx, guildID, peer, modRole)

	resolve := func(u snowflake.ID) roles.Resolution {
		res, err := roles.Resolve(ctx, f.q, guildID, u, 0)
		require.NoError(t, err)
		return res
	}

	modRes, peerRes, plainRes, ownerRes := resolve(mod), resolve(peer), resolve(plain), resolve(owner)

	require.True(t, modRes.Outranks(plainRes.Standing()), "5 outranks the floor")
	require.False(t, plainRes.Outranks(modRes.Standing()), "and the floor does not outrank 5")

	require.False(t, modRes.Outranks(peerRes.Standing()),
		"equal standing is not enough, or two holders of one role could act on each other")
	require.False(t, plainRes.Outranks(plainRes.Standing()),
		"two members at the floor cannot act on one another either")

	require.True(t, ownerRes.Outranks(modRes.Standing()), "the owner is above everyone")
	require.True(t, ownerRes.Outranks(0), "including at the floor, where their own standing reads as 0")

	// The owner as *target*, which comparing standings cannot express and which the first version of this
	// primitive got backwards. The owner's own standing is a meaningless zero, so a bare positional
	// comparison reports that any role-holder outranks them.
	require.True(t, modRes.Outranks(ownerRes.Standing()),
		"the bare positional form does say 5 > 0, which is why it is not the one to use on a member")
	require.False(t, modRes.OutranksMember(owner, ownerRes.Standing()),
		"nobody inside the guild acts on its owner")
	require.True(t, modRes.OutranksMember(plain, plainRes.Standing()),
		"and an ordinary member is still reachable")
	require.False(t, modRes.OutranksMember(peer, peerRes.Standing()),
		"equal standing still refuses, through the member form too")
}

// TestBothStandingReadersAgreeBelowTheFloor is the boundary two different functions have to meet at.
//
// Resolve computes the *actor's* standing by folding a maximum over a zero seed; GetMemberHighestRolePosition
// computes the *target's* with an aggregate. Nothing in the schema keeps a role from sitting at or below
// zero — roles.position carries no CHECK, and SetRolePosition's lower bound only guards the reorder path —
// so the two must floor identically or one hierarchy comparison gets two answers depending on which side
// of it a member is on.
func TestBothStandingReadersAgreeBelowTheFloor(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()

	owner := f.newUser(ctx, "owner")
	member := f.newUser(ctx, "member")
	guildID, _ := f.newGuild(ctx, owner, roles.PermViewChannel)
	f.join(ctx, guildID, member)

	// Written directly, which is the only way in: no endpoint produces this and none should.
	below := f.newRole(ctx, guildID, -5, roles.PermSendMessages)
	f.grantRole(ctx, guildID, member, below)

	res, err := roles.Resolve(ctx, f.q, guildID, member, 0)
	require.NoError(t, err)

	asTarget, err := f.q.GetMemberHighestRolePosition(ctx, db.GetMemberHighestRolePositionParams{
		GuildID: int64(guildID),
		UserID:  int64(member),
	})
	require.NoError(t, err)

	require.Equal(t, int32(0), res.Standing(), "the actor reader floors at @everyone's position")
	require.Equal(t, int32(0), asTarget, "and so must the target reader")
	require.Equal(t, res.Standing(), asTarget, "one comparison cannot have two answers")
}

// TestAGuildWideSliceResolvesEachChannelSeparately is the case ListGuildPermissionOverwrites exists to
// serve and that nothing exercised until a security review asked for it.
//
// The channel listing loads every overwrite in the guild in one query and asks about each channel in turn.
// Both overwrite queries deliberately return the same row type, so handing the whole slice to InChannel is
// type-correct — and before the channel id became a parameter it silently merged every channel's tiers:
// an allow on one channel restoring what another denied, which shows a private channel in a listing, and
// a member-tier deny anywhere hiding every channel.
//
// Confirmed by removal: drop the ChannelID guard in applyOverwrites and this test fails — on the
// #general assertion, not the #staff one it would be natural to predict. That is the sharper version of
// the bug. The @everyone tier is applied row by row as it is encountered, so merging two channels' rows
// makes the answer depend on the order the query returned them: #staff's deny lands after #general's
// allow and takes viewing away from a channel nobody denied. Merged tiers do not resolve to one wrong
// answer, they resolve to whichever wrong answer the row order picks.
func TestAGuildWideSliceResolvesEachChannelSeparately(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()

	owner := f.newUser(ctx, "owner")
	member := f.newUser(ctx, "member")
	guildID, everyoneID := f.newGuild(ctx, owner, roles.PermViewChannel|roles.PermSendMessages)
	f.join(ctx, guildID, member)

	general := f.newChannel(ctx, guildID)
	staff := f.newChannel(ctx, guildID)
	quiet := f.newChannel(ctx, guildID)

	// #general re-allows what @everyone already grants; #staff denies viewing; #quiet denies sending only
	// to this one member. Three channels, three different tiers, one slice.
	f.overwrite(ctx, general, roles.OverwriteTargetRole, everyoneID, roles.PermViewChannel, 0)
	f.overwrite(ctx, staff, roles.OverwriteTargetRole, everyoneID, 0, roles.PermViewChannel)
	f.overwrite(ctx, quiet, roles.OverwriteTargetMember, member, 0, roles.PermSendMessages)

	res, err := roles.Resolve(ctx, f.q, guildID, member, 0)
	require.NoError(t, err)

	all, err := f.q.ListGuildPermissionOverwrites(ctx, db.ListGuildPermissionOverwritesParams{
		ChannelIds: []int64{int64(general), int64(staff), int64(quiet)},
		GuildID:    int64(guildID),
	})
	require.NoError(t, err)
	require.Len(t, all, 3, "one query returns every channel's rows, which is the point of it")

	require.True(t, res.InChannel(general, all).Has(roles.PermViewChannel),
		"#general is visible")
	require.False(t, res.InChannel(staff, all).Has(roles.PermViewChannel),
		"#staff must stay hidden — #general's allow belongs to #general and must not reach this evaluation")
	require.True(t, res.InChannel(quiet, all).Has(roles.PermViewChannel),
		"#quiet is visible; only sending is denied there")
	require.False(t, res.InChannel(quiet, all).Has(roles.PermSendMessages),
		"the member-tier deny applies in its own channel")
	require.True(t, res.InChannel(general, all).Has(roles.PermSendMessages),
		"and nowhere else — a deny in one channel must not follow the loop into the others")
}

// TestAnOverwriteForAnotherGuildIsNotReturned pins the scoping on the guild-wide read.
//
// The ids come from this guild's own channel listing, so the guild predicate is redundant against every
// caller that exists — which is exactly the reasoning this milestone rejected once already, after
// reproducing overwrite rows crossing between guilds through a statement whose caller happened to be safe.
func TestAnOverwriteForAnotherGuildIsNotReturned(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()

	owner := f.newUser(ctx, "owner")
	mine, myEveryone := f.newGuild(ctx, owner, roles.PermViewChannel)
	theirs, theirEveryone := f.newGuild(ctx, owner, roles.PermViewChannel)

	myChannel := f.newChannel(ctx, mine)
	theirChannel := f.newChannel(ctx, theirs)
	f.overwrite(ctx, myChannel, roles.OverwriteTargetRole, myEveryone, 0, roles.PermSendMessages)
	f.overwrite(ctx, theirChannel, roles.OverwriteTargetRole, theirEveryone, 0, roles.PermSendMessages)

	got, err := f.q.ListGuildPermissionOverwrites(ctx, db.ListGuildPermissionOverwritesParams{
		ChannelIds: []int64{int64(myChannel), int64(theirChannel)},
		GuildID:    int64(mine),
	})
	require.NoError(t, err)
	require.Len(t, got, 1, "a channel id from another guild contributes nothing")
	require.Equal(t, int64(myChannel), got[0].ChannelID)
}
