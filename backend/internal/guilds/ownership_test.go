// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// TestAKickWaitsForAnOwnershipChangeAndSeesItsResult pins the invariant M13a makes fragile: a guild's owner
// is always one of its members.
//
// RemoveMember refuses to remove the owner, and until M13a it read the owner without a lock — safe only
// because `owner_id` never changed after creation. Once it can, a kick that reads the old owner and then
// deletes the target can land on somebody a transfer made owner in between, leaving a guild owned by a
// non-member: no layer 2 at all, the state M12 forbids.
//
// A race too narrow to provoke by racing (M15's lesson), so this does not race. One transaction makes the
// target owner and holds its lock; the kick starts; the test waits until Postgres reports the kick blocked
// on a lock, then commits. The kick must then see the new owner and refuse. Without the lock the kick is
// never blocked — it reads the old owner and deletes the membership before the transaction commits — and
// the test says so rather than timing out quietly.
func TestAKickWaitsForAnOwnershipChangeAndSeesItsResult(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()

	owner := f.newUser(ctx, "owner")
	target := f.newUser(ctx, "target")
	// The kicker keeps full authority whichever way the ownership change lands, so the only thing that
	// can stop this kick is the owner check itself. The old owner would not do: once the change commits
	// they are an ordinary member, and the kick is refused for lacking PermKickMembers before the owner
	// check is ever asked — which is correct, and proves nothing about the lock.
	operator := f.newUser(ctx, "operator")
	f.makeInstanceAdmin(ctx, operator)
	guildID := f.newGuild(ctx, owner, roles.PermViewChannel)
	f.join(ctx, guildID, target)

	// The ownership change, held open. A plain UPDATE is what a transfer's statement does to this row.
	tx, err := f.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(ctx, `UPDATE guilds SET owner_id = $1 WHERE id = $2`, int64(target), int64(guildID))
	require.NoError(t, err)

	// The target is an ordinary member as far as any committed state says.
	done := make(chan error, 1)
	go func() { done <- f.svc.RemoveMember(ctx, userActor(operator), guildID, target) }()

	blocked := false
	deadline := time.Now().Add(10 * time.Second)
	for !blocked && time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("the kick finished (err=%v) while an ownership change was uncommitted: it read the "+
				"guild row without a lock, so it acted on the old owner and could remove the new one", err)
		default:
		}
		require.NoError(t, f.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			                WHERE datname = current_database() AND wait_event_type = 'Lock')`,
		).Scan(&blocked))
		if !blocked {
			time.Sleep(20 * time.Millisecond)
		}
	}
	require.True(t, blocked, "the kick never blocked on the guild row")

	require.NoError(t, tx.Commit(ctx))

	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrCannotRemoveOwner,
			"having waited for the change, the kick must see the target is now the owner")
	case <-time.After(10 * time.Second):
		t.Fatal("the kick did not finish after the ownership change committed")
	}

	var member bool
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM guild_members WHERE guild_id = $1 AND user_id = $2)`,
		int64(guildID), int64(target)).Scan(&member))
	require.True(t, member, "the new owner is still a member of the guild they own")
}

// transferFixture is a guild with an owner, an ordinary member, an administrator and a stranger.
type transferFixture struct {
	*fixture
	guildID                        snowflake.ID
	owner, member, admin, stranger snowflake.ID
}

func newTransferFixture(t *testing.T) *transferFixture {
	t.Helper()
	f := newFixture(t)
	ctx := t.Context()

	tf := &transferFixture{fixture: f}
	tf.owner = f.newUser(ctx, "owner")
	tf.member = f.newUser(ctx, "member")
	tf.admin = f.newUser(ctx, "admin")
	tf.stranger = f.newUser(ctx, "stranger")

	tf.guildID = f.newGuild(ctx, tf.owner, roles.PermViewChannel)
	f.join(ctx, tf.guildID, tf.member)
	f.join(ctx, tf.guildID, tf.admin)
	f.grantRole(ctx, tf.guildID, tf.admin, f.newRole(ctx, tf.guildID, 1, roles.PermAdministrator))
	return tf
}

