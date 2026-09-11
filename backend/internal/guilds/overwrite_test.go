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
// The setup is an ordinary configuration rather than a contrived one: a channel denies @everyone a
// permission the moderator would otherwise hold, and their PermManageRoles is untouched by that deny — so
// they may edit the channel's permissions and are themselves subject to one of them.
//
// The denied bit is PermSendMessages and not PermViewChannel, which this comment used to say. A caller
// who cannot *view* a channel is now refused it entirely, so that version of the setup stopped being
// reachable when M14 required the view bit alongside every management permission.
func TestDeletingAnOverwriteCannotLiftARestrictionOnYou(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel|roles.PermSendMessages)
	ctx := t.Context()

	// Written directly: the owner would be the one to configure this, and going through the service here
	// would only be testing the owner's path.
	//
	// The denied bit is PermSendMessages rather than PermViewChannel, and the difference is M14's item
	// zero. This test used to build a moderator who could manage a channel they could not *see*, which was
	// reachable until every channel-scoped route started requiring the view bit alongside its own. The
	// property under test is unchanged — a caller cannot remove a row that denies them something — and it
	// needs a bit that does not also decide whether they may reach the endpoint at all.
	f.overwrite(ctx, f.channelID, roles.OverwriteTargetRole, f.everyoneID, 0, roles.PermSendMessages)

	res, err := roles.Resolve(ctx, f.q(), f.guildID, f.mod, f.channelID)
	require.NoError(t, err)
	require.True(t, res.Permissions.Has(roles.PermManageRoles), "the moderator may manage this channel")
	require.True(t, res.Permissions.Has(roles.PermViewChannel), "and may see it, which the route requires")
	require.False(t, res.Permissions.Has(roles.PermSendMessages), "but is denied this one here")

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
	// PermSendMessages rather than PermViewChannel, for the reason the overwrite test above gives: every
	// channel-scoped route now requires the view bit, so a denial of *that* stops the caller reaching the
	// endpoint rather than exercising the check under test.
	f.overwrite(ctx, f.channelID, roles.OverwriteTargetRole, restricting, 0, roles.PermSendMessages)

	res, err := roles.Resolve(ctx, f.q(), f.guildID, f.mod, f.channelID)
	require.NoError(t, err)
	require.False(t, res.Permissions.Has(roles.PermSendMessages),
		"the moderator is denied this permission here, by a role they hold")

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

// TestAReorderCannotDemoteARoleAboveYourOwn is the takeover the origin check exists to refuse, and the
// one a destination-only check lets through.
//
// The natural rule — "every requested position must be below your standing" — is necessary and not
// sufficient. It says nothing about where the role is now, so a caller can name a role above them and
// move it below them, at which point every other check in the milestone passes for it.
func TestAReorderCannotDemoteARoleAboveYourOwn(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	// aboveRole sits at 9, the moderator's standing is 5. Destination 2 is below them; origin is not.
	_, err := f.svc.ReorderRoles(ctx, userActor(f.mod), f.guildID,
		[]RolePosition{{ID: f.aboveRole, Position: 2}})
	require.ErrorIs(t, err, ErrOutranked,
		"the destination is below the caller, which is exactly why the origin has to be checked too")

	// A role already below them moves freely, so the refusal is about standing and not the endpoint.
	below := f.newRole(ctx, f.guildID, 2, 0)
	_, err = f.svc.ReorderRoles(ctx, userActor(f.mod), f.guildID,
		[]RolePosition{{ID: below, Position: 3}})
	require.NoError(t, err)

	// And the destination check still holds on its own: a role below them cannot be pushed above them.
	_, err = f.svc.ReorderRoles(ctx, userActor(f.mod), f.guildID,
		[]RolePosition{{ID: below, Position: 8}})
	require.ErrorIs(t, err, ErrOutranked)
}

