// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/guildauth"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// M16b's recording switch is the only field on PATCH /guilds/{guild_id} whose authority differs from the
// rest of the request, and it differs in two directions at once: it needs *more* than PermManageGuild
// (the owner) and it is refused to an authority that outranks the owner everywhere else (layer 1).
//
// The tests below are deliberately one property each rather than one test for "the authority", because
// the interesting failures are the ones only a single case can see: a check written as `!decision.Owns()`
// passes everything here except TestAnInstanceAdminWhoOwnsTheGuildStillMayFlipIt, and it refuses an
// instance operator their *own* guild. No count is given for them, for the reason CLAUDE.md's suffixed-
// milestone list is a standing warning — a number that enumerates its own members goes stale silently on
// the next addition.

// TestOnlyTheOwnerMayFlipTheRecordingSwitch is the ordinary case: PermManageGuild renames a guild and does
// not decide whether its members are recorded.
//
// The refusal is 403 rather than 404 because the caller is a member who can see the guild — M12's split —
// and it must leave the setting alone, which is the half a status-code assertion alone would miss.
func TestOnlyTheOwnerMayFlipTheRecordingSwitch(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	// A role carrying PermManageGuild, which is everything this endpoint asks for on every other field.
	manager := f.newRole(ctx, f.guildID, 7, roles.PermManageGuild|roles.PermViewChannel)
	f.grantRole(ctx, f.guildID, f.plain, manager)

	on := true
	_, err := f.svc.Update(ctx, userActor(f.plain), f.guildID, UpdateGuildInput{MessageAuditEnabled: &on})
	require.ErrorIs(t, err, httpx.ErrForbidden,
		"PermManageGuild renames a guild; it must not decide whether its members are recorded")

	after, err := f.svc.Get(ctx, userActor(f.owner), f.guildID)
	require.NoError(t, err)
	require.False(t, after.MessageAuditEnabled,
		"a refused request must change nothing — the check has to run before the UPDATE, not after it")

	// And the same actor may still do what PermManageGuild is for, so the refusal is scoped to the field
	// rather than to the request. Without this the test above passes against a service that refuses the
	// whole endpoint to anyone but the owner.
	name := "renamed by the manager"
	_, err = f.svc.Update(ctx, userActor(f.plain), f.guildID, UpdateGuildInput{Name: &name})
	require.NoError(t, err, "the refusal must be about the field, not about the endpoint")
}

// TestAnInstanceAdminMayNotStartRecordingAGuild is the layer-1 refusal, and it is the first place in this
// codebase where the instance tier is narrower than a guild's owner.
//
// Rule 14 requires every Instance Admin action to reach instance_audit_log, which is M72's table and does
// not exist — so the tier could otherwise switch recording on for a guild it has never joined and leave
// nothing anywhere that says it did. See mayFlipMessageAudit for why the refusal is temporary and why
// lifting it later is the additive direction.
func TestAnInstanceAdminMayNotStartRecordingAGuild(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	admin := f.newUser(ctx, "instance-admin")
	f.makeInstanceAdmin(ctx, admin)

	on := true
	_, err := f.svc.Update(ctx, userActor(admin), f.guildID, UpdateGuildInput{MessageAuditEnabled: &on})
	require.ErrorIs(t, err, httpx.ErrForbidden,
		"an Instance Admin must not be able to start recording a guild while rule 14 has nowhere to "+
			"record that they did — see mayFlipMessageAudit")

	after, err := f.svc.Get(ctx, userActor(f.owner), f.guildID)
	require.NoError(t, err)
	require.False(t, after.MessageAuditEnabled)
}

// TestAnInstanceAdminWhoOwnsTheGuildStillMayFlipIt is the other half, and it is the one a check written
// the obvious way fails.
//
// guildauth.Authorize short-circuits at layer 1 and returns a Decision with a *zero* resolution, because
// an Instance Admin is not a member and resolving them would be ADR 0008's conflation. So
// `decision.Owns()` is false for an Instance Admin **even when they own the guild**, and a refusal
// written against it locks an instance operator out of a guild they created themselves. That reads as a
// permission bug and invites the repair that hands the tier the capability the test above withholds.
//
// The test above passes against that broken check. This one does not, which is why both exist.
func TestAnInstanceAdminWhoOwnsTheGuildStillMayFlipIt(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	f.makeInstanceAdmin(ctx, f.owner)

	// The fact this test turns on, asserted rather than described. Without it the test below reads as
	// "an owner may flip the switch", which is already covered — what makes it worth its own case is
	// that for *this* actor the Decision reports no ownership at all, so an implementation reading
	// ownership from the Decision refuses somebody the guilds table says is the owner.
	decision, err := guildauth.Authorize(
		ctx, db.New(f.pool), userActor(f.owner), f.guildID, 0, roles.PermViewChannel)
	require.NoError(t, err)
	require.True(t, decision.InstanceAdmin())
	require.False(t, decision.Owns(),
		"layer 1 short-circuits to a zero resolution, so Decision.Owns() is false even for the owner — "+
			"this is the trap mayFlipMessageAudit exists to avoid, and if this ever becomes true the "+
			"function can be simplified")

	on := true
	updated, err := f.svc.Update(ctx, userActor(f.owner), f.guildID, UpdateGuildInput{MessageAuditEnabled: &on})
	require.NoError(t, err,
		"ownership is read off the guild row, never from Decision.Owns(), which is false for layer 1 "+
			"whatever the row says")
	require.True(t, updated.MessageAuditEnabled)
}

