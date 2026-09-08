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
		require.True(t, got.Has(roles.PermBanMembers|roles.PermManageGuild),
			"the owner must hold every permission, got %d", got)
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
		require.True(t, got.Has(roles.PermBanMembers|roles.PermManageGuild),
			"an administrator must hold every permission, got %d", got)
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
		require.Equal(t, want, got, "expected the union of @everyone and both roles")
		require.False(t, got.Has(roles.PermBanMembers), "no role granted PermBanMembers")
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
		require.Equal(t, roles.PermViewChannel, got)
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
		require.False(t, got.Has(roles.PermSendMessages), "the @everyone deny must apply")
		require.True(t, got.Has(roles.PermViewChannel), "it must not remove what it did not deny")
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
		require.True(t, got.Has(roles.PermSendMessages),
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
		require.True(t, got.Has(roles.PermSendMessages), "the member overwrite is the most specific tier")
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
		require.True(t, got.Has(roles.PermSendMessages),
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
		require.True(t, got.Has(roles.PermSendMessages),
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
		require.True(t, got.Has(roles.PermSendMessages))
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
	require.True(t, got.Has(roles.PermSendMessages),
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
	require.True(t, guildLevel.Has(roles.PermSendMessages), "a guild-level check must ignore channel overwrites")

	inChannel, err := roles.Resolve(ctx, f.q, guildID, member, channelID)
	require.NoError(t, err)
	require.False(t, inChannel.Has(roles.PermSendMessages), "the same check in the channel must see the deny")
}
