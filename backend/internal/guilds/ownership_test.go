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

	err := f.raceOwnershipChange(t, guildID, target, func() error {
		return f.svc.RemoveMember(ctx, userActor(operator), guildID, target)
	})
	require.ErrorIs(t, err, ErrCannotRemoveOwner,
		"having waited for the change, the kick must see the target is now the owner")

	var member bool
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM guild_members WHERE guild_id = $1 AND user_id = $2)`,
		int64(guildID), int64(target)).Scan(&member))
	require.True(t, member, "the new owner is still a member of the guild they own")
}

// raceOwnershipChange runs op while an ownership change of guildID to newOwner is held open, and returns
// op's result once the change has committed.
//
// It does not race (M15's lesson): it holds the change, starts op, waits until Postgres reports a backend
// blocked on a lock, and only then commits. An op that reads the owner without a lock never blocks — it
// finishes while the change is uncommitted, acting on the old owner — and this fails naming that rather
// than timing out quietly.
func (f *fixture) raceOwnershipChange(
	t *testing.T, guildID, newOwner snowflake.ID, op func() error,
) error {
	t.Helper()
	// A plain UPDATE is what a transfer's statement does to this row.
	return f.holdAndRace(t, func(tx pgx.Tx) {
		_, err := tx.Exec(t.Context(), `UPDATE guilds SET owner_id = $1 WHERE id = $2`,
			int64(newOwner), int64(guildID))
		require.NoError(t, err)
	}, op)
}

// holdAndRace is raceOwnershipChange with the held-open work supplied by the caller: hold runs inside a
// transaction left uncommitted while op starts, and the transaction commits only once op is blocked.
func (f *fixture) holdAndRace(t *testing.T, hold func(tx pgx.Tx), op func() error) error {
	t.Helper()
	ctx := t.Context()

	tx, err := f.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	hold(tx)

	done := make(chan error, 1)
	go func() { done <- op() }()

	blocked := false
	deadline := time.Now().Add(10 * time.Second)
	for !blocked && time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("the operation finished (err=%v) while a conflicting change was uncommitted: it read "+
				"what that change writes without a lock, so it acted on the state before it", err)
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
	require.True(t, blocked, "the operation never blocked on the held change")

	require.NoError(t, tx.Commit(ctx))

	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("the operation did not finish after the held change committed")
		return nil
	}
}

// TestAFormerOwnerCannotDeleteOrRenameAGuildTransferredMidRequest is /code-review's first M13a finding.
// Delete decided ownership through an unlocked resolve and then ran a bare DELETE; Update's rename
// authorized the same way. So a transfer committing between the check and the write let the former owner
// destroy, or rename, a guild that now belonged to somebody else — RemoveMember's race, on the two paths
// M13a did not look at.
func TestAFormerOwnerCannotDeleteOrRenameAGuildTransferredMidRequest(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()

	owner := f.newUser(ctx, "owner")
	heir := f.newUser(ctx, "heir")
	guildID := f.newGuild(ctx, owner, roles.PermViewChannel)
	f.join(ctx, guildID, heir)

	err := f.raceOwnershipChange(t, guildID, heir, func() error {
		return f.svc.Delete(ctx, userActor(owner), guildID)
	})
	require.ErrorIs(t, err, httpx.ErrForbidden, "the delete must see it no longer comes from the owner")

	var exists bool
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM guilds WHERE id = $1)`, int64(guildID)).Scan(&exists))
	require.True(t, exists, "and the heir's guild survives")

	// Back to the owner, then the rename. PermManageGuild is not granted to anybody here, so the owner's
	// authority to rename is layer 2 alone — the one a transfer takes away.
	f.exec(ctx, `UPDATE guilds SET owner_id = $1 WHERE id = $2`, int64(owner), int64(guildID))
	renamed := "taken back"
	err = f.raceOwnershipChange(t, guildID, heir, func() error {
		_, err := f.svc.Update(ctx, userActor(owner), guildID, UpdateGuildInput{Name: &renamed})
		return err
	})
	require.ErrorIs(t, err, httpx.ErrForbidden, "a rename authorized by ownership must see it has gone")
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