// TestAReorderRefusesAnArrangementWithACollision covers both routes to two roles sharing a position.
//
// There is no unique constraint to catch one — 000015 declines it deliberately, because a reorder has to
// pass through such a state — so the check is the only thing standing between a request and an ordering
// in which two roles are neither above nor below each other.
func TestAReorderRefusesAnArrangementWithACollision(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	a := f.newRole(ctx, f.guildID, 2, 0)
	b := f.newRole(ctx, f.guildID, 3, 0)

	t.Run("within the request", func(t *testing.T) {
		_, err := f.svc.ReorderRoles(ctx, userActor(f.owner), f.guildID,
			[]RolePosition{{ID: a, Position: 4}, {ID: b, Position: 4}})
		require.ErrorIs(t, err, httpx.ErrConflict)
	})

	t.Run("against a role the request does not mention", func(t *testing.T) {
		// modRole sits at 5 and is not in the request. A request that is internally consistent still has
		// to be checked against the roles it leaves alone.
		_, err := f.svc.ReorderRoles(ctx, userActor(f.owner), f.guildID,
			[]RolePosition{{ID: a, Position: 5}})
		require.ErrorIs(t, err, httpx.ErrConflict)
	})

	t.Run("a repeated id", func(t *testing.T) {
		_, err := f.svc.ReorderRoles(ctx, userActor(f.owner), f.guildID,
			[]RolePosition{{ID: a, Position: 6}, {ID: a, Position: 7}})
		require.ErrorIs(t, err, httpx.ErrBadRequest)
	})
}

// TestAReorderRefusesAnOutOfRangePosition bounds the one path that could write an arbitrary integer.
//
// Zero is @everyone's, and a role sharing the floor confers no standing on whoever holds it — so the
// protection that role was granted to give silently disappears. The upper bound exists because positions
// grow only through creation's renumber, which the ceiling bounds; a reorder writing near the column's
// maximum would make the next creation's renumber overflow and take role creation out permanently.
func TestAReorderRefusesAnOutOfRangePosition(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	target := f.newRole(ctx, f.guildID, 2, 0)

	for _, position := range []int32{0, -1, 251, 2147483647} {
		_, err := f.svc.ReorderRoles(ctx, userActor(f.owner), f.guildID,
			[]RolePosition{{ID: target, Position: position}})
		require.ErrorIs(t, err, httpx.ErrBadRequest, "position %d must be refused", position)
	}

	// The owner bypasses the standing check and does not bypass this one — they are the only caller who
	// could otherwise reach the ceiling.
	_, err := f.svc.ReorderRoles(ctx, userActor(f.owner), f.guildID,
		[]RolePosition{{ID: target, Position: 4}})
	require.NoError(t, err)
}

// TestAReorderCannotMoveTheDefaultRole keeps @everyone on the floor every resolution starts from.
func TestAReorderCannotMoveTheDefaultRole(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	_, err := f.svc.ReorderRoles(ctx, userActor(f.owner), f.guildID,
		[]RolePosition{{ID: f.everyoneID, Position: 3}})
	require.ErrorIs(t, err, ErrDefaultRoleImmutable)
}

// TestCreatingARoleAndAssigningItDoesNotRaiseStanding is the three-step sequence this milestone exists to
// refuse, driven end to end.
//
// It asserts *which* check refused each step rather than only that standing did not rise, because two
// independent guards refuse it and a test that watched the outcome alone would pin neither. Bottom
// placement means step 1 produces a role below the creator; the role check means step 2 could not have
// taken a top-placed one either.
func TestCreatingARoleAndAssigningItDoesNotRaiseStanding(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	before, err := roles.Resolve(ctx, f.q(), f.guildID, f.mod, 0)
	require.NoError(t, err)
	require.Equal(t, int32(5), before.Standing())

	created, err := f.svc.CreateRole(ctx, userActor(f.mod), f.guildID,
		CreateRoleInput{Name: "ladder", Permissions: roles.PermManageRoles})
	require.NoError(t, err)
	require.Less(t, created.Position, before.Standing(),
		"guard one: the new role lands below its creator, so there is no ladder to climb")

	_, err = f.svc.AssignRole(ctx, userActor(f.mod), f.guildID, f.mod, created.ID)
	require.NoError(t, err, "and taking a role below you is ordinary")

	after, err := roles.Resolve(ctx, f.q(), f.guildID, f.mod, 0)
	require.NoError(t, err)

	// The property is *relative*, not the absolute number, and the first draft of this test asserted the
	// number. Creating a role renumbers the guild — everything shifts up to make room at the bottom — so
	// the moderator's standing legitimately changes from 5 to 6 without them having gained anything. What
	// must not change is who they outrank.
	aboveRole, err := f.svc.ListRoles(ctx, userActor(f.owner), f.guildID)
	require.NoError(t, err)
	for _, r := range aboveRole {
		if r.ID == f.aboveRole {
			require.False(t, after.Outranks(r.Position),
				"the role that was above them is still above them after the whole sequence")
		}
		if r.ID == created.ID {
			require.True(t, after.Outranks(r.Position), "and the one they made is still below")
		}
	}

	// Guard two, independently: a role above them cannot be taken even when one exists.
	_, err = f.svc.AssignRole(ctx, userActor(f.mod), f.guildID, f.mod, f.aboveRole)
	require.ErrorIs(t, err, ErrOutranked,
		"guard two: the role check refuses a top-placed role regardless of how it got there")
}

