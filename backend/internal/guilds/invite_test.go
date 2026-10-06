// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// inviteFixture is a guild whose @everyone may view and talk but not invite, an inviter holding
// PermCreateInvite through a role, an ordinary member, and an outsider with an account and no membership.
type inviteFixture struct {
	*fixture
	guildID, channelID, everyoneID   snowflake.ID
	owner, inviter, member, outsider snowflake.ID
}

func newInviteFixture(t *testing.T) *inviteFixture {
	t.Helper()
	f := newFixture(t)
	ctx := t.Context()

	owner, inviter := f.newUser(ctx, "owner"), f.newUser(ctx, "inviter")
	member, outsider := f.newUser(ctx, "member"), f.newUser(ctx, "outsider")
	guildID, everyoneID := f.newGuildWithEveryone(ctx, owner,
		roles.PermViewChannel|roles.PermSendMessages|roles.PermReadMessageHistory)
	f.join(ctx, guildID, inviter)
	f.join(ctx, guildID, member)
	f.grantRole(ctx, guildID, inviter, f.newRole(ctx, guildID, 1, roles.PermCreateInvite))

	return &inviteFixture{
		fixture: f, guildID: guildID, channelID: f.newChannel(ctx, guildID), everyoneID: everyoneID,
		owner: owner, inviter: inviter, member: member, outsider: outsider,
	}
}

func (f *inviteFixture) invite(t *testing.T, in CreateInviteInput) Invite {
	t.Helper()
	if in.ChannelID == 0 {
		in.ChannelID = f.channelID
	}
	inv, err := f.svc.CreateInvite(t.Context(), userActor(f.inviter), in)
	require.NoError(t, err)
	return inv
}

func (f *inviteFixture) isMember(t *testing.T, userID snowflake.ID) bool {
	t.Helper()
	var in bool
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM guild_members WHERE guild_id = $1 AND user_id = $2)`,
		int64(f.guildID), int64(userID)).Scan(&in))
	return in
}

// TestAnOutsiderJoinsThroughAnInvite is the milestone's reason for invites: somebody with no way into a
// guild previews a code, redeems it, and is a member of the guild it named.
func TestAnOutsiderJoinsThroughAnInvite(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := t.Context()
	inv := f.invite(t, CreateInviteInput{})
	require.Len(t, inv.Code, 16)
	require.NotNil(t, inv.Inviter)
	assert.Equal(t, "inviter", inv.Inviter.Username)

	// As a person pastes it: lower case, with a dash and spaces. M10's normalization, unchanged.
	typed := " " + strings.ToLower(inv.Code[:8]) + "-" + inv.Code[8:] + " "
	preview, err := f.svc.PreviewInvite(ctx, typed)
	require.NoError(t, err)
	assert.Equal(t, f.guildID, preview.Guild.ID)
	assert.Equal(t, "test guild", preview.Guild.Name)
	assert.Equal(t, f.channelID, preview.Channel.ID)
	require.NotNil(t, preview.Inviter)
	assert.Equal(t, f.inviter, preview.Inviter.ID)
	assert.Nil(t, preview.ExpiresAt, "an invite asked for with no limit never expires")

	guild, err := f.svc.RedeemInvite(ctx, userActor(f.outsider), typed)
	require.NoError(t, err)
	assert.Equal(t, f.guildID, guild.ID)
	assert.True(t, f.isMember(t, f.outsider))
}

// TestEveryDeadCodeAnswersAsAnUnknownOne: unknown, malformed, expired, revoked and used up are one answer on
// all three routes that take a code, so none of them tells somebody without a valid code which codes once
// existed. Asserted as the very error value, because a 404 carrying a different message is a second answer.
func TestEveryDeadCodeAnswersAsAnUnknownOne(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := t.Context()

	expired := f.invite(t, CreateInviteInput{MaxAge: time.Hour})
	f.exec(ctx, `UPDATE invites SET expires_at = now() - interval '1 second' WHERE id = $1`, int64(expired.ID))
	revoked := f.invite(t, CreateInviteInput{})
	require.NoError(t, f.svc.RevokeInvite(ctx, userActor(f.inviter), revoked.Code))
	usedUp := f.invite(t, CreateInviteInput{MaxUses: 1})
	_, err := f.svc.RedeemInvite(ctx, userActor(f.outsider), usedUp.Code)
	require.NoError(t, err)

	stranger := f.newUser(ctx, "stranger")
	for name, code := range map[string]string{
		"unknown":   "BBBBBBBBBBBBBBBB",
		"malformed": "not a code at all",
		"expired":   expired.Code,
		"revoked":   revoked.Code,
		"used up":   usedUp.Code,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.svc.PreviewInvite(ctx, code)
			assert.Equal(t, httpx.ErrNotFound, err, "preview")
			_, err = f.svc.RedeemInvite(ctx, userActor(stranger), code)
			assert.Equal(t, httpx.ErrNotFound, err, "redeem")
			assert.Equal(t, httpx.ErrNotFound, f.svc.RevokeInvite(ctx, userActor(f.owner), code), "revoke")
		})
	}
	assert.False(t, f.isMember(t, stranger))

	var rows int
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM invites WHERE id = $1`,
		int64(usedUp.ID)).Scan(&rows))
	assert.Zero(t, rows, "the redemption that spent the last use deleted the invite")
}

