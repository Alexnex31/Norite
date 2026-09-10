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

// TestTheChannelListingHidesADeniedChannel is the debt M12 wrote into M13's roadmap entry: the listing
// could not filter by per-channel view permission because nothing could write an overwrite to hide one
// with.
//
// The three short-circuits are asserted alongside, because a filter written against the two obvious ones
// looks correct while doing the right thing by accident — layer 3 returns permAll before overwrites are
// applied, so an administrator's listing is unfiltered whether the code remembers them or not.
func TestTheChannelListingHidesADeniedChannel(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	open := f.channelID
	hidden := f.newChannel(ctx, f.guildID)
	f.overwrite(ctx, hidden, roles.OverwriteTargetRole, f.everyoneID, 0, roles.PermViewChannel)

	names := func(who snowflake.ID) []snowflake.ID {
		got, err := f.svc.ListChannels(ctx, userActor(who), f.guildID)
		require.NoError(t, err)
		ids := make([]snowflake.ID, 0, len(got))
		for _, c := range got {
			ids = append(ids, c.ID)
		}
		return ids
	}

	require.ElementsMatch(t, []snowflake.ID{open}, names(f.plain),
		"a plain member sees only the channel they are not denied")
	require.ElementsMatch(t, []snowflake.ID{open, hidden}, names(f.owner),
		"the owner sees everything — layer 2 sits above layer 5")

	admin := f.newUser(ctx, "admin")
	f.join(ctx, f.guildID, admin)
	adminRole := f.newRole(ctx, f.guildID, 7, roles.PermAdministrator)
	f.grantRole(ctx, f.guildID, admin, adminRole)
	require.ElementsMatch(t, []snowflake.ID{open, hidden}, names(admin),
		"and so does a member holding PermAdministrator — layer 3, the short-circuit a two-name filter misses")

	instanceAdmin := f.newUser(ctx, "instance-admin")
	f.makeInstanceAdmin(ctx, instanceAdmin)
	require.ElementsMatch(t, []snowflake.ID{open, hidden}, names(instanceAdmin),
		"layer 1 is never resolved against the guild, so the filter has to check the tier explicitly")
}

// TestADenyInOneChannelDoesNotFollowTheListingLoop is the bug the guild-wide read makes possible and that
// a single-channel fixture cannot see.
//
// Every channel's overwrites arrive in one slice, so a loop that applies the whole slice per channel
// accumulates them: an allow on one channel restoring what another denied, and a deny anywhere hiding
// everything. Resolution.InChannel filters by channel id for this reason, and this is the service-level
// assertion of it.
func TestADenyInOneChannelDoesNotFollowTheListingLoop(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel|roles.PermSendMessages)
	ctx := t.Context()

	first := f.channelID
	second := f.newChannel(ctx, f.guildID)
	third := f.newChannel(ctx, f.guildID)

	// One allow, one deny, one member-tier deny — three tiers, three channels, one slice.
	f.overwrite(ctx, first, roles.OverwriteTargetRole, f.everyoneID, roles.PermViewChannel, 0)
	f.overwrite(ctx, second, roles.OverwriteTargetRole, f.everyoneID, 0, roles.PermViewChannel)
	f.overwrite(ctx, third, roles.OverwriteTargetMember, f.plain, 0, roles.PermSendMessages)

	got, err := f.svc.ListChannels(ctx, userActor(f.plain), f.guildID)
	require.NoError(t, err)

	ids := make([]snowflake.ID, 0, len(got))
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	require.ElementsMatch(t, []snowflake.ID{first, third}, ids,
		"only the channel that denies viewing is hidden — the other two carry unrelated overwrites")
}

// TestTheListingCarriesEachChannelsOwnOverwrites pins the embedded payload, which is the read side of the
// endpoint C1 added and the reason a client can draw a permission editor at all.
func TestTheListingCarriesEachChannelsOwnOverwrites(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	bare := f.channelID
	configured := f.newChannel(ctx, f.guildID)
	f.overwrite(ctx, configured, roles.OverwriteTargetRole, f.everyoneID, roles.PermSendMessages, 0)
	f.overwrite(ctx, configured, roles.OverwriteTargetMember, f.plain, 0, roles.PermSendMessages)

	got, err := f.svc.ListChannels(ctx, userActor(f.owner), f.guildID)
	require.NoError(t, err)

	byID := map[snowflake.ID]Channel{}
	for _, c := range got {
		byID[c.ID] = c
	}

	require.NotNil(t, byID[bare].PermissionOverwrites,
		"never nil: an omitted array and an empty one must not be one schema meaning two things")
	require.Empty(t, byID[bare].PermissionOverwrites)
	require.Len(t, byID[configured].PermissionOverwrites, 2,
		"and a channel's own rows, not the guild's")
}