// TestYouCannotAssignARoleCarryingPermissionsYouLack is this milestone's one deliberate departure from
// Discord, driven as the confederate scenario.
func TestYouCannotAssignARoleCarryingPermissionsYouLack(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	// A low role carrying a permission the moderator does not hold — the misconfiguration Discord's model
	// relies on nobody making.
	bot := f.newRole(ctx, f.guildID, 2, roles.PermManageGuild)

	_, err := f.svc.AssignRole(ctx, userActor(f.mod), f.guildID, f.plain, bot)
	require.ErrorIs(t, err, httpx.ErrForbidden,
		"a delegated authority never exceeds its delegator, and assignment is that question")

	// The owner holds everything and passes with no special branch — decision.allows exempts them.
	_, err = f.svc.AssignRole(ctx, userActor(f.owner), f.guildID, f.plain, bot)
	require.NoError(t, err)

	// And a role whose permissions the moderator does hold assigns fine.
	ordinary := f.newRole(ctx, f.guildID, 3, roles.PermViewChannel)
	_, err = f.svc.AssignRole(ctx, userActor(f.mod), f.guildID, f.plain, ordinary)
	require.NoError(t, err)
}

// TestYouCannotShedARoleThatRestrictsYou is gap 13: removing a role stopped being a pure demotion the
// moment a role could carry a channel deny.
func TestYouCannotShedARoleThatRestrictsYou(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel|roles.PermSendMessages)
	ctx := t.Context()

	muted := f.newRole(ctx, f.guildID, 2, 0)
	f.overwrite(ctx, f.channelID, roles.OverwriteTargetRole, muted, 0, roles.PermSendMessages)
	f.grantRole(ctx, f.guildID, f.mod, muted)

	_, err := f.svc.UnassignRole(ctx, userActor(f.mod), f.guildID, f.mod, muted)
	require.ErrorIs(t, err, httpx.ErrForbidden,
		"taking off a role that denies you something grants you that thing")

	// The owner, denied nothing, may remove it — so the refusal is about the caller.
	_, err = f.svc.UnassignRole(ctx, userActor(f.owner), f.guildID, f.mod, muted)
	require.NoError(t, err)
}

