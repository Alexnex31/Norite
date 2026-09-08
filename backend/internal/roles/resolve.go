// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package roles

import (
	"context"
	"errors"
	"fmt"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// Overwrite target types, as stored in permission_overwrites.target_type.
//
// Two columns rather than two nullable foreign keys, because an overwrite targets exactly one of a role
// and a member — see migration 000015. A value that is neither is ignored by [Resolve] rather than
// treated as an error: a row written by a newer schema must not make an older binary refuse to resolve
// permissions at all, which would take the guild down rather than degrade it.
const (
	OverwriteTargetRole   int16 = 0
	OverwriteTargetMember int16 = 1
)

// ErrNotAMember reports that the actor holds no membership in the guild being resolved — either because
// they are not in it, or because the guild does not exist.
//
// # Why one error covers both
//
// The two cases are deliberately not distinguished, and a caller must not try to. Guild ids are
// snowflakes, which are sequential and carry their own creation time, so answering 404 for a missing guild
// and 403 for a guild the caller is not in turns any list of plausible ids into a map of which guilds
// exist on the instance. M11 settled the same question for session ids and for the same reason.
//
// What this does *not* cover is a member who holds the guild but lacks a permission. They already know the
// guild exists, so refusing them with a distinct answer leaks nothing — see guilds.Service.authorize,
// which maps this to 404 and a failed permission check to 403.
var ErrNotAMember = errors.New("roles: actor is not a member of this guild")

// Resolve computes the effective permissions of one account in one guild, optionally within one channel.
//
// It implements ADR 0008 layers 2 through 5, in the order the ADR fixes:
//
//  2. the guild owner bypasses every check within their guild;
//  3. a role carrying [PermAdministrator] short-circuits the same way, scoped to that guild;
//  4. otherwise the permissions of every role the member holds are OR'd together, including @everyone;
//  5. then the channel's overwrites are applied most-specific-last — @everyone, then the union of the
//     member's role overwrites, then the member's own — with deny applied before allow at each tier.
//
// Layer 1 (Instance Admin) is not implemented here and must not be. See the package comment.
//
// channelID may be zero, meaning a guild-level check with no channel in the request path. That skips the
// overwrite query entirely rather than running it against an id that matches nothing — one round trip
// instead of two on every guild-scoped mutation.
//
// Returns [ErrNotAMember] when the actor holds no membership, which includes the guild not existing.
func Resolve(ctx context.Context, q db.Querier, guildID, userID, channelID snowflake.ID) (Permission, error) {
	rows, err := q.ListGuildMemberAuthority(ctx, db.ListGuildMemberAuthorityParams{
		ID:     int64(guildID),
		UserID: int64(userID),
	})
	if err != nil {
		return 0, fmt.Errorf("roles: load guild authority: %w", err)
	}

	// No rows means no guild. The LEFT joins guarantee a row for a guild that exists even when the actor
	// is in it with no roles, so an empty result is unambiguous.
	if len(rows) == 0 {
		return 0, ErrNotAMember
	}

	// Layer 2. Checked before membership on purpose: an owner is always a member in practice, but a
	// resolution that depended on that would fail closed in exactly the situation — a half-written guild,
	// a membership row removed by hand — where an owner is the only person who could repair it.
	if snowflake.ID(rows[0].OwnerID) == userID {
		return permAll, nil
	}

	if !rows[0].IsMember {
		return 0, ErrNotAMember
	}

	// Layer 4, and the ids layer 5 will need. RoleID is nil when the member holds no roles and the guild
	// has no default — which should not happen, since guild creation writes @everyone in the same
	// transaction, but a nil dereference here would be a panic in the middle of a permission check.
	var (
		base           Permission
		heldRoleIDs    = make(map[int64]struct{}, len(rows))
		everyoneRoleID int64
	)

	for _, row := range rows {
		if row.RoleID == nil {
			continue
		}

		heldRoleIDs[*row.RoleID] = struct{}{}

		if row.RolePermissions != nil {
			base = base.Add(PermissionFromInt64(*row.RolePermissions))
		}

		if row.RoleIsDefault != nil && *row.RoleIsDefault {
			everyoneRoleID = *row.RoleID
		}
	}

	// Layer 3. After the OR rather than inside the loop, because the bit may come from any role the member
	// holds and short-circuiting on the first one that has it would give the same answer more obscurely.
	if base.Has(PermAdministrator) {
		return permAll, nil
	}

	if channelID == 0 {
		return base, nil
	}

	overwrites, err := q.ListChannelPermissionOverwrites(ctx, db.ListChannelPermissionOverwritesParams{
		ChannelID: int64(channelID),
		GuildID:   int64(guildID),
	})
	if err != nil {
		return 0, fmt.Errorf("roles: load channel overwrites: %w", err)
	}

	return applyOverwrites(base, overwrites, heldRoleIDs, everyoneRoleID, userID), nil
}

// applyOverwrites is ADR 0008 layer 5, split out so the precedence can be read in one screen and tested
// without a database.
//
// # The tiers are not a sort
//
// @everyone applies first, then role overwrites, then the member's own — but the middle tier is an
// *accumulation* across every role the member holds, not an ordering among them. Two roles' overwrites are
// unioned and neither wins: all their denies are collected, all their allows are collected, and the denies
// are applied before the allows so an explicit allow on any held role beats a deny on another. Applying
// role overwrites one at a time in some order would make the result depend on that order, which the ADR
// does not define and the database does not preserve.
//
// Within each tier deny is applied before allow, so the more specific tier's allow can restore what a
// broader deny removed. That is what "most specific wins" means operationally.
func applyOverwrites(
	base Permission,
	overwrites []db.ListChannelPermissionOverwritesRow,
	heldRoleIDs map[int64]struct{},
	everyoneRoleID int64,
	userID snowflake.ID,
) Permission {
	var (
		roleAllow, roleDeny     Permission
		memberAllow, memberDeny Permission
	)

	for _, ow := range overwrites {
		allow := PermissionFromInt64(ow.Allow)
		deny := PermissionFromInt64(ow.Deny)

		switch {
		case ow.TargetType == OverwriteTargetRole && ow.TargetID == everyoneRoleID:
			// The @everyone tier, applied immediately: it is the broadest and everything below it must be
			// able to override what it does.
			base = base.Remove(deny).Add(allow)

		case ow.TargetType == OverwriteTargetRole:
			if _, held := heldRoleIDs[ow.TargetID]; held {
				roleAllow = roleAllow.Add(allow)
				roleDeny = roleDeny.Add(deny)
			}

		case ow.TargetType == OverwriteTargetMember && ow.TargetID == int64(userID):
			memberAllow = memberAllow.Add(allow)
			memberDeny = memberDeny.Add(deny)
		}
	}

	base = base.Remove(roleDeny).Add(roleAllow)
	base = base.Remove(memberDeny).Add(memberAllow)

	return base
}