// TestAChannelInheritsItsCategorysOverwrites is the control a security review found missing: the query
// that copies them existed, fully written, with no call site — while the endpoint that makes a category
// worth locking had already shipped.
func TestAChannelInheritsItsCategorysOverwrites(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel|roles.PermManageChannels)
	ctx := t.Context()

	category := f.newCategory(ctx, f.guildID)
	f.overwrite(ctx, category, roles.OverwriteTargetRole, f.everyoneID, 0, roles.PermViewChannel)

	child, err := f.svc.CreateChannel(ctx, userActor(f.owner), f.guildID, CreateChannelInput{
		Name: "staff-notes", Type: ChannelGuildText, ParentID: &category,
	})
	require.NoError(t, err)
	require.Len(t, child.PermissionOverwrites, 1,
		"the response carries what the channel has, not an empty array a client would cache as truth")

	// The point of the copy, asserted through the listing rather than through the rows.
	got, err := f.svc.ListChannels(ctx, userActor(f.plain), f.guildID)
	require.NoError(t, err)
	for _, c := range got {
		require.NotEqual(t, child.ID, c.ID,
			"a channel created inside a locked category must not be readable by everyone")
	}

	// And a channel created at the top level inherits nothing, so the copy is scoped to the parent.
	loose, err := f.svc.CreateChannel(ctx, userActor(f.owner), f.guildID, CreateChannelInput{
		Name: "general-2", Type: ChannelGuildText,
	})
	require.NoError(t, err)
	require.Empty(t, loose.PermissionOverwrites)
}

// TestAChannelCannotBeCreatedInACategoryYouCannotManage is the other half. CreateChannel resolved
// PermManageChannels at guild level while UpdateChannel and DeleteChannel resolved per-channel, so a
// guild-wide grant reached inside a category that denied it.
func TestAChannelCannotBeCreatedInACategoryYouCannotManage(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel|roles.PermManageChannels)
	ctx := t.Context()

	category := f.newCategory(ctx, f.guildID)
	f.overwrite(ctx, category, roles.OverwriteTargetRole, f.everyoneID, 0, roles.PermManageChannels)

	_, err := f.svc.CreateChannel(ctx, userActor(f.plain), f.guildID, CreateChannelInput{
		Name: "wedge", Type: ChannelGuildText, ParentID: &category,
	})
	require.ErrorIs(t, err, httpx.ErrForbidden,
		"a guild-wide grant must not reach inside a category that denies it")

	// The same caller may still create at the top level, so the refusal is about the category.
	_, err = f.svc.CreateChannel(ctx, userActor(f.plain), f.guildID, CreateChannelInput{
		Name: "fine", Type: ChannelGuildText,
	})
	require.NoError(t, err)
}

// TestDeletingARoleCannotLiftARestrictionOnYou closes the route around DeleteOverwrite.
//
// Removing one overwrite is refused when the caller does not hold the bits it carries. Deleting the role
// that overwrite names reached the same outcome on every channel at once, behind a guild-level permission
// check alone — so the endpoint that refuses the act had a sibling that performed it.
func TestDeletingARoleCannotLiftARestrictionOnYou(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel|roles.PermSendMessages)
	ctx := t.Context()

	// A low role that restricts whoever holds it, and the moderator holds it.
	restricting := f.newRole(ctx, f.guildID, 2, 0)
	f.grantRole(ctx, f.guildID, f.mod, restricting)
	f.overwrite(ctx, f.channelID, roles.OverwriteTargetRole, restricting, 0, roles.PermViewChannel)

	res, err := roles.Resolve(ctx, f.q(), f.guildID, f.mod, f.channelID)
	require.NoError(t, err)
	require.False(t, res.Permissions.Has(roles.PermViewChannel), "the moderator is denied this channel")

	// Refused directly...
	err = f.svc.DeleteOverwrite(ctx, userActor(f.mod), f.channelID, roles.OverwriteTargetRole, restricting)
	require.ErrorIs(t, err, httpx.ErrForbidden)

	// ...and refused by the route around it.
	err = f.svc.DeleteRole(ctx, userActor(f.mod), f.guildID, restricting)
	require.ErrorIs(t, err, httpx.ErrForbidden,
		"deleting the role that carries the deny is the same act as deleting the deny")

	// The owner, who is denied nothing, may still delete it — so the refusal is about the caller.
	require.NoError(t, f.svc.DeleteRole(ctx, userActor(f.owner), f.guildID, restricting))
}