// TestAssignmentIsIdempotentAndSelfTargeted covers the two shapes a client depends on.
func TestAssignmentIsIdempotentAndSelfTargeted(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	target := f.newRole(ctx, f.guildID, 2, 0)

	first, err := f.svc.AssignRole(ctx, userActor(f.mod), f.guildID, f.plain, target)
	require.NoError(t, err)
	require.Contains(t, first.Roles, target)

	second, err := f.svc.AssignRole(ctx, userActor(f.mod), f.guildID, f.plain, target)
	require.NoError(t, err, "a repeat assignment is a PUT succeeding, not a conflict")
	require.Equal(t, first.Roles, second.Roles)

	require.NoError(t, mustUnassign(t, f, f.mod, f.plain, target))
	require.NoError(t, mustUnassign(t, f, f.mod, f.plain, target),
		"and removing what is not held is a DELETE succeeding")

	// Self-targeting: the target comparison is skipped, the role check is not.
	_, err = f.svc.AssignRole(ctx, userActor(f.mod), f.guildID, f.mod, target)
	require.NoError(t, err, "nobody outranks themselves, so a flat rule would break self-service roles")

	_, err = f.svc.AssignRole(ctx, userActor(f.mod), f.guildID, f.mod, f.modRole)
	require.ErrorIs(t, err, ErrOutranked,
		"but you still cannot take the role that establishes your own standing")

	// @everyone is never a grant.
	_, err = f.svc.AssignRole(ctx, userActor(f.owner), f.guildID, f.plain, f.everyoneID)
	require.ErrorIs(t, err, ErrDefaultRoleImmutable)

	// A non-member answers as a user id that does not exist does.
	_, err = f.svc.AssignRole(ctx, userActor(f.owner), f.guildID, f.next(), target)
	require.ErrorIs(t, err, httpx.ErrNotFound)
}

func mustUnassign(t *testing.T, f *overwriteFixture, actor, target, role snowflake.ID) error {
	t.Helper()
	_, err := f.svc.UnassignRole(t.Context(), userActor(actor), f.guildID, target, role)
	return err
}

// TestAMemberAboveYourOwnCannotBeActedOn is the M12 security review's original finding, and the field it
// did not name.
//
// The review found RemoveMember letting a PermKickMembers holder kick an administrator, and UpdateMember
// letting a PermMuteMembers holder server-mute one. `nickname` is the third field on the same endpoint and
// renaming somebody above you is the same class of act, so the check sits at the top of the operation
// rather than beside the two the review happened to list.
func TestAMemberAboveYourOwnCannotBeActedOn(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	// A member standing above the moderator, holding no permissions of their own — so the refusal is
	// about position and cannot be mistaken for a permission check.
	senior := f.newUser(ctx, "senior")
	f.join(ctx, f.guildID, senior)
	f.grantRole(ctx, f.guildID, senior, f.aboveRole)

	// The moderator holds every bit these operations need, guild-wide.
	f.exec(ctx, `UPDATE roles SET permissions = $1 WHERE id = $2`,
		(roles.PermManageRoles | roles.PermViewChannel | roles.PermKickMembers |
			roles.PermMuteMembers | roles.PermDeafenMembers | roles.PermManageGuild).Int64(),
		int64(f.modRole))

	mute, deaf := true, true
	nickname := "renamed"

	for _, tc := range []struct {
		name  string
		input UpdateMemberInput
	}{
		{"mute", UpdateMemberInput{Mute: &mute}},
		{"deafen", UpdateMemberInput{Deaf: &deaf}},
		{"rename", UpdateMemberInput{Nickname: &nickname}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.UpdateMember(ctx, userActor(f.mod), f.guildID, senior, tc.input)
			require.ErrorIs(t, err, ErrOutranked,
				"holding the permission is not the same as outranking the person")
		})
	}

	t.Run("kick", func(t *testing.T) {
		err := f.svc.RemoveMember(ctx, userActor(f.mod), f.guildID, senior)
		require.ErrorIs(t, err, ErrOutranked)
	})

	// Somebody below them is reachable by all four, so the refusals are about standing.
	t.Run("a member below is still reachable", func(t *testing.T) {
		_, err := f.svc.UpdateMember(ctx, userActor(f.mod), f.guildID, f.plain,
			UpdateMemberInput{Mute: &mute})
		require.NoError(t, err)
		require.NoError(t, f.svc.RemoveMember(ctx, userActor(f.mod), f.guildID, f.plain))
	})
}