// TestConcurrentRedemptionsGetExactlyMaxUses is the redemption statement's whole reason to be one
// statement. Ten accounts race for three uses; exactly three get in. M10 measured the read-then-update
// shape at four racers out of four getting in.
func TestConcurrentRedemptionsGetExactlyMaxUses(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := t.Context()
	inv := f.invite(t, CreateInviteInput{MaxUses: 3})

	racers := make([]snowflake.ID, 10)
	for i := range racers {
		racers[i] = f.newUser(ctx, "racer"+string(rune('a'+i)))
	}

	var wg sync.WaitGroup
	results := make([]error, len(racers))
	start := make(chan struct{})
	for i, id := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, results[i] = f.svc.RedeemInvite(ctx, userActor(id), inv.Code)
		}()
	}
	close(start)
	wg.Wait()

	joined := 0
	for _, err := range results {
		switch {
		case err == nil:
			joined++
		case errors.Is(err, httpx.ErrNotFound):
		default:
			t.Fatalf("a racer failed some other way: %v", err)
		}
	}
	assert.Equal(t, 3, joined)

	var members int
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT count(*) FROM guild_members WHERE guild_id = $1 AND user_id = ANY($2)`,
		int64(f.guildID), ids64(racers)).Scan(&members))
	assert.Equal(t, 3, members, "exactly the winners are members")
}

func ids64(ids []snowflake.ID) []int64 {
	out := make([]int64, len(ids))
	for i, id := range ids {
		out[i] = int64(id)
	}
	return out
}

// TestARedeemingMemberSpendsNothing: a second click is not an error and not a second use. The guild comes
// back, the count does not move, and nothing is recorded, because nobody joined.
func TestARedeemingMemberSpendsNothing(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := t.Context()
	inv := f.invite(t, CreateInviteInput{MaxUses: 1})

	guild, err := f.svc.RedeemInvite(ctx, userActor(f.member), inv.Code)
	require.NoError(t, err)
	assert.Equal(t, f.guildID, guild.ID)

	var uses int
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT uses FROM invites WHERE id = $1`, int64(inv.ID)).Scan(&uses))
	assert.Zero(t, uses, "a member redeeming spends no use, so a single-use invite still works for its outsider")
	assert.Zero(t, f.auditCount(t, ActionMemberJoin))

	_, err = f.svc.RedeemInvite(ctx, userActor(f.outsider), inv.Code)
	require.NoError(t, err)
}

