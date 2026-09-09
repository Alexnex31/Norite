// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// overwriteFixture builds a guild with a channel, a moderator holding PermManageRoles at position 5, and
// a role above them at position 9.
type overwriteFixture struct {
	*fixture
	guildID, channelID snowflake.ID
	owner, mod, plain  snowflake.ID
	modRole, aboveRole snowflake.ID
	everyoneID         snowflake.ID
}

func newOverwriteFixture(t *testing.T, everyonePerms roles.Permission) *overwriteFixture {
	t.Helper()
	f := newFixture(t)
	ctx := t.Context()

	owner := f.newUser(ctx, "owner")
	mod := f.newUser(ctx, "mod")
	plain := f.newUser(ctx, "plain")

	guildID, everyoneID := f.newGuildWithEveryone(ctx, owner, everyonePerms)
	f.join(ctx, guildID, mod)
	f.join(ctx, guildID, plain)

	modRole := f.newRole(ctx, guildID, 5, roles.PermManageRoles|roles.PermViewChannel|roles.PermSendMessages)
	aboveRole := f.newRole(ctx, guildID, 9, roles.PermViewChannel)
	f.grantRole(ctx, guildID, mod, modRole)

	return &overwriteFixture{
		fixture: f, guildID: guildID, channelID: f.newChannel(ctx, guildID),
		owner: owner, mod: mod, plain: plain,
		modRole: modRole, aboveRole: aboveRole, everyoneID: everyoneID,
	}
}

// TestAnOverwriteCannotNameAPermissionTheCallerLacks covers both bitfields, because the split between
// them was considered and reversed: an earlier draft left deny unrestricted on the reasoning that taking
// a permission away cannot escalate the caller. Discord checks both, and a test is what keeps it checked.
func TestAnOverwriteCannotNameAPermissionTheCallerLacks(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	// The moderator holds neither bit below.
	for _, tc := range []struct {
		name        string
		allow, deny roles.Permission
	}{
		{"in allow", roles.PermBanMembers, 0},
		{"in deny", 0, roles.PermBanMembers},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.SetOverwrite(ctx, userActor(f.mod), SetOverwriteInput{
				ChannelID: f.channelID, TargetType: roles.OverwriteTargetRole,
				TargetID: f.everyoneID, Allow: tc.allow, Deny: tc.deny,
			})
			require.ErrorIs(t, err, httpx.ErrForbidden)
		})
	}

	// And a bit they do hold goes through, so the refusals above are about the bit and not the endpoint.
	_, err := f.svc.SetOverwrite(ctx, userActor(f.mod), SetOverwriteInput{
		ChannelID: f.channelID, TargetType: roles.OverwriteTargetRole,
		TargetID: f.everyoneID, Deny: roles.PermSendMessages,
	})
	require.NoError(t, err)
}

// TestDeletingAnOverwriteCannotLiftARestrictionOnYou is the escalation the whole union rule exists for,
// and the one whose absence would look most like thorough coverage — the create and replace paths would
// all be green.
//
// The setup is an ordinary configuration rather than a contrived one: a channel denies @everyone
// PermViewChannel, and the moderator's PermManageRoles is untouched by that deny. So they can manage the
// channel's permissions and cannot see it, which is exactly what a staff-only channel looks like from
// below.
func TestDeletingAnOverwriteCannotLiftARestrictionOnYou(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel|roles.PermSendMessages)
	ctx := t.Context()

	// Written directly: the owner would be the one to configure this, and going through the service here
	// would only be testing the owner's path.
	f.overwrite(ctx, f.channelID, roles.OverwriteTargetRole, f.everyoneID, 0, roles.PermViewChannel)

	res, err := roles.Resolve(ctx, f.q(), f.guildID, f.mod, f.channelID)
	require.NoError(t, err)
	require.True(t, res.Permissions.Has(roles.PermManageRoles), "the moderator may manage this channel")
	require.False(t, res.Permissions.Has(roles.PermViewChannel), "and cannot see it")

	err = f.svc.DeleteOverwrite(ctx, userActor(f.mod), f.channelID, roles.OverwriteTargetRole, f.everyoneID)
	require.ErrorIs(t, err, httpx.ErrForbidden,
		"deleting the row that denies you a permission grants you that permission")

	// The same escalation by replacement rather than removal: write an empty overwrite over it.
	_, err = f.svc.SetOverwrite(ctx, userActor(f.mod), SetOverwriteInput{
		ChannelID: f.channelID, TargetType: roles.OverwriteTargetRole, TargetID: f.everyoneID,
	})
	require.ErrorIs(t, err, httpx.ErrForbidden,
		"blanking the row is the same change by another verb, and the union check is what catches both")
}

