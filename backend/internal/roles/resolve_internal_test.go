// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package roles

import (
	"testing"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// The overwrite tier tests live in-package and take no database, because both properties they pin are
// properties of *ordering* — and a query returns rows in whatever order Postgres finds them. Driving
// applyOverwrites directly is the only way to assert that the answer does not depend on that order, which
// is exactly what the accumulation exists to guarantee.
//
// Both tests below were written after the database-level tests failed to catch a deliberately broken
// implementation. That is what they are for.

const (
	everyoneID = int64(100)
	heldID     = int64(200)
	otherID    = int64(300)
	memberID   = snowflake.ID(400)
)

func overwriteRow(targetType int16, targetID int64, allow, deny Permission) db.ListChannelPermissionOverwritesRow {
	return db.ListChannelPermissionOverwritesRow{
		TargetType: targetType,
		TargetID:   targetID,
		Allow:      allow.Int64(),
		Deny:       deny.Int64(),
	}
}

func held() map[int64]struct{} {
	// @everyone is returned by the authority query alongside the roles a member actually holds, so it is
	// in this set too. That is precisely why the everyoneRoleID parameter is needed: without it @everyone
	// is indistinguishable from any other held role.
	return map[int64]struct{}{everyoneID: {}, heldID: {}}
}

// TestAnEveryoneAllowIsOverriddenByARoleDeny is the case that separates the @everyone tier from the role
// tier, and the only one that does.
//
// Every other combination gives the same answer whether @everyone is applied as its own tier or lumped in
// with the role accumulation, because deny is applied before allow within a tier — so an @everyone deny
// and a role allow produce "allowed" either way. Reverse the polarity and the two implementations
// disagree:
//
//	tiered      @everyone allow lands in base, then the role deny removes it   -> denied
//	lumped      both go in one bucket, deny before allow                       -> allowed
//
// ADR 0008 fixes the first: most specific wins, and a role is more specific than @everyone.
//
// Confirmed by removal — collapse the @everyone case into the role case in applyOverwrites and this fails.
func TestAnEveryoneAllowIsOverriddenByARoleDeny(t *testing.T) {
	got := applyOverwrites(
		0,
		[]db.ListChannelPermissionOverwritesRow{
			overwriteRow(OverwriteTargetRole, everyoneID, PermSendMessages, 0),
			overwriteRow(OverwriteTargetRole, heldID, 0, PermSendMessages),
		},
		held(), everyoneID, memberID,
	)

	if got.Has(PermSendMessages) {
		t.Errorf("a role deny must override an @everyone allow; got %d", got)
	}
}

// TestRoleOverwritesDoNotDependOnRowOrder pins the accumulation.
//
// Two roles the member holds, one allowing and one denying the same bit. The ADR unions them and applies
// deny before allow, so the allow wins — and critically, it wins whichever order the rows arrive in, since
// permission_overwrites has no ordering and the query deliberately does not impose one.
//
// Confirmed by removal: apply role overwrites to base one at a time inside the loop instead of
// accumulating, and exactly one of these two subtests fails. Which one depends on the mutation, which is
// the whole point of testing both.
func TestRoleOverwritesDoNotDependOnRowOrder(t *testing.T) {
	allowRow := overwriteRow(OverwriteTargetRole, heldID, PermSendMessages, 0)
	denyRow := overwriteRow(OverwriteTargetRole, otherID, 0, PermSendMessages)

	for _, tc := range []struct {
		name string
		rows []db.ListChannelPermissionOverwritesRow
	}{
		{"allow first", []db.ListChannelPermissionOverwritesRow{allowRow, denyRow}},
		{"deny first", []db.ListChannelPermissionOverwritesRow{denyRow, allowRow}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			heldBoth := map[int64]struct{}{everyoneID: {}, heldID: {}, otherID: {}}

			got := applyOverwrites(0, tc.rows, heldBoth, everyoneID, memberID)
			if !got.Has(PermSendMessages) {
				t.Errorf("an allow on one held role must beat a deny on another regardless of row order; got %d", got)
			}
		})
	}
}

// TestAMemberOverwriteBeatsEveryRoleTier covers the most specific tier, in both row orders for the same
// reason as above.
func TestAMemberOverwriteBeatsEveryRoleTier(t *testing.T) {
	roleDeny := overwriteRow(OverwriteTargetRole, heldID, 0, PermSendMessages)
	memberAllow := overwriteRow(OverwriteTargetMember, int64(memberID), PermSendMessages, 0)

	for _, tc := range []struct {
		name string
		rows []db.ListChannelPermissionOverwritesRow
	}{
		{"role first", []db.ListChannelPermissionOverwritesRow{roleDeny, memberAllow}},
		{"member first", []db.ListChannelPermissionOverwritesRow{memberAllow, roleDeny}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := applyOverwrites(0, tc.rows, held(), everyoneID, memberID)
			if !got.Has(PermSendMessages) {
				t.Errorf("the member tier is the most specific and must win; got %d", got)
			}
		})
	}
}

// TestAMemberDenyBeatsAnEveryoneAndRoleAllow is the member tier's other polarity, and it is the case that
// would silently pass if the member tier were folded into the role accumulation.
func TestAMemberDenyBeatsAnEveryoneAndRoleAllow(t *testing.T) {
	got := applyOverwrites(
		0,
		[]db.ListChannelPermissionOverwritesRow{
			overwriteRow(OverwriteTargetRole, everyoneID, PermSendMessages, 0),
			overwriteRow(OverwriteTargetRole, heldID, PermSendMessages, 0),
			overwriteRow(OverwriteTargetMember, int64(memberID), 0, PermSendMessages),
		},
		held(), everyoneID, memberID,
	)

	if got.Has(PermSendMessages) {
		t.Errorf("a member deny must override allows at every broader tier; got %d", got)
	}
}

// TestAnUnknownTargetTypeIsIgnored covers a row written by a newer schema.
//
// Refusing to resolve at all would take a guild down rather than degrade it, so an unrecognized
// target_type contributes nothing and resolution continues.
func TestAnUnknownTargetTypeIsIgnored(t *testing.T) {
	got := applyOverwrites(
		PermViewChannel,
		[]db.ListChannelPermissionOverwritesRow{
			overwriteRow(int16(7), heldID, PermBanMembers, 0),
		},
		held(), everyoneID, memberID,
	)

	if got != PermViewChannel {
		t.Errorf("an unknown target type must contribute nothing; got %d, want %d", got, PermViewChannel)
	}
}