func (tf *transferFixture) ownerOf(t *testing.T) snowflake.ID {
	t.Helper()
	var owner int64
	require.NoError(t, tf.pool.QueryRow(t.Context(),
		`SELECT owner_id FROM guilds WHERE id = $1`, int64(tf.guildID)).Scan(&owner))
	return snowflake.ID(owner)
}

// TestTransferringOwnershipWritesOneEntryNamingBothOwners is the milestone's happy path and its rule 2
// half together: the owner changes, and exactly one entry records it with the pair.
func TestTransferringOwnershipWritesOneEntryNamingBothOwners(t *testing.T) {
	t.Parallel()
	tf := newTransferFixture(t)
	ctx := t.Context()

	guild, err := tf.svc.TransferOwnership(ctx, userActor(tf.owner), tf.guildID, tf.member)
	require.NoError(t, err)
	require.Equal(t, tf.member, guild.OwnerID, "the response is the guild as it now stands")
	require.Equal(t, tf.member, tf.ownerOf(t))

	entries, err := tf.svc.ListAuditLog(ctx, userActor(tf.member), tf.guildID,
		ListAuditLogInput{Action: ActionGuildOwnerTransfer})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, tf.owner, entries[0].ActorID)
	require.Equal(t,
		map[string]any{"owner_id": map[string]any{"from": tf.owner.String(), "to": tf.member.String()}},
		decodeChanges(t, entries[0].Changes))
}

// TestOnlyTheOwnerOrAnInstanceAdminMayTransfer: ownership is layer 2 and no permission reaches it, so an
// administrator of the guild is refused exactly as a plain member is, and a stranger learns nothing.
func TestOnlyTheOwnerOrAnInstanceAdminMayTransfer(t *testing.T) {
	t.Parallel()
	tf := newTransferFixture(t)
	ctx := t.Context()

	_, err := tf.svc.TransferOwnership(ctx, userActor(tf.admin), tf.guildID, tf.admin)
	require.ErrorIs(t, err, httpx.ErrForbidden, "PermAdministrator does not reach ownership")
	_, err = tf.svc.TransferOwnership(ctx, userActor(tf.member), tf.guildID, tf.member)
	require.ErrorIs(t, err, httpx.ErrForbidden)
	_, err = tf.svc.TransferOwnership(ctx, userActor(tf.stranger), tf.guildID, tf.stranger)
	require.ErrorIs(t, err, httpx.ErrNotFound, "a stranger must not learn the guild exists")
	require.Equal(t, tf.owner, tf.ownerOf(t), "and nothing moved")

	// The case that motivates the milestone: the tier, who is in no guild, unsticking one.
	operator := tf.newUser(ctx, "operator")
	tf.makeInstanceAdmin(ctx, operator)
	_, err = tf.svc.TransferOwnership(ctx, userActor(operator), tf.guildID, tf.member)
	require.NoError(t, err)
	require.Equal(t, tf.member, tf.ownerOf(t))

	// And for a guild id naming nothing, the tier gets 404 rather than a 500 from the foreign key.
	_, err = tf.svc.TransferOwnership(ctx, userActor(operator), tf.next(), tf.member)
	require.ErrorIs(t, err, httpx.ErrNotFound)
}

// TestTheRecipientMustBeALiveMember covers every way the recipient can be wrong, and that each non-member
// answer is the same 404 a missing guild gets.
func TestTheRecipientMustBeALiveMember(t *testing.T) {
	t.Parallel()
	tf := newTransferFixture(t)
	ctx := t.Context()
	owner := userActor(tf.owner)

	_, err := tf.svc.TransferOwnership(ctx, owner, tf.guildID, tf.stranger)
	require.ErrorIs(t, err, httpx.ErrNotFound, "not a member")
	_, err = tf.svc.TransferOwnership(ctx, owner, tf.guildID, tf.next())
	require.ErrorIs(t, err, httpx.ErrNotFound, "no such account")
	_, err = tf.svc.TransferOwnership(ctx, owner, tf.guildID, tf.owner)
	require.ErrorIs(t, err, httpx.ErrBadRequest, "already the owner")

	// A deleted account is still in guild_members until M76a decides otherwise, and must not receive a
	// guild — M76a's own entry: a guild whose owner is deleted is not a guild with a NULL owner.
	tf.exec(ctx, `UPDATE users SET deleted_at = now() WHERE id = $1`, int64(tf.member))
	_, err = tf.svc.TransferOwnership(ctx, owner, tf.guildID, tf.member)
	require.ErrorIs(t, err, httpx.ErrNotFound, "a deleted account")

	require.Equal(t, tf.owner, tf.ownerOf(t))
}