func (f *inviteFixture) auditCount(t *testing.T, action string) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM audit_log_entries WHERE guild_id = $1 AND action = $2`,
		int64(f.guildID), action).Scan(&n))
	return n
}

// TestInvitesAndJoinsAreAudited: one entry each for creating, revoking and joining, naming who did it and
// what it was done to — and none of them carrying the code. The audit log is read under PermViewAuditLog,
// which is not the bit that lists live codes, and a live code is a way into the guild.
func TestInvitesAndJoinsAreAudited(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := t.Context()

	kept := f.invite(t, CreateInviteInput{MaxUses: 5, MaxAge: time.Hour})
	revoked := f.invite(t, CreateInviteInput{})
	require.NoError(t, f.svc.RevokeInvite(ctx, userActor(f.inviter), revoked.Code))
	_, err := f.svc.RedeemInvite(ctx, userActor(f.outsider), kept.Code)
	require.NoError(t, err)

	for _, c := range []struct {
		action        string
		count         int
		actor, target snowflake.ID
	}{
		{ActionInviteCreate, 2, f.inviter, 0},
		{ActionInviteRevoke, 1, f.inviter, revoked.ID},
		{ActionMemberJoin, 1, f.outsider, f.outsider},
	} {
		t.Run(c.action, func(t *testing.T) {
			require.Equal(t, c.count, f.auditCount(t, c.action))
			rows, err := f.pool.Query(ctx, `SELECT actor_id, target_id, changes FROM audit_log_entries
			                                WHERE guild_id = $1 AND action = $2`, int64(f.guildID), c.action)
			require.NoError(t, err)
			defer rows.Close()
			for rows.Next() {
				var actor int64
				var target *int64
				var changes []byte
				require.NoError(t, rows.Scan(&actor, &target, &changes))
				assert.Equal(t, int64(c.actor), actor)
				require.NotNil(t, target)
				if c.target != 0 {
					assert.Equal(t, int64(c.target), *target)
				}
				for _, code := range []string{kept.Code, revoked.Code} {
					assert.NotContains(t, string(changes), code, "an audit entry carries a live code")
				}
			}
		})
	}

	// The join names the invite that brought it, which is what ties an arrival to its invitation.
	var changes []byte
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT changes FROM audit_log_entries WHERE guild_id = $1 AND action = $2`,
		int64(f.guildID), ActionMemberJoin).Scan(&changes))
	var payload map[string]any
	require.NoError(t, json.Unmarshal(changes, &payload))
	assert.Equal(t, kept.ID.String(), payload["invite_id"])
	assert.Equal(t, f.inviter.String(), payload["inviter_id"])
}

// TestCreatingAnInviteNeedsCreateInviteOnThatChannel: the bit, resolved with the channel's overwrites, and
// the view bit with it, since the channel is where the joiner lands.
func TestCreatingAnInviteNeedsCreateInviteOnThatChannel(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := t.Context()

	create := func(actor, channel snowflake.ID) error {
		_, err := f.svc.CreateInvite(ctx, userActor(actor), CreateInviteInput{ChannelID: channel})
		return err
	}

	assert.Equal(t, httpx.ErrForbidden, create(f.member, f.channelID), "a member without the bit")
	assert.Equal(t, httpx.ErrNotFound, create(f.outsider, f.channelID), "a non-member")
	require.NoError(t, create(f.owner, f.channelID), "the owner")

	// A member-tier deny on one channel takes it there and nowhere else.
	denied := f.newChannel(ctx, f.guildID)
	f.overwrite(ctx, denied, roles.OverwriteTargetMember, f.inviter, 0, roles.PermCreateInvite)
	assert.Equal(t, httpx.ErrForbidden, create(f.inviter, denied))
	require.NoError(t, create(f.inviter, f.channelID))

	// A channel the inviter cannot see answers as one that does not exist.
	hidden := f.newChannel(ctx, f.guildID)
	f.overwrite(ctx, hidden, roles.OverwriteTargetMember, f.inviter, 0, roles.PermViewChannel)
	assert.Equal(t, httpx.ErrNotFound, create(f.inviter, hidden))

	// Text channels only.
	err := create(f.owner, f.newCategory(ctx, f.guildID))
	assert.ErrorIs(t, err, ErrUnsupportedChannelType)

	// And the request's own bounds.
	for _, in := range []CreateInviteInput{
		{ChannelID: f.channelID, MaxUses: MaxInviteUses + 1},
		{ChannelID: f.channelID, MaxAge: MaxInviteMaxAge + time.Second},
	} {
		_, err = f.svc.CreateInvite(ctx, userActor(f.owner), in)
		assert.ErrorIs(t, err, httpx.ErrBadRequest)
	}
}