// TestTwoTransfersToOneAccountCannotBothPassTheCeiling is /code-review's second M13a finding. Transfers
// from different guilds lock only their own guild rows, so two aimed at one account both counted it below
// the ceiling and both committed — past the limit the ledger said a transfer could never exceed.
//
// The held transaction is the other transfer, mid-flight: it has taken the account's ownership lock and
// handed it a guild, which puts the account at its ceiling of one. The second transfer must wait for it,
// then count what it committed.
func TestTwoTransfersToOneAccountCannotBothPassTheCeiling(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()

	tiny, err := NewService(ServiceOptions{
		Pool: f.pool, IDs: f.svc.ids,
		MaxChannelsPerGuild: 500, MaxRolesPerGuild: 250, MaxGuildsPerAccount: 1,
	})
	require.NoError(t, err)

	first, second, heir := f.newUser(ctx, "first"), f.newUser(ctx, "second"), f.newUser(ctx, "heir")
	g1 := f.newGuild(ctx, first, roles.PermViewChannel)
	g2 := f.newGuild(ctx, second, roles.PermViewChannel)
	f.join(ctx, g1, heir)
	f.join(ctx, g2, heir)

	err = f.holdAndRace(t, func(tx pgx.Tx) {
		require.NoError(t, db.New(tx).LockAccountOwnership(ctx, int64(heir)))
		_, err := tx.Exec(ctx, `UPDATE guilds SET owner_id = $1 WHERE id = $2`, int64(heir), int64(g2))
		require.NoError(t, err)
	}, func() error {
		_, err := tiny.TransferOwnership(ctx, userActor(first), g1, heir)
		return err
	})
	require.ErrorIs(t, err, ErrGuildFull, "the second transfer must count the guild the first one gave")

	var owned int
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT count(*) FROM guilds WHERE owner_id = $1`, int64(heir)).Scan(&owned))
	require.Equal(t, 1, owned, "and the heir owns exactly the ceiling")
}

// TestARefusedCallerTakesNoLockOnTheGuild is /code-review's fifth and sixth M13a findings together.
//
// Both paths locked the guild row before knowing who was asking. RemoveMember took FOR SHARE before
// authorizing, so any stranger looping DELETE /guilds/{id}/members/{self} held share locks on an arbitrary
// guild that stalled its updates and transfers; TransferOwnership took FOR UPDATE before its owner check,
// so any member could take the exclusive lock by posting a transfer they would be refused — and FOR UPDATE
// also blocks the key-share lock every insert into a child table takes, so the guild's message sends
// waited behind it.
//
// Held here: the strongest lock there is, on the guild row, released only after the request returns. A
// refused caller that tries to lock anything waits for it, runs into its own timeout, and fails this test
// with a deadline rather than the refusal it should have given at once.
func TestARefusedCallerTakesNoLockOnTheGuild(t *testing.T) {
	t.Parallel()
	tf := newTransferFixture(t)

	tx, err := tf.pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(t.Context(), `SELECT 1 FROM guilds WHERE id = $1 FOR UPDATE`, int64(tf.guildID))
	require.NoError(t, err)

	withDeadline := func(op func(ctx context.Context) error) error {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		return op(ctx)
	}

	err = withDeadline(func(ctx context.Context) error {
		return tf.svc.RemoveMember(ctx, userActor(tf.stranger), tf.guildID, tf.stranger)
	})
	require.ErrorIs(t, err, httpx.ErrNotFound, "a stranger leaving a guild they are not in is refused at once")

	err = withDeadline(func(ctx context.Context) error {
		_, err := tf.svc.TransferOwnership(ctx, userActor(tf.member), tf.guildID, tf.member)
		return err
	})
	require.ErrorIs(t, err, httpx.ErrForbidden, "a member who is not the owner is refused at once")
}

// TestATransfersLockDoesNotStallTheGuildsInserts pins why the transfer's lock is FOR NO KEY UPDATE rather
// than FOR UPDATE. Every insert into a table referencing guilds — a channel, an audit entry, a recorded
// message — takes FOR KEY SHARE on the guild row, which FOR UPDATE blocks and FOR NO KEY UPDATE does not.
// Held for the length of a transfer, the stronger lock would stall the guild's writes behind it.
func TestATransfersLockDoesNotStallTheGuildsInserts(t *testing.T) {
	t.Parallel()
	tf := newTransferFixture(t)
	ctx := t.Context()

	tx, err := tf.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = db.New(tx).GetGuildForNoKeyUpdate(ctx, int64(tf.guildID))
	require.NoError(t, err)

	insertCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err = tf.pool.Exec(insertCtx,
		`INSERT INTO channels (id, guild_id, name, type, position) VALUES ($1, $2, 'during', 0, 0)`,
		int64(tf.next()), int64(tf.guildID))
	require.NoError(t, err, "a channel created while a transfer holds the guild row must not wait for it")
}
