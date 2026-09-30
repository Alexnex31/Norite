// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package roles_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// ResolveMany is how the gateway decides who receives a channel's events, and Resolve is how REST decides
// who may read that channel. If the two ever disagree, a member sees events for a channel REST hides from
// them, or misses events for one it shows. So they are held equal, account by account, over a guild built
// to exercise every layer: an owner, an administrator, roles with overlapping bits, members holding random
// subsets, non-members, and overwrites at all three tiers. Seeded, so a failure reproduces.
func TestResolveManyAgreesWithResolveForEveryAccount(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()

	for seed := range uint64(5) {
		rng := rand.New(rand.NewPCG(seed, 0x6d31385f))
		owner := f.newUser(ctx, fmt.Sprintf("owner%d", seed))
		guildID, everyoneID := f.newGuild(ctx, owner, randomPerms(rng))
		channelID := f.newChannel(ctx, guildID)

		roleIDs := make([]snowflake.ID, 6)
		for i := range roleIDs {
			perms := randomPerms(rng)
			if i == 0 {
				perms = perms.Add(roles.PermAdministrator)
			}
			roleIDs[i] = f.newRole(ctx, guildID, i+1, perms)
		}

		var users []snowflake.ID
		users = append(users, owner)
		for i := range 24 {
			u := f.newUser(ctx, fmt.Sprintf("user%d_%d", seed, i))
			users = append(users, u)
			if i%6 == 5 {
				continue // a non-member
			}
			f.join(ctx, guildID, u)
			for _, r := range roleIDs {
				// Rarely the administrator, so most members reach the overwrites.
				if r == roleIDs[0] && rng.IntN(8) != 0 {
					continue
				}
				if rng.IntN(3) == 0 {
					f.grantRole(ctx, guildID, u, r)
				}
			}
		}

		f.overwrite(ctx, channelID, 0, everyoneID, randomPerms(rng), randomPerms(rng))
		for _, r := range roleIDs[1:] {
			if rng.IntN(2) == 0 {
				f.overwrite(ctx, channelID, 0, r, randomPerms(rng), randomPerms(rng))
			}
		}
		for _, u := range users[1:] {
			if rng.IntN(4) == 0 {
				f.overwrite(ctx, channelID, 1, u, randomPerms(rng), randomPerms(rng))
			}
		}

		for _, channel := range []snowflake.ID{channelID, 0} {
			many, err := roles.ResolveMany(ctx, f.q, guildID, channel, users, nil)
			require.NoError(t, err)

			for _, u := range users {
				one, err := roles.Resolve(ctx, f.q, guildID, u, channel)
				got, present := many[u]
				if err != nil {
					require.ErrorIs(t, err, roles.ErrNotAMember)
					assert.False(t, present, "seed %d: a non-member must be absent from ResolveMany", seed)
					continue
				}
				require.True(t, present, "seed %d: a member must be present", seed)
				assert.Equal(t, one.Permissions, got, "seed %d, channel %d, user %d", seed, channel, u)
			}
		}
	}
}

// A CHANNEL_DELETE is resolved after the channel's rows are gone, from the overwrites captured before the
// delete. Resolving from a supplied slice must agree with resolving from the rows.
func TestResolveManyFromSuppliedOverwritesAgreesWithTheRows(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()

	owner, member := f.newUser(ctx, "owner"), f.newUser(ctx, "member")
	guildID, everyoneID := f.newGuild(ctx, owner, roles.PermViewChannel|roles.PermSendMessages)
	f.join(ctx, guildID, member)
	channelID := f.newChannel(ctx, guildID)
	f.overwrite(ctx, channelID, 0, everyoneID, 0, roles.PermViewChannel)

	rows, err := f.q.ListChannelPermissionOverwrites(ctx, db.ListChannelPermissionOverwritesParams{
		ChannelID: int64(channelID), GuildID: int64(guildID),
	})
	require.NoError(t, err)

	fromRows, err := roles.ResolveMany(ctx, f.q, guildID, channelID, []snowflake.ID{owner, member}, nil)
	require.NoError(t, err)
	supplied, err := roles.ResolveMany(ctx, f.q, guildID, channelID, []snowflake.ID{owner, member}, rows)
	require.NoError(t, err)

	assert.Equal(t, fromRows, supplied)
	assert.False(t, supplied[member].Has(roles.PermViewChannel), "the member's view was denied")
	assert.True(t, supplied[owner].Has(roles.PermViewChannel), "and the owner bypasses it")
}

func TestResolveManyOfAMissingGuildIsEmpty(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	got, err := roles.ResolveMany(context.Background(), f.q, f.next(), 0, []snowflake.ID{f.next()}, nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// randomPerms draws from the bits overwrites matter for, so allows and denies collide often.
func randomPerms(rng *rand.Rand) roles.Permission {
	var p roles.Permission
	for _, bit := range []roles.Permission{
		roles.PermViewChannel, roles.PermSendMessages, roles.PermReadMessageHistory,
		roles.PermManageMessages, roles.PermManageChannels,
	} {
		if rng.IntN(2) == 0 {
			p = p.Add(bit)
		}
	}
	return p
}