// TestAnOverwriteCannotNameARoleAboveYourOwn is the hierarchy half, on both verbs.
func TestAnOverwriteCannotNameARoleAboveYourOwn(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	_, err := f.svc.SetOverwrite(ctx, userActor(f.mod), SetOverwriteInput{
		ChannelID: f.channelID, TargetType: roles.OverwriteTargetRole,
		TargetID: f.aboveRole, Deny: roles.PermViewChannel,
	})
	require.ErrorIs(t, err, ErrOutranked)

	f.overwrite(ctx, f.channelID, roles.OverwriteTargetRole, f.aboveRole, 0, roles.PermViewChannel)
	err = f.svc.DeleteOverwrite(ctx, userActor(f.mod), f.channelID, roles.OverwriteTargetRole, f.aboveRole)
	require.ErrorIs(t, err, ErrOutranked,
		"removing a role's protection is as much an act on that role as writing one")
}

// TestAnOverwriteTargetMustMatchItsType pins the type-directed validation. A request naming a role id
// while claiming a member target would otherwise write a row no client can explain.
func TestAnOverwriteTargetMustMatchItsType(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	for _, tc := range []struct {
		name       string
		targetType int16
		targetID   snowflake.ID
		wantErr    error
	}{
		{"a role id claiming to be a member", roles.OverwriteTargetMember, f.everyoneID, httpx.ErrNotFound},
		{"a member id claiming to be a role", roles.OverwriteTargetRole, f.plain, httpx.ErrNotFound},
		{"a stranger", roles.OverwriteTargetMember, f.next(), httpx.ErrNotFound},
		{"a type the resolver ignores", 7, f.everyoneID, httpx.ErrBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.SetOverwrite(ctx, userActor(f.mod), SetOverwriteInput{
				ChannelID: f.channelID, TargetType: tc.targetType, TargetID: tc.targetID,
			})
			require.ErrorIs(t, err, tc.wantErr)
		})
	}

	// A member of this guild, correctly typed, is accepted — so the refusals above are about the mismatch.
	_, err := f.svc.SetOverwrite(ctx, userActor(f.mod), SetOverwriteInput{
		ChannelID: f.channelID, TargetType: roles.OverwriteTargetMember, TargetID: f.plain,
		Deny: roles.PermSendMessages,
	})
	require.NoError(t, err)
}

// TestAnOverwriteIsMeasuredAgainstChannelPermissions is the scope half of the escalation check.
//
// The moderator holds PermSendMessages at guild level and is denied it in this channel. Resolving at
// guild level would let them allow it back to themselves in one request; resolving in the channel refuses.
func TestAnOverwriteIsMeasuredAgainstChannelPermissions(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	f.overwrite(ctx, f.channelID, roles.OverwriteTargetRole, f.modRole, 0, roles.PermSendMessages)

	_, err := f.svc.SetOverwrite(ctx, userActor(f.mod), SetOverwriteInput{
		ChannelID: f.channelID, TargetType: roles.OverwriteTargetMember, TargetID: f.mod,
		Allow: roles.PermSendMessages,
	})
	require.ErrorIs(t, err, httpx.ErrForbidden,
		"the caller does not hold PermSendMessages *here*, which is the scope that governs an overwrite")
}

// TestAStrangerGetsTheSameAnswerForEveryOverwriteRefusal is the anti-enumeration property, and the reason
// checkOverwriteTarget is only ever called after authorizeChannel.
//
// M12 shipped two endpoints that answered a non-member with a public error naming something they could
// not see, and both were found by review. Every refusal here reads a loaded row, so every one of them is
// an authorization question wearing the shape of an input question.
func TestAStrangerGetsTheSameAnswerForEveryOverwriteRefusal(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()
	stranger := f.newUser(ctx, "stranger")

	for _, tc := range []struct {
		name  string
		input SetOverwriteInput
	}{
		{"a real role", SetOverwriteInput{ChannelID: f.channelID, TargetID: f.everyoneID}},
		{"a role above the caller", SetOverwriteInput{ChannelID: f.channelID, TargetID: f.aboveRole}},
		{"a nonexistent target", SetOverwriteInput{ChannelID: f.channelID, TargetID: f.next()}},
		{"a bogus type", SetOverwriteInput{ChannelID: f.channelID, TargetType: 7, TargetID: f.everyoneID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.SetOverwrite(ctx, userActor(stranger), tc.input)
			require.ErrorIs(t, err, httpx.ErrNotFound,
				"every one of these must be the answer a nonexistent channel gets")
		})
	}
}