// TestYouCanStillChangeYourOwnNickname is the carve-out, and its limit.
//
// Nobody outranks themselves, so widening the hierarchy check to every field of UpdateMember would have
// made renaming yourself impossible — a regression on an endpoint M12 shipped working. Self-mute and
// self-deafen get no exemption: those are the server-side bits a moderator applies, and clearing your own
// would undo the moderation they exist for.
func TestYouCanStillChangeYourOwnNickname(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel|roles.PermManageGuild)
	ctx := t.Context()

	nickname := "myself"
	_, err := f.svc.UpdateMember(ctx, userActor(f.plain), f.guildID, f.plain,
		UpdateMemberInput{Nickname: &nickname})
	require.NoError(t, err, "a member must be able to rename themselves")

	// Server-mute is a moderation bit, not a self-service one. Granted guild-wide and still refused,
	// because the target is the actor and nobody outranks themselves.
	f.exec(ctx, `UPDATE roles SET permissions = $1 WHERE id = $2`,
		(roles.PermViewChannel | roles.PermManageGuild | roles.PermMuteMembers).Int64(),
		int64(f.everyoneID))

	unmute := false
	_, err = f.svc.UpdateMember(ctx, userActor(f.plain), f.guildID, f.plain,
		UpdateMemberInput{Mute: &unmute})
	require.ErrorIs(t, err, ErrOutranked,
		"clearing your own server-mute would undo the moderation the bit exists for")
}

// TestYouCanAlwaysLeave is the one self-action exempt from the hierarchy entirely.
//
// Not because it is a demotion — removing a role looked like one too, and stopped being one when roles
// gained channel denies. Leaving forfeits every permission in the guild at once, so it cannot be a route
// to gaining one. That property is what earns the exemption.
func TestYouCanAlwaysLeave(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	// A member with no permissions at all, standing at the floor.
	require.NoError(t, f.svc.RemoveMember(ctx, userActor(f.plain), f.guildID, f.plain))

	// And the owner still cannot leave, because a guild without layer 2 has no layer 2 at all.
	err := f.svc.RemoveMember(ctx, userActor(f.owner), f.guildID, f.owner)
	require.ErrorIs(t, err, ErrCannotRemoveOwner)
}

// TestAnIdempotentRepeatWritesNoAuditEntry is the difference between succeeding and having done something.
//
// Assigning a role the member already holds succeeds, and so does removing one they do not have — that is
// what makes these verbs idempotent. Neither is a mutation, and rule 2 asks for an entry per mutation. An
// entry for a no-op puts "X was given role Y" in the log for a member who already had it, which is an
// operator reading a record of something that did not happen.
func TestAnIdempotentRepeatWritesNoAuditEntry(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	target := f.newRole(ctx, f.guildID, 2, 0)

	entries := func() int {
		var n int
		require.NoError(t, f.pool.QueryRow(ctx,
			`SELECT count(*) FROM audit_log_entries WHERE guild_id = $1 AND action = ANY($2)`,
			int64(f.guildID), []string{ActionMemberRoleAdd, ActionMemberRoleRemove}).Scan(&n))
		return n
	}

	require.Zero(t, entries())

	_, err := f.svc.AssignRole(ctx, userActor(f.mod), f.guildID, f.plain, target)
	require.NoError(t, err)
	require.Equal(t, 1, entries(), "the grant happened")

	_, err = f.svc.AssignRole(ctx, userActor(f.mod), f.guildID, f.plain, target)
	require.NoError(t, err, "the repeat still succeeds")
	require.Equal(t, 1, entries(), "and records nothing, because nothing changed")

	_, err = f.svc.UnassignRole(ctx, userActor(f.mod), f.guildID, f.plain, target)
	require.NoError(t, err)
	require.Equal(t, 2, entries(), "the removal happened")

	_, err = f.svc.UnassignRole(ctx, userActor(f.mod), f.guildID, f.plain, target)
	require.NoError(t, err, "removing what is not held still succeeds")
	require.Equal(t, 2, entries(), "and records nothing")
}

