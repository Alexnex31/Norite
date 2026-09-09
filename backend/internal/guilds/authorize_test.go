// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"context"
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
	t    *testing.T
	svc  *Service
	pool *pgxpool.Pool
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

	svc, err := NewService(ServiceOptions{
		Pool: pool, IDs: ids,
		MaxChannelsPerGuild: 500, MaxRolesPerGuild: 250, MaxGuildsPerAccount: 50,
	})
	require.NoError(t, err)

	return &fixture{t: t, svc: svc, pool: pool}
}

func (f *fixture) next() snowflake.ID {
	f.t.Helper()
	id, err := f.svc.ids.Next()
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

func (f *fixture) makeInstanceAdmin(ctx context.Context, userID snowflake.ID) {
	f.exec(ctx, `INSERT INTO instance_admins (user_id) VALUES ($1)`, int64(userID))
}

// newGuild creates a guild, its @everyone role carrying everyonePerms, and the owner's membership.
func (f *fixture) newGuild(ctx context.Context, owner snowflake.ID, everyonePerms roles.Permission,
) snowflake.ID {
	guildID, everyoneID := f.next(), f.next()

	f.exec(ctx, `INSERT INTO guilds (id, name, owner_id) VALUES ($1, 'test guild', $2)`,
		int64(guildID), int64(owner))
	f.exec(ctx, `INSERT INTO roles (id, guild_id, name, position, is_default, permissions)
	             VALUES ($1, $2, '@everyone', 0, true, $3)`,
		int64(everyoneID), int64(guildID), everyonePerms.Int64())
	f.exec(ctx, `INSERT INTO guild_members (guild_id, user_id) VALUES ($1, $2)`,
		int64(guildID), int64(owner))

	return guildID
}

func (f *fixture) join(ctx context.Context, guildID, userID snowflake.ID) {
	f.exec(ctx, `INSERT INTO guild_members (guild_id, user_id) VALUES ($1, $2)`,
		int64(guildID), int64(userID))
}

func userActor(id snowflake.ID) auth.Actor {
	return auth.Actor{Kind: auth.ActorUser, UserID: id}
}

// TestAnInstanceAdminIsNotResolvedThroughRoles is ADR 0008's layer-1 separation, asserted from both sides.
//
// The admin is deliberately *not* a member of the guild. Resolve must refuse them — they hold no
// membership, so there is nothing for it to resolve — and authorize must permit them anyway, because the
// instance tier is checked before the guild is consulted at all.
//
// This is the test that fails if somebody folds the tier check inside Resolve, which is the simplification
// ADR 0008 rejects by name. It would also fail if authorize resolved first and only checked the tier as a
// fallback, since a non-member's refusal arrives as ErrNotFound before any tier is consulted.
func TestAnInstanceAdminIsNotResolvedThroughRoles(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()

	owner := f.newUser(ctx, "owner")
	admin := f.newUser(ctx, "admin")
	f.makeInstanceAdmin(ctx, admin)

	guildID := f.newGuild(ctx, owner, roles.PermViewChannel)

	t.Run("roles.Resolve refuses them, because they are not a member", func(t *testing.T) {
		_, err := roles.Resolve(ctx, db.New(f.pool), guildID, admin, 0)
		require.ErrorIs(t, err, roles.ErrNotAMember,
			"an Instance Admin holds no membership and must not resolve to guild permissions")
	})

	t.Run("authorize permits them, because layer 1 sits above the guild", func(t *testing.T) {
		require.NoError(t, f.svc.authorize(ctx, userActor(admin), guildID, 0, roles.PermManageGuild))
	})
}

func TestAuthorizeRefusesTheTwoCasesDifferently(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()

	owner := f.newUser(ctx, "owner")
	member := f.newUser(ctx, "member")
	stranger := f.newUser(ctx, "stranger")

	// @everyone grants viewing and nothing else, so a member is a member and still cannot manage.
	guildID := f.newGuild(ctx, owner, roles.PermViewChannel)
	f.join(ctx, guildID, member)

	t.Run("a member lacking the permission gets forbidden", func(t *testing.T) {
		err := f.svc.authorize(ctx, userActor(member), guildID, 0, roles.PermManageGuild)
		require.ErrorIs(t, err, httpx.ErrForbidden,
			"they already know the guild exists, so 403 discloses nothing")
	})

	t.Run("a member holding the permission is allowed", func(t *testing.T) {
		require.NoError(t, f.svc.authorize(ctx, userActor(member), guildID, 0, roles.PermViewChannel))
	})

	// The two cases below must be indistinguishable. Guild ids are snowflakes — sequential, and carrying
	// their own creation time — so a 404/403 split turns a list of plausible ids into a map of which
	// guilds exist. M11 settled this for session ids.
	t.Run("a non-member gets not-found", func(t *testing.T) {
		err := f.svc.authorize(ctx, userActor(stranger), guildID, 0, roles.PermViewChannel)
		require.ErrorIs(t, err, httpx.ErrNotFound)
	})

	t.Run("a guild that does not exist gets the same not-found", func(t *testing.T) {
		err := f.svc.authorize(ctx, userActor(stranger), f.next(), 0, roles.PermViewChannel)
		require.ErrorIs(t, err, httpx.ErrNotFound)
	})
}

func TestTheOwnerIsAuthorizedForEverything(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()

	owner := f.newUser(ctx, "owner")
	guildID := f.newGuild(ctx, owner, 0)

	require.NoError(t, f.svc.authorize(ctx, userActor(owner), guildID, 0,
		roles.PermManageGuild|roles.PermBanMembers))
}

// TestAuthorizeCanRunInsideACallersTransaction covers the reason authorizeWith takes a querier.
//
// Rule 1 asks for resolution against data freshly loaded for the request. A mutation that authorizes on
// the pool and then opens a transaction to write reads a snapshot from before its own BEGIN, so a
// demotion committed in between would be missed. Running the check on the transaction closes that window,
// and this asserts the plumbing works — a permission granted inside an uncommitted transaction is visible
// to a check made on that same transaction, and to nothing else.
func TestAuthorizeCanRunInsideACallersTransaction(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()

	owner := f.newUser(ctx, "owner")
	member := f.newUser(ctx, "member")
	guildID := f.newGuild(ctx, owner, roles.PermViewChannel)
	f.join(ctx, guildID, member)

	// Outside any transaction, the member cannot manage the guild.
	require.ErrorIs(t, f.svc.authorize(ctx, userActor(member), guildID, 0, roles.PermManageGuild),
		httpx.ErrForbidden)

	tx, err := f.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `UPDATE roles SET permissions = $1 WHERE guild_id = $2 AND is_default`,
		roles.PermManageGuild.Int64(), int64(guildID))
	require.NoError(t, err)

	_, err = authorizeWith(ctx, f.svc.queries.WithTx(tx), userActor(member), guildID, 0,
		roles.PermManageGuild)
	require.NoError(t, err,
		"a check on the caller's transaction must see the caller's own uncommitted grant")

	require.ErrorIs(t, f.svc.authorize(ctx, userActor(member), guildID, 0, roles.PermManageGuild),
		httpx.ErrForbidden,
		"and a check on the pool must not")
}