// TestAHiddenChannelAnswers404ToAMemberWhoCannotSeeIt is the oracle the listing filter opened.
//
// A 403 for a member lacking a permission and a 404 for a non-member was the right split while every
// channel in a member's guild was listed to them. Once channels can be hidden, the pair confirms a hidden
// channel's existence to anybody holding its id.
func TestAHiddenChannelAnswers404ToAMemberWhoCannotSeeIt(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	hidden := f.newChannel(ctx, f.guildID)
	f.overwrite(ctx, hidden, roles.OverwriteTargetRole, f.everyoneID, 0, roles.PermViewChannel)

	_, err := f.svc.SetOverwrite(ctx, userActor(f.plain), SetOverwriteInput{
		ChannelID: hidden, TargetType: roles.OverwriteTargetRole, TargetID: f.everyoneID,
	})
	require.ErrorIs(t, err, httpx.ErrNotFound,
		"a member who cannot see the channel gets what a stranger gets")

	// Somebody who *can* see it and merely lacks PermManageRoles still gets 403: for them the channel's
	// existence was never a secret.
	_, err = f.svc.SetOverwrite(ctx, userActor(f.plain), SetOverwriteInput{
		ChannelID: f.channelID, TargetType: roles.OverwriteTargetRole, TargetID: f.everyoneID,
	})
	require.ErrorIs(t, err, httpx.ErrForbidden)
}

// TestARoleIsCreatedAtTheBottom is M13's placement rule.
//
// M12 created roles at max(position)+1, above everything. Under a hierarchy that is a dead end: the
// creator cannot edit, delete, assign or reposition a role above their own standing, so a moderator's
// first use of the endpoint produced a role they could not touch.
func TestARoleIsCreatedAtTheBottom(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	// modRole is at 5 and aboveRole at 9 from the fixture. A new role must land below both.
	created, err := f.svc.CreateRole(ctx, userActor(f.owner), f.guildID, CreateRoleInput{Name: "new"})
	require.NoError(t, err)
	require.Equal(t, int32(1), created.Position, "immediately above @everyone")

	listed, err := f.svc.ListRoles(ctx, userActor(f.owner), f.guildID)
	require.NoError(t, err)

	byID := map[snowflake.ID]int32{}
	for _, r := range listed {
		byID[r.ID] = r.Position
	}
	require.Equal(t, int32(0), byID[f.everyoneID], "@everyone never moves off the floor")
	require.Greater(t, byID[f.modRole], created.Position, "everything else shifted above it")
	require.Greater(t, byID[f.aboveRole], byID[f.modRole], "and relative order is preserved")

	// Positions stay bounded by the ceiling rather than growing with lifetime creations: create and
	// delete repeatedly and the survivors do not drift upward.
	for i := 0; i < 5; i++ {
		tmp, err := f.svc.CreateRole(ctx, userActor(f.owner), f.guildID, CreateRoleInput{Name: "tmp"})
		require.NoError(t, err)
		require.NoError(t, f.svc.DeleteRole(ctx, userActor(f.owner), f.guildID, tmp.ID))
	}
	after, err := f.svc.ListRoles(ctx, userActor(f.owner), f.guildID)
	require.NoError(t, err)
	for _, r := range after {
		require.LessOrEqual(t, r.Position, int32(len(after)),
			"renumbering keeps every position within the live role count, whatever the churn")
	}
}

// TestARoleAboveYourOwnCannotBeEditedOrDeleted is layer 4's second sentence on the two role endpoints
// that had no hierarchy check at all.
//
// refuseEscalation bounds *which permissions* may be granted and says nothing about *which role* may be
// given them, so without this a moderator could edit the administrator role — its name, its color, and
// the permissions it hands everybody who holds it.
func TestARoleAboveYourOwnCannotBeEditedOrDeleted(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	name := "renamed"
	_, err := f.svc.UpdateRole(ctx, userActor(f.mod), f.guildID, f.aboveRole, UpdateRoleInput{Name: &name})
	require.ErrorIs(t, err, ErrOutranked, "position 9 is above the moderator's 5")

	err = f.svc.DeleteRole(ctx, userActor(f.mod), f.guildID, f.aboveRole)
	require.ErrorIs(t, err, ErrOutranked)

	// A role below them is editable, so the refusal is about standing and not the endpoint.
	below := f.newRole(ctx, f.guildID, 2, 0)
	_, err = f.svc.UpdateRole(ctx, userActor(f.mod), f.guildID, below, UpdateRoleInput{Name: &name})
	require.NoError(t, err)

	// Equal standing is not enough: the moderator cannot edit the very role that gives them their standing.
	_, err = f.svc.UpdateRole(ctx, userActor(f.mod), f.guildID, f.modRole, UpdateRoleInput{Name: &name})
	require.ErrorIs(t, err, ErrOutranked,
		"strictly greater, or you could edit the role that establishes your own position")
}