// TestListingInvitesNeedsManageGuild: creating one is a smaller grant than reading every live one, since
// whoever can list them can hand any of them on. Expired ones are not listed.
func TestListingInvitesNeedsManageGuild(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := t.Context()
	live := f.invite(t, CreateInviteInput{})
	gone := f.invite(t, CreateInviteInput{MaxAge: time.Hour})
	f.exec(ctx, `UPDATE invites SET expires_at = now() - interval '1 second' WHERE id = $1`, int64(gone.ID))

	_, err := f.svc.ListInvites(ctx, userActor(f.inviter), f.guildID)
	assert.Equal(t, httpx.ErrForbidden, err, "PermCreateInvite does not list")
	_, err = f.svc.ListInvites(ctx, userActor(f.outsider), f.guildID)
	assert.Equal(t, httpx.ErrNotFound, err)

	manager := f.newUser(ctx, "manager")
	f.join(ctx, f.guildID, manager)
	f.grantRole(ctx, f.guildID, manager, f.newRole(ctx, f.guildID, 2, roles.PermManageGuild))
	list, err := f.svc.ListInvites(ctx, userActor(manager), f.guildID)
	require.NoError(t, err)
	require.Len(t, list, 1, "the expired invite is not listed")
	assert.Equal(t, live.Code, list[0].Code, "codes in full, because the guild needs them back")
	require.NotNil(t, list[0].Inviter)
	assert.Equal(t, "inviter", list[0].Inviter.Username)
}

// TestRevokingIsTheCreatorsOrManageGuilds: the creator, while still a member, and a PermManageGuild holder;
// nobody else. Two actors who are not the same person, because M17's sweep found tests whose moderator
// was always also the creator and could not see the branch that tells them apart.
func TestRevokingIsTheCreatorsOrManageGuilds(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := t.Context()
	inv := f.invite(t, CreateInviteInput{})

	assert.Equal(t, httpx.ErrForbidden, f.svc.RevokeInvite(ctx, userActor(f.member), inv.Code),
		"a member who did not create it")
	assert.Equal(t, httpx.ErrNotFound, f.svc.RevokeInvite(ctx, userActor(f.outsider), inv.Code),
		"a stranger holding the code is answered as though it named nothing")
	require.NoError(t, f.svc.RevokeInvite(ctx, userActor(f.inviter), inv.Code), "its creator")

	other := f.invite(t, CreateInviteInput{})
	manager := f.newUser(ctx, "manager")
	f.join(ctx, f.guildID, manager)
	f.grantRole(ctx, f.guildID, manager, f.newRole(ctx, f.guildID, 2, roles.PermManageGuild))
	require.NoError(t, f.svc.RevokeInvite(ctx, userActor(manager), other.Code), "somebody else's, by MANAGE_GUILD")

	// A creator who has left can no longer revoke; a moderator still can.
	leftover := f.invite(t, CreateInviteInput{})
	require.NoError(t, f.svc.RemoveMember(ctx, userActor(f.inviter), f.guildID, f.inviter))
	assert.Equal(t, httpx.ErrNotFound, f.svc.RevokeInvite(ctx, userActor(f.inviter), leftover.Code))
	require.NoError(t, f.svc.RevokeInvite(ctx, userActor(manager), leftover.Code))
}

