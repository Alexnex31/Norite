// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guildauth_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/guildauth"
	"github.com/Alexnex31/Norite/backend/internal/platform/database"
	"github.com/Alexnex31/Norite/backend/internal/platform/dbtest"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
	"github.com/Alexnex31/Norite/backend/migrations"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

// The package this was extracted from still owns the behavioral coverage, and that is not an oversight:
// the M12–M14 suite drives these functions through real endpoints and is the oracle the M15 refactor was
// measured against. What that suite cannot do is fail when *this* package's contract changes without a
// caller noticing, because it reaches everything through `guilds`.
//
// So these tests are deliberately narrow. They assert the properties a future importer — `messages`,
// `reports`, `tags`, `whispers` — would otherwise have to rediscover by reading `guilds`: that the two
// refusals are distinguishable and which is which, and that the locking and non-locking entry points
// really are two different queries rather than one with a misleading name.

// queries opens a migrated database and returns the generated querier over it.
func queries(t *testing.T) (*db.Queries, *pgxpool.Pool, context.Context) {
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

	return db.New(pool), pool, ctx
}

// TestTheTwoRefusalsAreDistinguishableAndDocumented is the anti-enumeration contract, asserted from
// outside the package that used to own it.
//
// A stranger to the guild gets 404 and never 403, because guild ids are snowflakes and a 404/403 split
// turns a list of plausible ids into a map of which guilds exist. An importer that reversed these would
// reopen the oracle M11 closed for session ids, and nothing in `guilds` would fail.
func TestTheTwoRefusalsAreDistinguishableAndDocumented(t *testing.T) {
	t.Parallel()

	q, _, ctx := queries(t)

	// Nobody is a member of a guild that does not exist, so this is the non-member path.
	_, err := guildauth.Authorize(ctx, q, auth.Actor{UserID: 1}, snowflake.ID(999), 0, roles.PermViewChannel)
	require.ErrorIs(t, err, httpx.ErrNotFound,
		"a non-member must be refused as not-found, never as forbidden — the 403 would confirm the guild exists")
}

// TestTheUnlockedEntryPointDoesNotLockTheChannelRow pins the distinction M15 added, and it is the one thing
// here that a behavioral test in `guilds` cannot cover: both entry points return the same answers, so
// only the lock tells them apart.
//
// Without it the backlog read — the hottest read in the product — would take an exclusive row lock on the
// channel per page, serializing two members scrolling the same channel.
func TestTheUnlockedEntryPointDoesNotLockTheChannelRow(t *testing.T) {
	t.Parallel()

	_, pool, ctx := queries(t)

	// A real guild, channel and owner, inserted directly — this test is about the lock, not about the
	// service that would normally create them.
	const owner, guild, channel = 8001, 8002, 8003
	mustExec(t, ctx, pool, `INSERT INTO users (id, username, email, display_name, created_at, updated_at)
	                        VALUES ($1,'lockowner','lock@example.test','Lock Owner',now(),now())`, owner)
	mustExec(t, ctx, pool, `INSERT INTO guilds (id, name, owner_id, created_at, updated_at)
	                        VALUES ($1,'lock guild',$2,now(),now())`, guild, owner)
	mustExec(t, ctx, pool, `INSERT INTO channels (id, guild_id, name, type, position, created_at, updated_at)
	                        VALUES ($1,$2,'general',0,0,now(),now())`, channel, guild)
	mustExec(t, ctx, pool, `INSERT INTO roles (id, guild_id, name, permissions, position, is_default,
	                                           created_at, updated_at)
	                        VALUES ($1,$2,'@everyone',$3,0,true,now(),now())`,
		guild+1, guild, int64(roles.PermViewChannel))
	mustExec(t, ctx, pool, `INSERT INTO guild_members (guild_id, user_id, joined_at)
	                        VALUES ($1,$2,now())`, guild, owner)

	actor := auth.Actor{UserID: snowflake.ID(owner)}

	// held reports whether the channel row is locked against another connection while fn runs in a
	// transaction. NOWAIT turns "somebody holds it" into an immediate error rather than a wait, so this
	// cannot pass or fail on timing.
	held := func(fn func(q *db.Queries) error) bool {
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()

		require.NoError(t, fn(db.New(pool).WithTx(tx)))

		_, lockErr := pool.Exec(ctx, `SELECT 1 FROM channels WHERE id = $1 FOR UPDATE NOWAIT`, channel)
		return lockErr != nil
	}

	lockedByWrite := held(func(q *db.Queries) error {
		_, _, _, err := guildauth.AuthorizeChannel(ctx, q, actor, snowflake.ID(channel), roles.PermViewChannel)
		return err
	})
	lockedByRead := held(func(q *db.Queries) error {
		_, _, _, err := guildauth.AuthorizeChannelUnlocked(ctx, q, actor, snowflake.ID(channel), roles.PermViewChannel)
		return err
	})

	require.True(t, lockedByWrite,
		"AuthorizeChannel must hold the row lock the diff race depends on — see its FOR UPDATE section")
	require.False(t, lockedByRead,
		"AuthorizeChannelUnlocked must not lock: the backlog fetch runs it per page, and a row lock there "+
			"serializes every member reading the same channel")
}