// TestTheCeilingsComeFromConfiguration pins that the creation limits are settings, not constants.
//
// They were constants until a review asked whether a self-hoster could change them. The failure this
// guards is somebody reintroducing a package-level constant "for clarity": the test builds a service with
// a ceiling of one and requires it to bite, which no hardcoded 500 can satisfy.
func TestTheCeilingsComeFromConfiguration(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()

	// A service whose ceilings are 1, rather than the shipped 500/250/50.
	tiny, err := NewService(ServiceOptions{
		Pool: f.pool, IDs: f.svc.ids,
		MaxChannelsPerGuild: 1, MaxRolesPerGuild: 1, MaxGuildsPerAccount: 1,
	})
	require.NoError(t, err)

	owner := f.newUser(ctx, "owner")
	actor := userActor(owner)

	first, err := tiny.Create(ctx, actor, CreateGuildInput{Name: "first"})
	require.NoError(t, err)

	_, err = tiny.Create(ctx, actor, CreateGuildInput{Name: "second"})
	require.ErrorIs(t, err, ErrGuildFull, "a ceiling of one guild must refuse the second")

	// The guild it did create already holds its @everyone role, so the role ceiling is reached too.
	_, err = tiny.CreateRole(ctx, actor, first.ID, CreateRoleInput{Name: "extra"})
	require.ErrorIs(t, err, ErrGuildFull, "a ceiling of one role must refuse the second")

	_, err = tiny.CreateChannel(ctx, actor, first.ID,
		CreateChannelInput{Name: "general", Type: ChannelGuildText})
	require.NoError(t, err, "the first channel is within a ceiling of one")

	_, err = tiny.CreateChannel(ctx, actor, first.ID,
		CreateChannelInput{Name: "second", Type: ChannelGuildText})
	require.ErrorIs(t, err, ErrGuildFull, "a ceiling of one channel must refuse the second")
}

// TestAZeroCeilingIsRefusedAtConstruction fails at startup rather than at the first create.
//
// A zero ceiling refuses every creation with a conflict, which reads as a bug in the endpoint rather than
// as an unset setting — and an unset setting is exactly what it would be.
func TestAZeroCeilingIsRefusedAtConstruction(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	_, err := NewService(ServiceOptions{
		Pool: f.pool, IDs: f.svc.ids,
		MaxChannelsPerGuild: 0, MaxRolesPerGuild: 250, MaxGuildsPerAccount: 50,
	})
	require.Error(t, err, "a zero ceiling must be refused before the service exists")
	require.Contains(t, err.Error(), "ceiling")
}

// TestDeletingAGuildThatDoesNotExistAnswers404 covers the one refusal an Instance Admin reaches that no
// guild resolution can produce.
//
// Every other caller of Delete is refused by authorizeWith, which resolves the guild and answers 404 when
// it finds nothing. An Instance Admin is not resolved at all — layer 1 short-circuits above roles.Resolve
// by design, because the tier acts on guilds it is not in — so for that one actor the guild's existence is
// never established, and the first thing to touch it is the audit write. Without an explicit check that
// is a foreign-key violation and a 500, where every other actor gets 404.
//
// M12 got this right by accident: Delete opened with a GetGuild whose ErrNoRows branch answered 404, and
// the owner comparison happened to need the same row. M13 removed that read, because the owner id now
// arrives with the resolution — and removed the existence check with it.
func TestDeletingAGuildThatDoesNotExistAnswers404(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	ctx := t.Context()

	admin := f.newUser(ctx, "admin")
	f.makeInstanceAdmin(ctx, admin)

	err := f.svc.Delete(ctx, userActor(admin), f.next())
	require.ErrorIs(t, err, httpx.ErrNotFound,
		"an Instance Admin deleting a guild that is not there gets the same 404 as anybody else")
}