// TestTheCeilingsHold: a guild's live invites, and the guilds an account is in — the second checked by
// redemption and by creating a guild, since both add a membership.
func TestTheCeilingsHold(t *testing.T) {
	t.Parallel()
	base := newInviteFixture(t)
	ctx := t.Context()
	tight, err := NewService(ServiceOptions{
		Pool: base.pool, IDs: base.svc.ids,
		MaxChannelsPerGuild: 500, MaxRolesPerGuild: 250, MaxGuildsPerAccount: 50,
		MaxJoinedGuildsPerAccount: 2, MaxInvitesPerGuild: 2,
	})
	require.NoError(t, err)
	inviter := userActor(base.inviter)

	first, err := tight.CreateInvite(ctx, inviter, CreateInviteInput{ChannelID: base.channelID})
	require.NoError(t, err)
	_, err = tight.CreateInvite(ctx, inviter, CreateInviteInput{ChannelID: base.channelID})
	require.NoError(t, err)
	_, err = tight.CreateInvite(ctx, inviter, CreateInviteInput{ChannelID: base.channelID})
	assert.ErrorIs(t, err, ErrGuildFull, "a third live invite")

	// An expired invite does not count against the guild.
	base.exec(ctx, `UPDATE invites SET expires_at = now() - interval '1 second' WHERE id = $1`, int64(first.ID))
	_, err = tight.CreateInvite(ctx, inviter, CreateInviteInput{ChannelID: base.channelID})
	require.NoError(t, err)

	// The outsider is in one guild of their own; joining this one through a live invite makes two, the
	// ceiling.
	_, err = tight.Create(ctx, userActor(base.outsider), CreateGuildInput{Name: "mine"})
	require.NoError(t, err)
	live, err := tight.ListInvites(ctx, userActor(base.owner), base.guildID)
	require.NoError(t, err)
	require.NotEmpty(t, live)
	_, err = tight.RedeemInvite(ctx, userActor(base.outsider), live[0].Code)
	require.NoError(t, err)

	other := base.newGuild(ctx, base.owner, roles.PermViewChannel)
	otherChannel := base.newChannel(ctx, other)
	far, err := tight.CreateInvite(ctx, userActor(base.owner), CreateInviteInput{ChannelID: otherChannel})
	require.NoError(t, err)
	_, err = tight.RedeemInvite(ctx, userActor(base.outsider), far.Code)
	assert.ErrorIs(t, err, ErrGuildFull, "a third guild by invite")
	_, err = tight.Create(ctx, userActor(base.outsider), CreateGuildInput{Name: "another"})
	assert.ErrorIs(t, err, ErrGuildFull, "a third guild by creating one")

	var uses int
	require.NoError(t, base.pool.QueryRow(ctx, `SELECT uses FROM invites WHERE id = $1`, int64(far.ID)).Scan(&uses))
	assert.Zero(t, uses, "a refused redemption spends nothing")
}