// TestAMemberWhoseViewComesFromAnOverwriteIsNotLockedIn is the configuration this milestone made
// buildable and the previous shape of these gates made a trap.
//
// @everyone withholds PermViewChannel at role level and one channel allows it back — an ordinary way to
// build a guild whose front door is a single welcome channel. Such a member resolves to nothing at guild
// level, so a listing gated on guild-level view refused them the listing its own per-channel filter would
// have answered correctly, and a leave gated the same way refused them the exit.
func TestAMemberWhoseViewComesFromAnOverwriteIsNotLockedIn(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, 0) // @everyone grants nothing at all
	ctx := t.Context()

	welcome := f.newChannel(ctx, f.guildID)
	hidden := f.newChannel(ctx, f.guildID)
	f.overwrite(ctx, welcome, roles.OverwriteTargetRole, f.everyoneID, roles.PermViewChannel, 0)

	got, err := f.svc.ListChannels(ctx, userActor(f.plain), f.guildID)
	require.NoError(t, err, "membership is what the listing needs; the filter decides the rest")

	ids := make([]snowflake.ID, 0, len(got))
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	require.ElementsMatch(t, []snowflake.ID{welcome}, ids,
		"the channel the overwrite allows, and only that one")
	require.NotContains(t, ids, hidden)

	// And they can leave. A permission check whose only job was to confirm membership must not be able to
	// refuse somebody who is plainly a member.
	require.NoError(t, f.svc.RemoveMember(ctx, userActor(f.plain), f.guildID, f.plain))

	// A non-member is still refused, so the gate did not become nothing.
	stranger := f.newUser(ctx, "stranger")
	_, err = f.svc.ListChannels(ctx, userActor(stranger), f.guildID)
	require.ErrorIs(t, err, httpx.ErrNotFound)
}

// TestParentIdRefusalsAreIndistinguishable closes the last of this milestone's three oracles.
//
// Three distinct messages told a caller iterating snowflakes which ids were live channels in their guild
// and what type each was. Harmless while every channel in a member's guild was listed to them; an oracle
// once the listing hides channels, which is the same correction the overwrite routes and the channel
// routes each needed.
func TestParentIdRefusalsAreIndistinguishable(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel|roles.PermManageChannels)
	ctx := t.Context()

	// A category the caller cannot see, and a plain channel they can.
	hiddenCategory := f.newCategory(ctx, f.guildID)
	f.overwrite(ctx, hiddenCategory, roles.OverwriteTargetRole, f.everyoneID, 0, roles.PermViewChannel)
	visibleText := f.channelID

	message := func(parent snowflake.ID) string {
		_, err := f.svc.CreateChannel(ctx, userActor(f.plain), f.guildID, CreateChannelInput{
			Name: "probe", Type: ChannelGuildText, ParentID: &parent,
		})
		require.Error(t, err)
		return err.Error()
	}

	nonexistent := message(f.next())
	hidden := message(hiddenCategory)
	wrongType := message(visibleText)

	require.Equal(t, nonexistent, hidden,
		"a category being hidden must not be distinguishable from it not existing")
	require.Equal(t, nonexistent, wrongType,
		"nor from a channel of the wrong type")

	// A parent the caller can actually use still works, so the refusals are about the parent.
	usable := f.newCategory(ctx, f.guildID)
	_, err := f.svc.CreateChannel(ctx, userActor(f.plain), f.guildID, CreateChannelInput{
		Name: "fine", Type: ChannelGuildText, ParentID: &usable,
	})
	require.NoError(t, err)
}