// TestIgnoringVisibilityLiftsTheRequirementAndNotTheRefusal is M16a's central guard, and it is here rather
// than only in `messages` because the property it pins belongs to this package's contract.
//
// [guildauth.AuthorizeChannelIgnoringVisibility] does two things that sound like one. It stops *requiring*
// PermViewChannel, so a moderator denied view still reaches content M16 already decided they may read. It
// keeps the refusal *downgrade*, so somebody who fails and cannot view is answered 404 rather than 403 —
// without which any member could probe for hidden channels in their own guild, the oracle M14 closed.
//
// The third assertion is the one worth having. Dropping the downgrade alongside the fold reads as a
// two-line simplification, leaves the first two assertions passing, and is a disclosure.
func TestIgnoringVisibilityLiftsTheRequirementAndNotTheRefusal(t *testing.T) {
	t.Parallel()

	q, pool, ctx := queries(t)

	// A moderator who holds PermManageMessages and is denied PermViewChannel on the one channel — the state
	// M13's bug and M16's manual pass both needed, and which no happy path constructs. The actor is never
	// the owner: layer 2 short-circuits permission resolution, so an owner would pass every assertion here
	// without exercising a single one of them.
	const owner, moderator, plain = 9001, 9002, 9003
	const guild, channel, everyone, modRole = 9010, 9011, 9012, 9013

	for id, name := range map[int]string{owner: "iv-owner", moderator: "iv-mod", plain: "iv-plain"} {
		mustExec(t, ctx, pool, `INSERT INTO users (id, username, email, display_name, created_at, updated_at)
		                        VALUES ($1,$2,$3,$4,now(),now())`, id, name, name+"@example.test", name)
	}
	mustExec(t, ctx, pool, `INSERT INTO guilds (id, name, owner_id, created_at, updated_at)
	                        VALUES ($1,'iv guild',$2,now(),now())`, guild, owner)
	mustExec(t, ctx, pool, `INSERT INTO channels (id, guild_id, name, type, position, created_at, updated_at)
	                        VALUES ($1,$2,'general',0,0,now(),now())`, channel, guild)
	mustExec(t, ctx, pool, `INSERT INTO roles (id, guild_id, name, permissions, position, is_default,
	                                           created_at, updated_at)
	                        VALUES ($1,$2,'@everyone',$3,0,true,now(),now())`,
		everyone, guild, int64(roles.PermViewChannel))
	mustExec(t, ctx, pool, `INSERT INTO roles (id, guild_id, name, permissions, position, is_default,
	                                           created_at, updated_at)
	                        VALUES ($1,$2,'moderator',$3,1,false,now(),now())`,
		modRole, guild, int64(roles.PermManageMessages))
	for _, id := range []int{owner, moderator, plain} {
		mustExec(t, ctx, pool, `INSERT INTO guild_members (guild_id, user_id, joined_at)
		                        VALUES ($1,$2,now())`, guild, id)
	}
	mustExec(t, ctx, pool, `INSERT INTO guild_member_roles (guild_id, user_id, role_id)
	                        VALUES ($1,$2,$3)`, guild, moderator, modRole)

	// The deny is member-tier, which is the tier a role cannot lift: this is what "denied VIEW_CHANNEL"
	// means in practice and what M16's blindmod account was built with.
	for _, id := range []int{moderator, plain} {
		mustExec(t, ctx, pool, `INSERT INTO permission_overwrites (channel_id, target_type, target_id,
		                                                           allow, deny)
		                        VALUES ($1,$2,$3,0,$4)`,
			channel, roles.OverwriteTargetMember, id, int64(roles.PermViewChannel))
	}

	modActor := auth.Actor{UserID: snowflake.ID(moderator)}
	plainActor := auth.Actor{UserID: snowflake.ID(plain)}

	_, _, _, err := guildauth.AuthorizeChannelUnlocked(
		ctx, q, modActor, snowflake.ID(channel), roles.PermManageMessages)
	require.ErrorIs(t, err, httpx.ErrNotFound,
		"the ordinary entry point must still fold in PermViewChannel and refuse as not-found — if this "+
			"passes, the fold has been lost for every caller, not only for this one")

	_, _, decision, err := guildauth.AuthorizeChannelIgnoringVisibility(
		ctx, q, modActor, snowflake.ID(channel), roles.PermManageMessages)
	require.NoError(t, err,
		"a PermManageMessages holder denied only PermViewChannel must reach the content M16 already "+
			"decided they may read — otherwise a moderator can read a reported message and not its history")
	require.True(t, decision.Allows(roles.PermManageMessages))

	_, _, _, err = guildauth.AuthorizeChannelIgnoringVisibility(
		ctx, q, plainActor, snowflake.ID(channel), roles.PermManageMessages)
	require.ErrorIs(t, err, httpx.ErrNotFound,
		"a member who can neither view the channel nor moderate it must get 404 and never 403: the "+
			"downgrade is a separate property from the fold, and a 403 here is a probe for hidden channels")
}

func mustExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	_, err := pool.Exec(ctx, sql, args...)
	require.NoError(t, err)
}