// TestARedemptionWaitsForTheAccountLockAndSeesWhatItCounted is the joined ceiling's lock, proved without
// racing (M15's lesson). A transaction holding the account's lock adds the membership that fills the
// account, and stays open; the redemption must block on the lock and, once that commits, count the
// membership and refuse. Without the lock it does not block — it counts before the commit and joins one
// guild past the ceiling — and holdAndRace fails saying so. M13a's lesson: a lock on the row being changed
// does not cover a count over rows it does not touch.
func TestARedemptionWaitsForTheAccountLockAndSeesWhatItCounted(t *testing.T) {
	t.Parallel()
	base := newInviteFixture(t)
	ctx := t.Context()
	tight, err := NewService(ServiceOptions{
		Pool: base.pool, IDs: base.svc.ids,
		MaxChannelsPerGuild: 500, MaxRolesPerGuild: 250, MaxGuildsPerAccount: 50,
		MaxJoinedGuildsPerAccount: 1, MaxInvitesPerGuild: 500,
	})
	require.NoError(t, err)
	inv := base.invite(t, CreateInviteInput{})
	elsewhere := base.newGuild(ctx, base.owner, roles.PermViewChannel)

	err = base.holdAndRace(t, func(tx pgx.Tx) {
		_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(1313033476, ($1::bigint & 2147483647)::int)`,
			int64(base.outsider))
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `INSERT INTO guild_members (guild_id, user_id) VALUES ($1, $2)`,
			int64(elsewhere), int64(base.outsider))
		require.NoError(t, err)
	}, func() error {
		_, err := tight.RedeemInvite(context.Background(), userActor(base.outsider), inv.Code)
		return err
	})
	assert.ErrorIs(t, err, ErrGuildFull)
	assert.False(t, base.isMember(t, base.outsider))
}

// TestAnInviteNamesNobodyOnceItsCreatorIsDeleted: the issuer's name follows a message author's rule, so a
// deleted account's placeholder never reaches the person reading the preview.
func TestAnInviteNamesNobodyOnceItsCreatorIsDeleted(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := t.Context()
	inv := f.invite(t, CreateInviteInput{})
	f.exec(ctx, `UPDATE users SET deleted_at = now(), username = 'deleted-placeholder' WHERE id = $1`, int64(f.inviter))

	preview, err := f.svc.PreviewInvite(ctx, inv.Code)
	require.NoError(t, err)
	assert.Nil(t, preview.Inviter)
}

// TestARedemptionAndAGuildDeletionDoNotDeadlock is /code-review's first M20a finding, staged rather than
// raced. Deleting a guild locks it FOR UPDATE and then cascades to its invites; redemption locked the
// invite and then took the guild's key-share lock through the membership's foreign key — the other order.
// Here the guild is held as deletion holds it, the redemption is started and seen blocked, and then the
// deletion goes on to cascade. Locking the guild first, redemption is waiting without the invite, so the
// deletion completes and the redemption finds nothing. In the old order Postgres detected the cycle and
// aborted one of them, a 500 on one side.
func TestARedemptionAndAGuildDeletionDoNotDeadlock(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := t.Context()
	inv := f.invite(t, CreateInviteInput{})

	tx, err := f.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(ctx, `SELECT 1 FROM guilds WHERE id = $1 FOR UPDATE`, int64(f.guildID))
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, err := f.svc.RedeemInvite(context.Background(), userActor(f.outsider), inv.Code)
		done <- err
	}()
	f.waitUntilBlocked(t, done)

	_, err = tx.Exec(ctx, `DELETE FROM guilds WHERE id = $1`, int64(f.guildID))
	require.NoError(t, err, "the deletion cascades to the invite without meeting the redemption")
	require.NoError(t, tx.Commit(ctx))

	select {
	case err := <-done:
		assert.Equal(t, httpx.ErrNotFound, err, "the guild is gone, and the code with it")
	case <-time.After(10 * time.Second):
		t.Fatal("the redemption did not finish after the deletion committed")
	}
}

// waitUntilBlocked returns once Postgres reports a backend waiting on a lock, and fails if the operation
// whose result arrives on done finishes first.
func (f *inviteFixture) waitUntilBlocked(t *testing.T, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("the operation finished (err=%v) before it met the held lock", err)
		default:
		}
		var blocked bool
		require.NoError(t, f.pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			                WHERE datname = current_database() AND wait_event_type = 'Lock')`).Scan(&blocked))
		if blocked {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the operation never blocked on the held lock")
}

// TestACodeCollisionIsRetried: the retry ran inside the creating transaction, and Postgres aborts a
// transaction on an error, so the attempt after a unique violation failed too and the request was a 500
// (/code-review). A collision is now an empty RETURNING, and the next code is tried. Forced through the
// service's generator, since a real collision at 69 bits never happens.
func TestACodeCollisionIsRetried(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := t.Context()
	taken := f.invite(t, CreateInviteInput{}).Code

	codes := []string{taken, taken, "QQQQQQQQQQQQQQQQ"}
	f.svc.newInviteCode = func() (string, error) {
		next := codes[0]
		codes = codes[1:]
		return next, nil
	}
	inv, err := f.svc.CreateInvite(ctx, userActor(f.inviter), CreateInviteInput{ChannelID: f.channelID})
	require.NoError(t, err, "two collisions and then a free code")
	assert.Equal(t, "QQQQQQQQQQQQQQQQ", inv.Code)

	f.svc.newInviteCode = func() (string, error) { return taken, nil }
	_, err = f.svc.CreateInvite(ctx, userActor(f.inviter), CreateInviteInput{ChannelID: f.channelID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not generate an unused invite code",
		"three collisions is a broken generator, reported rather than looped on")
}

// TestAnInviteIntoAChannelBeingDeletedIs404: the channel is authorized without a lock, so its deletion can
// commit while the invite is inserted; the insert's foreign-key check waits for it and refuses. That was a
// 500 where messages and tags answer 404 (/code-review).
func TestAnInviteIntoAChannelBeingDeletedIs404(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := t.Context()

	err := f.holdAndRace(t, func(tx pgx.Tx) {
		_, err := tx.Exec(ctx, `DELETE FROM channels WHERE id = $1`, int64(f.channelID))
		require.NoError(t, err)
	}, func() error {
		_, err := f.svc.CreateInvite(context.Background(), userActor(f.inviter),
			CreateInviteInput{ChannelID: f.channelID})
		return err
	})
	assert.Equal(t, httpx.ErrNotFound, err)
}

// TestAnInviteIntoAHiddenChannelIsNotTheirsToManage: a MANAGE_GUILD holder who cannot see a channel is not
// shown the invites into it and cannot revoke them — the channel listing hides it from them, and M14's rule
// is that a channel you cannot see is not yours to manage (/code-review).
func TestAnInviteIntoAHiddenChannelIsNotTheirsToManage(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := t.Context()

	hidden := f.newChannel(ctx, f.guildID)
	manager := f.newUser(ctx, "manager")
	f.join(ctx, f.guildID, manager)
	f.grantRole(ctx, f.guildID, manager, f.newRole(ctx, f.guildID, 2, roles.PermManageGuild))
	f.overwrite(ctx, hidden, roles.OverwriteTargetMember, manager, 0, roles.PermViewChannel)

	open, err := f.svc.CreateInvite(ctx, userActor(f.owner), CreateInviteInput{ChannelID: f.channelID})
	require.NoError(t, err)
	secret, err := f.svc.CreateInvite(ctx, userActor(f.owner), CreateInviteInput{ChannelID: hidden})
	require.NoError(t, err)

	listed, err := f.svc.ListInvites(ctx, userActor(manager), f.guildID)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, open.Code, listed[0].Code, "only the invite into a channel they can see")

	assert.Equal(t, httpx.ErrNotFound, f.svc.RevokeInvite(ctx, userActor(manager), secret.Code))
	require.NoError(t, f.svc.RevokeInvite(ctx, userActor(manager), open.Code))

	all, err := f.svc.ListInvites(ctx, userActor(f.owner), f.guildID)
	require.NoError(t, err)
	assert.Len(t, all, 1, "the owner, who sees every channel, still sees the other")
}

// TestARevocationRecordsTheUsesItEndedAt: the revoke read the invite without a lock, so a redemption
// committing before its delete went unrecorded (/code-review). The count now comes from the delete.
func TestARevocationRecordsTheUsesItEndedAt(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := t.Context()
	inv := f.invite(t, CreateInviteInput{MaxUses: 5})

	err := f.holdAndRace(t, func(tx pgx.Tx) {
		_, err := tx.Exec(ctx, `UPDATE invites SET uses = uses + 1 WHERE id = $1`, int64(inv.ID))
		require.NoError(t, err)
	}, func() error {
		return f.svc.RevokeInvite(context.Background(), userActor(f.inviter), inv.Code)
	})
	require.NoError(t, err)

	var changes []byte
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT changes FROM audit_log_entries WHERE guild_id = $1 AND action = $2`,
		int64(f.guildID), ActionInviteRevoke).Scan(&changes))
	var payload map[string]any
	require.NoError(t, json.Unmarshal(changes, &payload))
	assert.EqualValues(t, 1, payload["uses"], "the use committed while the revocation waited")
}

// TestAnInviteExpiresOnTheDatabasesClock: every read asks about expiry against the database's now(), so the
// expiry is computed there too. It was the application's clock, and a host lagging the database stored an
// invite already expired (/code-review). Both columns are now() in one transaction, so the difference is
// exactly what was asked for; a clock read in Go cannot produce that.
func TestAnInviteExpiresOnTheDatabasesClock(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	inv := f.invite(t, CreateInviteInput{MaxAge: 90 * time.Minute})

	// Compared as intervals, to the microsecond: rounding to seconds could not tell two clocks apart.
	var exact bool
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT expires_at - created_at = interval '90 minutes' FROM invites WHERE id = $1`,
		int64(inv.ID)).Scan(&exact))
	assert.True(t, exact, "the expiry is the creating transaction's now() plus exactly what was asked for")
}

