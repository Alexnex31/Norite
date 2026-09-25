// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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