// TestAMemberIsToldWhetherTheGuildRecordsThem is the milestone's structural obligation, and it is the one
// that M62a's screen `6e` cannot be drawn without.
//
// The flag has to be readable by any member, not by somebody holding a permission, or the screen cannot
// state the guild's status in both directions — and stating only the positive case would make the absence
// of a warning carry a meaning nothing guarantees.
//
// Both states are asserted, because a field that is merely present is not the property: a client has to
// be able to say "this guild does not record" as positively as it says the opposite.
func TestAMemberIsToldWhetherTheGuildRecordsThem(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	// f.plain holds nothing beyond @everyone's PermViewChannel: no PermManageGuild, no
	// PermViewMessageAudit, not the owner.
	before, err := f.svc.Get(ctx, userActor(f.plain), f.guildID)
	require.NoError(t, err)
	require.False(t, before.MessageAuditEnabled, "the off state must be readable, not merely absent")

	on := true
	_, err = f.svc.Update(ctx, userActor(f.owner), f.guildID, UpdateGuildInput{MessageAuditEnabled: &on})
	require.NoError(t, err)

	after, err := f.svc.Get(ctx, userActor(f.plain), f.guildID)
	require.NoError(t, err)
	require.True(t, after.MessageAuditEnabled,
		"a member with no permissions must be able to learn that they are being recorded")
}

// TestFlippingTheSwitchIsAuditedInBothDirections is the roadmap's done-when, and the negative case is the
// one with no other home.
//
// Turning recording off without a trace would make this the one setting somebody could change, act under,
// and change back. Turning it on is audited too, because a member's expectations about who reads their
// messages just changed.
//
// The third assertion is the one that would not exist unless somebody decided to write it: sending the
// value the guild already has writes neither verb. M14 settled that a field sent with the value it
// already had is not a change, and an entry announcing that recording was enabled when it was already
// enabled is exactly the noise a real entry could be hidden in.
func TestFlippingTheSwitchIsAuditedInBothDirections(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()
	owner := userActor(f.owner)

	on, off := true, false
	_, err := f.svc.Update(ctx, owner, f.guildID, UpdateGuildInput{MessageAuditEnabled: &on})
	require.NoError(t, err)
	_, err = f.svc.Update(ctx, owner, f.guildID, UpdateGuildInput{MessageAuditEnabled: &off})
	require.NoError(t, err)

	// Already off, so this one must record nothing.
	_, err = f.svc.Update(ctx, owner, f.guildID, UpdateGuildInput{MessageAuditEnabled: &off})
	require.NoError(t, err)

	entries, err := f.svc.ListAuditLog(ctx, owner, f.guildID, ListAuditLogInput{Limit: maxAuditLogPageSize})
	require.NoError(t, err)

	counts := map[string]int{}
	for _, e := range entries {
		counts[e.Action]++
	}

	require.Equal(t, 1, counts[ActionGuildMessageAuditEnable], "turning it on is audited, exactly once")
	require.Equal(t, 1, counts[ActionGuildMessageAuditDisable], "turning it off is audited, exactly once")

	// One guild.update, from the third request alone.
	//
	// The first two flipped the switch and wrote their verb and nothing else, which is the property this
	// assertion is really about. The third carried the value the guild already had, so it wrote no toggle
	// verb — and it wrote an empty guild.update, exactly as a no-op rename has since M12. That last part
	// is deliberately left alone: this milestone decided what a *toggle* records, not what an endpoint
	// does with a request that changes nothing, and changing the second under cover of the first is how
	// an unrelated behavior goes out in a milestone nobody would think to look in.
	//
	// The first draft of this test asserted zero and was wrong rather than the code being wrong, which is
	// worth leaving written down: the expectation came from the design note, and the design note was
	// about the flipping case.
	require.Equal(t, 1, counts[ActionGuildUpdate],
		"a request that flips the switch writes the toggle verb alone; only the no-op request writes "+
			"guild.update, as any no-op on this endpoint has since M12")
}

// TestAChangeArrivingBesideTheToggleIsStillAudited is the assertion the first version of Update's
// entry-writing condition would have passed and its successor exists for.
//
// That condition began as a hand-maintained list of the fields `guild.update` owns —
// `in.Name != nil || in.Description != nil || in.ClearDescription` — which is correct until somebody adds
// a field, gives it a `changes.changed(...)` line, and does not think to extend the list. From then on any
// request carrying that field *and* flipping recording writes the toggle verb alone and the real change
// goes unaudited, which is rule 2 broken in the one request that is also switching the recording off.
// M62a's per-guild preferences are the obvious next field.
//
// The condition is derived from the diff now, so it cannot drift from the diff. This test drives the
// shape rather than the mechanism: a change and a flip in one request must produce both entries, whatever
// the condition is written as.
func TestAChangeArrivingBesideTheToggleIsStillAudited(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()
	owner := userActor(f.owner)

	name, on := "renamed while switching recording on", true
	_, err := f.svc.Update(ctx, owner, f.guildID, UpdateGuildInput{Name: &name, MessageAuditEnabled: &on})
	require.NoError(t, err)

	entries, err := f.svc.ListAuditLog(ctx, owner, f.guildID, ListAuditLogInput{Limit: maxAuditLogPageSize})
	require.NoError(t, err)

	counts := map[string]int{}
	var update AuditLogEntry
	for _, e := range entries {
		counts[e.Action]++
		if e.Action == ActionGuildUpdate {
			update = e
		}
	}

	require.Equal(t, 1, counts[ActionGuildMessageAuditEnable], "the flip is recorded")
	require.Equal(t, 1, counts[ActionGuildUpdate],
		"and so is the rename — a change arriving alongside the toggle must never be the one that is "+
			"dropped, which is what a hand-maintained field list would eventually do")
	require.Contains(t, string(update.Changes), "renamed while switching recording on",
		"the guild.update entry must carry the rename it is recording, not be an empty shell")
}