// TestAJoinRacingATransferSeesTheNewOwner is M20a's second /code-review: the redemption's guild lock was FOR
// KEY SHARE, which does not wait for a transfer's FOR NO KEY UPDATE, so a join racing one answered with the
// old owner and sent it to the joiner, whose session the transfer's GUILD_UPDATE never reached. Staged: an
// ownership change is held open, the redemption must block on it, and once it commits the guild the
// redemption answers with names the new owner.
func TestAJoinRacingATransferSeesTheNewOwner(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	inv := f.invite(t, CreateInviteInput{})

	var joined Guild
	err := f.holdAndRace(t, func(tx pgx.Tx) {
		_, err := tx.Exec(t.Context(), `UPDATE guilds SET owner_id = $1 WHERE id = $2`,
			int64(f.member), int64(f.guildID))
		require.NoError(t, err)
	}, func() error {
		var err error
		joined, err = f.svc.RedeemInvite(context.Background(), userActor(f.outsider), inv.Code)
		return err
	})
	require.NoError(t, err)
	assert.Equal(t, f.member, joined.OwnerID, "the owner the transfer committed, not the one it replaced")
}

// TestRevokingRefusesInTheOrderTheGuildRoutesDo: a caller who cannot see the invite's channel gets the 404
// an unknown code gets, before anything is said about their authority; one who can see it and lacks
// MANAGE_GUILD gets 403. Asking for the permission first answered the first caller 403 (M20a's second
// /code-review), which says the channel is there.
func TestRevokingRefusesInTheOrderTheGuildRoutesDo(t *testing.T) {
	t.Parallel()
	f := newInviteFixture(t)
	ctx := t.Context()
	hidden := f.newChannel(ctx, f.guildID)
	f.overwrite(ctx, hidden, roles.OverwriteTargetMember, f.member, 0, roles.PermViewChannel)

	secret, err := f.svc.CreateInvite(ctx, userActor(f.owner), CreateInviteInput{ChannelID: hidden})
	require.NoError(t, err)
	open, err := f.svc.CreateInvite(ctx, userActor(f.owner), CreateInviteInput{ChannelID: f.channelID})
	require.NoError(t, err)

	assert.Equal(t, httpx.ErrNotFound, f.svc.RevokeInvite(ctx, userActor(f.member), secret.Code))
	assert.Equal(t, httpx.ErrForbidden, f.svc.RevokeInvite(ctx, userActor(f.member), open.Code))
}