// TestAChannelYouCannotSeeIsNotYoursToManage is the bug M13's manual verification found after the tag,
// and the one no unit test on that branch was shaped to catch.
//
// The listing filters on PermViewChannel. Every channel-scoped mutation gated on its *own* permission,
// and an @everyone view-deny removes only the view bit — leaving the management bits intact. So a
// moderator got the channel omitted from their listing and could still rename it, delete it, and write
// its permission overwrites. Two code paths disagreeing about whether the same object existed.
//
// It needed a real guild to surface: every M13 test that hid a channel hid it from somebody holding
// nothing else, and every test that managed a channel managed a visible one. The divergence requires an
// actor who holds a management permission *and* lacks view in the same channel.
func TestAChannelYouCannotSeeIsNotYoursToManage(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	// The moderator holds both management bits guild-wide, and the channel denies @everyone the view bit
	// alone — which is exactly what locking a channel down looks like.
	f.exec(ctx, `UPDATE roles SET permissions = $1 WHERE id = $2`,
		(roles.PermViewChannel | roles.PermManageChannels | roles.PermManageRoles).Int64(),
		int64(f.modRole))

	hidden := f.newChannel(ctx, f.guildID)
	f.overwrite(ctx, hidden, roles.OverwriteTargetRole, f.everyoneID, 0, roles.PermViewChannel)

	// It is absent from their listing...
	listed, err := f.svc.ListChannels(ctx, userActor(f.mod), f.guildID)
	require.NoError(t, err)
	for _, c := range listed {
		require.NotEqual(t, hidden, c.ID, "the listing hides it")
	}

	// ...so every route that names it must agree, and answer as though it were not there.
	name := "renamed"
	_, err = f.svc.UpdateChannel(ctx, userActor(f.mod), hidden, UpdateChannelInput{Name: &name})
	require.ErrorIs(t, err, httpx.ErrNotFound, "renaming a channel hidden from you")

	err = f.svc.DeleteChannel(ctx, userActor(f.mod), hidden)
	require.ErrorIs(t, err, httpx.ErrNotFound, "deleting one")

	_, err = f.svc.SetOverwrite(ctx, userActor(f.mod), SetOverwriteInput{
		ChannelID: hidden, TargetType: roles.OverwriteTargetRole, TargetID: f.everyoneID,
		Allow: roles.PermSendMessages,
	})
	require.ErrorIs(t, err, httpx.ErrNotFound, "writing its permissions")

	err = f.svc.DeleteOverwrite(ctx, userActor(f.mod), hidden, roles.OverwriteTargetRole, f.everyoneID)
	require.ErrorIs(t, err, httpx.ErrNotFound, "or removing them")

	// And a channel they *can* see is still theirs to manage, so the refusal is about the view bit rather
	// than about the endpoint having stopped working.
	_, err = f.svc.UpdateChannel(ctx, userActor(f.mod), f.channelID, UpdateChannelInput{Name: &name})
	require.NoError(t, err)

	// The owner is unaffected: layer 2 short-circuits above layer 5.
	_, err = f.svc.UpdateChannel(ctx, userActor(f.owner), hidden, UpdateChannelInput{Name: &name})
	require.NoError(t, err)
}

// TestDenyingYourselfViewIsAOneWayDoor pins a consequence of requiring the view bit, so that it reads as
// a decision rather than as something nobody noticed.
//
// A member holding PermManageRoles can deny @everyone PermViewChannel on a channel in one request: the
// target is @everyone at position 0 so the standing check passes, and refuseEscalation passes because
// they hold the bit at the time of the write. From that moment they cannot see the channel, and therefore
// cannot reach any route that would undo it.
//
// Before M14 required the view bit they could have reversed their own change, because managing did not
// depend on seeing. That is the trade: the alternative is a moderator administering a channel their own
// listing refuses to mention, which is the bug this milestone opened with. Discord behaves the same way
// and its answer is the same — the owner, an administrator, or an Instance Admin repairs it.
func TestDenyingYourselfViewIsAOneWayDoor(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	_, err := f.svc.SetOverwrite(ctx, userActor(f.mod), SetOverwriteInput{
		ChannelID: f.channelID, TargetType: roles.OverwriteTargetRole, TargetID: f.everyoneID,
		Deny: roles.PermViewChannel,
	})
	require.NoError(t, err, "denying @everyone the view bit is an ordinary way to make a channel private")

	// And now the author cannot undo it.
	err = f.svc.DeleteOverwrite(ctx, userActor(f.mod), f.channelID, roles.OverwriteTargetRole, f.everyoneID)
	require.ErrorIs(t, err, httpx.ErrNotFound, "they can no longer see the channel they just hid")

	// The owner can, which is the recovery path and the reason this is a trade rather than a trap.
	err = f.svc.DeleteOverwrite(ctx, userActor(f.owner), f.channelID, roles.OverwriteTargetRole, f.everyoneID)
	require.NoError(t, err)
}
