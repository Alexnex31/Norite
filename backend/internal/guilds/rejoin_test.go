// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/guildauth"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// TestARejoinIsACleanSlate is M20a's answer to the question M13 left for whichever milestone first let a
// member come back, M57's until invites moved here: **a departure deletes the member's own overwrites, and
// a rejoin restores none of them.** Asked of both departures, leaving and being kicked, since they share the
// deletion and differ in who chose them.
//
// The cost is the point of the answer, and it is stated rather than hidden: a moderator's member-tier deny
// lasts until its subject leaves and redeems any live invite, and leaving needs no permission. Accepted at
// planning (F1) and carried to M74, which owns the restrictions meant to outlive a departure; see
// docs/security-ledger.md. The alternative a test would have to change for is overwrites surviving a
// departure, at which point this test is the one to rewrite rather than delete.
func TestARejoinIsACleanSlate(t *testing.T) {
	t.Parallel()

	for _, departure := range []string{"leaves", "is kicked"} {
		t.Run(departure, func(t *testing.T) {
			t.Parallel()
			f := newInviteFixture(t)
			ctx := t.Context()

			// A moderator's mute on one channel: the member may still see it and may not post in it.
			f.overwrite(ctx, f.channelID, roles.OverwriteTargetMember, f.member, 0, roles.PermSendMessages)
			mayPost := func() error {
				_, err := guildauth.Authorize(ctx, f.q(), userActor(f.member), f.guildID, f.channelID,
					roles.PermSendMessages)
				return err
			}
			require.Equal(t, httpx.ErrForbidden, mayPost(), "the mute holds while they are a member")

			if departure == "leaves" {
				require.NoError(t, f.svc.RemoveMember(ctx, userActor(f.member), f.guildID, f.member))
			} else {
				require.NoError(t, f.svc.RemoveMember(ctx, userActor(f.owner), f.guildID, f.member))
			}
			require.Zero(t, f.memberOverwrites(t, f.member), "a departure deletes the member's own overwrites")

			inv := f.invite(t, CreateInviteInput{})
			_, err := f.svc.RedeemInvite(ctx, userActor(f.member), inv.Code)
			require.NoError(t, err)

			require.Zero(t, f.memberOverwrites(t, f.member), "nothing comes back with them")
			require.NoError(t, mayPost(), "the mute is gone after leave-and-rejoin; M74 inherits that")
		})
	}
}

func (f *inviteFixture) memberOverwrites(t *testing.T, userID snowflake.ID) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM permission_overwrites WHERE target_type = $1 AND target_id = $2`,
		roles.OverwriteTargetMember, int64(userID)).Scan(&n))
	return n
}