// TestTheStatementRefusesARecipientWhoIsNotAMember pins the guard in the SQL on its own. The service checks
// membership first, so without this the statement's guard could be removed and nothing would notice —
// and it is the guard that holds when a kick lands between the service's check and the write.
func TestTheStatementRefusesARecipientWhoIsNotAMember(t *testing.T) {
	t.Parallel()
	tf := newTransferFixture(t)
	ctx := t.Context()

	_, err := tf.q().TransferGuildOwnership(ctx, db.TransferGuildOwnershipParams{
		GuildID: int64(tf.guildID), FromOwner: int64(tf.owner), ToOwner: int64(tf.stranger),
	})
	require.ErrorIs(t, err, pgx.ErrNoRows, "the statement must refuse a non-member by itself")

	_, err = tf.q().TransferGuildOwnership(ctx, db.TransferGuildOwnershipParams{
		GuildID: int64(tf.guildID), FromOwner: int64(tf.member), ToOwner: int64(tf.admin),
	})
	require.ErrorIs(t, err, pgx.ErrNoRows, "and a transfer from somebody who is not the owner")
	require.Equal(t, tf.owner, tf.ownerOf(t))
}

// TestTheFormerOwnerIsAnOrdinaryMemberAndMayLeave is the done-when's second half: the dead end M12 left is
// closed, and the authority went with the title.
func TestTheFormerOwnerIsAnOrdinaryMemberAndMayLeave(t *testing.T) {
	t.Parallel()
	tf := newTransferFixture(t)
	ctx := t.Context()

	_, err := tf.svc.TransferOwnership(ctx, userActor(tf.owner), tf.guildID, tf.member)
	require.NoError(t, err)

	require.ErrorIs(t, tf.svc.Delete(ctx, userActor(tf.owner), tf.guildID), httpx.ErrForbidden,
		"the former owner no longer holds layer 2")
	require.NoError(t, tf.svc.RemoveMember(ctx, userActor(tf.owner), tf.guildID, tf.owner),
		"and may now leave, which M12 refused them")
	require.ErrorIs(t, tf.svc.RemoveMember(ctx, userActor(tf.member), tf.guildID, tf.member),
		ErrCannotRemoveOwner, "the new owner is held in place the way the old one was")
}

// TestARecipientAtTheOwnedGuildCeilingIsRefused: a transfer is otherwise the one way past the ceiling
// Create enforces, and the way to push unwanted guilds onto somebody else's allowance.
func TestARecipientAtTheOwnedGuildCeilingIsRefused(t *testing.T) {
	t.Parallel()
	tf := newTransferFixture(t)
	ctx := t.Context()

	tiny, err := NewService(ServiceOptions{
		Pool: tf.pool, IDs: tf.svc.ids,
		MaxChannelsPerGuild: 500, MaxRolesPerGuild: 250, MaxGuildsPerAccount: 1,
	})
	require.NoError(t, err)

	// The member already owns one guild, which is the whole allowance here.
	tf.newGuild(ctx, tf.member, roles.PermViewChannel)

	_, err = tiny.TransferOwnership(ctx, userActor(tf.owner), tf.guildID, tf.member)
	require.ErrorIs(t, err, ErrGuildFull)
	require.Equal(t, tf.owner, tf.ownerOf(t))
}

// decodeChanges reads an entry's payload the way a client does, so the ids compare as the quoted decimal
// strings they cross the wire as.
func decodeChanges(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}
