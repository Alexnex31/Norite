// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package coordinator

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/backend/internal/dispatch"
	"github.com/Alexnex31/Norite/backend/internal/platform/database"
	"github.com/Alexnex31/Norite/backend/internal/platform/dbtest"
	"github.com/Alexnex31/Norite/backend/internal/platform/ratelimit"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
	"github.com/Alexnex31/Norite/backend/internal/voice"
	"github.com/Alexnex31/Norite/backend/internal/voice/voicetest"
	"github.com/Alexnex31/Norite/backend/migrations"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

type fixture struct {
	c     *Coordinator
	sfu   *voice.SFU
	pool  *pgxpool.Pool
	ctx   context.Context
	ids   *snowflake.Generator
	guild snowflake.ID
	// channel is a voice channel; everyone is the guild's @everyone role.
	channel, everyone snowflake.ID
	owner, member     snowflake.ID
}

// voiceGrant is what @everyone holds here: enough to see, join and speak in the channel.
const voiceGrant = roles.PermViewChannel | roles.PermConnectVoice | roles.PermSpeakVoice

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

	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	sfu, err := voice.New(voice.Options{Conn: conn, Loopback: true, Version: gatewayproto.DevVersion,
		Logger: zerolog.Nop()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sfu.Close() })

	// No bus: the tests deliver re-checks themselves, so they know when one has been taken.
	c, err := New(Options{Pool: pool, SFU: sfu, IDs: ids, Events: dispatch.NewPublisher(nil, nil),
		RateLimitBackend: ratelimit.MemoryBackend(), Logger: zerolog.Nop()})
	require.NoError(t, err)
	t.Cleanup(c.Close)

	f := &fixture{c: c, sfu: sfu, pool: pool, ctx: ctx, ids: ids}
	f.owner, f.member, f.guild, f.channel, f.everyone = ids.MustNext(), ids.MustNext(), ids.MustNext(),
		ids.MustNext(), ids.MustNext()
	for _, u := range []struct {
		id   snowflake.ID
		name string
	}{{f.owner, "owner"}, {f.member, "member"}} {
		f.exec(t, `INSERT INTO users (id, username, email, display_name, created_at, updated_at)
		           VALUES ($1,$2::text,$2::text||'@example.test',$2::text,now(),now())`, int64(u.id), u.name)
	}
	f.exec(t, `INSERT INTO guilds (id, name, owner_id, created_at, updated_at) VALUES ($1,'g',$2,now(),now())`,
		int64(f.guild), int64(f.owner))
	f.exec(t, `INSERT INTO channels (id, guild_id, name, type, position, created_at, updated_at)
	           VALUES ($1,$2,'lounge',2,0,now(),now())`, int64(f.channel), int64(f.guild))
	f.exec(t, `INSERT INTO roles (id, guild_id, name, permissions, position, is_default, created_at, updated_at)
	           VALUES ($1,$2,'@everyone',$3,0,true,now(),now())`, int64(f.everyone), int64(f.guild),
		voiceGrant.Int64())
	for _, u := range []snowflake.ID{f.owner, f.member} {
		f.exec(t, `INSERT INTO guild_members (guild_id, user_id, joined_at) VALUES ($1,$2,now())`,
			int64(f.guild), int64(u))
	}
	return f
}

func (f *fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	_, err := f.pool.Exec(f.ctx, sql, args...)
	require.NoError(t, err)
}

func (f *fixture) offer(t *testing.T) string {
	t.Helper()
	c, err := voicetest.New(voicetest.Options{Slots: 1, Loopback: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sdp, err := c.Offer(ctx)
	require.NoError(t, err)
	return sdp
}

func (f *fixture) join(t *testing.T, user snowflake.ID, session string, sdp string) (string, any) {
	t.Helper()
	ch, video := f.channel.String(), false
	return f.c.Join(f.ctx, user, session, "device-"+session, gatewayproto.VoiceStateUpdate{
		Nonce: "n", ChannelID: &ch, Offer: sdp, SupportsVideo: &video,
	})
}

// recheck delivers a re-check of the fixture's guild and waits for it to have run.
func (f *fixture) recheck(t *testing.T) {
	t.Helper()
	payload, err := json.Marshal(dispatch.VoiceRecheck{GuildID: f.guild})
	require.NoError(t, err)
	f.c.onRecheck(payload)
	require.Eventually(t, func() bool {
		f.c.mu.Lock()
		defer f.c.mu.Unlock()
		return f.c.rechecks[f.guild] == nil
	}, 10*time.Second, 5*time.Millisecond)
}

func (f *fixture) rows(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT count(*) FROM voice_states`).Scan(&n))
	return n
}

// A permission change committed after a join was authorized, and before it was provisioned, is seen by the
// re-check it publishes: the join is registered before it is authorized, and the re-check's cutoff refuses
// the provision. Staged with a hook between the two, never raced.
func TestARecheckLandingMidJoinRefusesTheProvision(t *testing.T) {
	f := newFixture(t)
	sdp := f.offer(t)

	authorized, release := make(chan struct{}), make(chan struct{})
	beforeProvision = func() {
		close(authorized)
		<-release
	}
	t.Cleanup(func() { beforeProvision = nil })

	type reply struct {
		event   string
		payload any
	}
	done := make(chan reply, 1)
	go func() {
		event, payload := f.join(t, f.member, "s1", sdp)
		done <- reply{event, payload}
	}()
	<-authorized

	f.exec(t, `UPDATE roles SET permissions = $1 WHERE id = $2`,
		(voiceGrant &^ roles.PermConnectVoice).Int64(), int64(f.everyone))
	f.recheck(t)
	close(release)

	r := <-done
	require.Equal(t, gatewayproto.EventVoiceJoinRefused, r.event, "%+v", r.payload)
	assert.Equal(t, gatewayproto.VoiceForbidden, r.payload.(gatewayproto.VoiceJoinRefused).Code)
	assert.Zero(t, f.rows(t), "a refused join left a voice state")
}

// An Instance Admin who is not a member passes the channel's authorization on layer 1 and is refused the
// call, as the re-check would evict them: a call is not an act rule 14 records.
func TestAnInstanceAdminWhoIsNoMemberIsRefusedTheCall(t *testing.T) {
	f := newFixture(t)
	admin := f.ids.MustNext()
	f.exec(t, `INSERT INTO users (id, username, email, display_name, created_at, updated_at)
	           VALUES ($1,'admin','admin@example.test','admin',now(),now())`, int64(admin))
	f.exec(t, `INSERT INTO instance_admins (user_id) VALUES ($1)`, int64(admin))
	event, payload := f.join(t, admin, "s1", f.offer(t))
	require.Equal(t, gatewayproto.EventVoiceJoinRefused, event, "%+v", payload)
	assert.Equal(t, gatewayproto.VoiceForbidden, payload.(gatewayproto.VoiceJoinRefused).Code)
}

// A session's end and a join still under way: the join ends what it provisioned rather than leave a call
// nobody holds.
func TestASessionEndedMidJoinLeavesNoCall(t *testing.T) {
	f := newFixture(t)
	sdp := f.offer(t)
	authorized, release := make(chan struct{}), make(chan struct{})
	beforeProvision = func() {
		close(authorized)
		<-release
	}
	t.Cleanup(func() { beforeProvision = nil })
	done := make(chan string, 1)
	go func() {
		event, _ := f.join(t, f.member, "s1", sdp)
		done <- event
	}()
	<-authorized
	f.c.Ended(f.member, "s1")
	close(release)
	assert.Equal(t, gatewayproto.EventVoiceJoinRefused, <-done)
	require.Eventually(t, func() bool { return f.rows(t) == 0 }, 5*time.Second, 10*time.Millisecond)
	f.c.mu.Lock()
	defer f.c.mu.Unlock()
	assert.Empty(t, f.c.calls)
	assert.Empty(t, f.c.inGuild, "the guild still counts a call this process does not hold")
}

// A re-check of a guild with no call here reads nothing, and re-checks of a guild coalesce.
func TestRechecksOfAGuildCoalesce(t *testing.T) {
	f := newFixture(t)
	payload, err := json.Marshal(dispatch.VoiceRecheck{GuildID: f.guild})
	require.NoError(t, err)
	f.c.onRecheck(payload)
	f.c.mu.Lock()
	assert.Empty(t, f.c.rechecks, "a guild with no call here was re-checked")
	f.c.mu.Unlock()

	event, payloadJoin := f.join(t, f.member, "s1", f.offer(t))
	require.Equal(t, gatewayproto.EventVoiceServerUpdate, event, "%+v", payloadJoin)
	f.c.mu.Lock()
	f.c.rechecks[f.guild] = &recheckState{} // one running
	f.c.mu.Unlock()
	for range 50 {
		f.c.onRecheck(payload)
	}
	f.c.mu.Lock()
	st := f.c.rechecks[f.guild]
	f.c.mu.Unlock()
	assert.True(t, st.again, "a re-check arriving while one runs is not kept for after it")
}

// Two joins that both read one place left under user_limit cannot both take it: the row is written under
// the channel's lock, and counted again there. Both are held after the first count, then let go together.
func TestTwoJoinsRacingForTheLastPlaceGetOne(t *testing.T) {
	f := newFixture(t)
	f.exec(t, `UPDATE channels SET user_limit = 1 WHERE id = $1`, int64(f.channel))
	offers := []string{f.offer(t), f.offer(t)}

	held, release := make(chan struct{}, 2), make(chan struct{})
	beforeProvision = func() {
		held <- struct{}{}
		<-release
	}
	t.Cleanup(func() { beforeProvision = nil })
	events := make(chan string, 2)
	for i, user := range []snowflake.ID{f.owner, f.member} {
		go func() {
			event, _ := f.join(t, user, "s"+string(rune('a'+i)), offers[i])
			events <- event
		}()
	}
	<-held
	<-held
	close(release)
	got := []string{<-events, <-events}
	assert.ElementsMatch(t, []string{gatewayproto.EventVoiceServerUpdate, gatewayproto.EventVoiceJoinRefused}, got)
	assert.Equal(t, 1, f.rows(t))
}
