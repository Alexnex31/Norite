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

// Resolution is everything one call to [Resolve] worked out about one account in one guild.
//
// It replaces the bare [Permission] this function used to return, and the reason is layer 4's second
// sentence. ADR 0008 says role position "governs who can manage whom", which needs the *highest* position
// among the roles a member holds — a number this query already has in hand, one column away from the
// permissions it was returning anyway. The alternative was a second round trip on every kick, mute,
// deafen, role edit, assignment and overwrite write.
//
// guilds.authorize argued against widening this signature to carry the owner id, on the grounds that
// reshaping the function the whole authority model rests on is a poor trade for saving one lookup on two
// cold paths. That argument does not carry here and the difference is worth stating: standing *is* layer
// 4, the same layer this function already implements, so carrying it is not a convenience bolted onto a
// permission check — it is the rest of the check.
type Resolution struct {
	// UserID is the account this resolution describes, carried so that nothing else has to be told again.
	//
	// [Resolution.IsOwner] and [Resolution.InChannel] used to take it as a parameter, which made it
	// possible — and on the channel-listing loop, easy — to resolve one account and then ask about
	// another: member-tier overwrites belonging to one person applied on top of another's roles and base
	// permissions, producing an answer that belonged to neither. Holding the id here makes that
	// unrepresentable rather than merely wrong, which is the same argument the base field below makes for
	// staying unexported.
	UserID snowflake.ID

	// Permissions is what the requested scope resolved to: layers 2 through 4 for a guild-level call, and
	// layer 5 applied on top when a channel was named.
	Permissions Permission

	// OwnerID is the guild's owner, which layer 2 needs and which every caller would otherwise re-read.
	OwnerID snowflake.ID

	// highestPosition is the account's standing: the highest position among the roles they hold.
	//
	// Unexported, and read only through [Resolution.Outranks], because on its own it is a number that
	// invites exactly one mistake. It is **meaningless when the account is the owner**, who is above the
	// hierarchy rather than placed in it, and is left at the zero value there — and zero is not an
	// obviously-empty value. It is @everyone's position, a perfectly valid standing, and the floor. So a
	// comparison that forgets to ask about ownership first does not fail loudly: it finds the owner at the
	// bottom, unable to moderate their own guild, which reads as a permission bug and invites the repair
	// that hands the guild to whoever holds the highest role.
	//
	// The first version of this field was exported with a comment saying to ask [Resolution.IsOwner] first.
	// A comment cannot fail to compile at any of the nine call sites this milestone adds, and this package
	// has had to make that lesson structural three times already — revokeEverything, RequireLiveSession,
	// factorProof. Outranks folds the ownership question in so there is nothing to remember.
	//
	// A member holding [PermAdministrator] *does* get a real position here, because layer 3 short-circuits
	// permissions and says nothing about standing (ADR 0008 puts the two in different layers). That return
	// sits after the loop that computes the position, so the value exists and dropping it would be the
	// same silent floor by a different route.
	highestPosition int32

	// base is layers 2 through 4 with no overwrite applied, kept so [Resolution.InChannel] can resolve any
	// number of channels from one guild-level query.
	//
	// Unexported and separate from Permissions on purpose: were InChannel to fold overwrites into whatever
	// Permissions currently holds, calling it on a resolution that already named a channel would apply a
	// second channel's overwrites on top of the first. Storing the base makes that impossible rather than
	// forbidden, which is the difference between a rule and a comment.
	base Permission

	// bypassed records that layer 2 or 3 short-circuited, so no overwrite applies at any channel.
	bypassed bool

	heldRoleIDs    map[int64]struct{}
	everyoneRoleID int64
}

// IsOwner reports whether the resolved account owns the guild — ADR 0008 layer 2.
func (r Resolution) IsOwner() bool {
	return r.OwnerID != 0 && r.OwnerID == r.UserID
}

// Standing is the resolved account's position in the hierarchy, for a caller that needs to report or
// compare it against something other than another Resolution — the target of a kick, or the role being
// assigned, neither of which is resolved.
//
// Prefer [Resolution.Outranks], which is the same number with the ownership question already asked.
func (r Resolution) Standing() int32 {
	return r.highestPosition
}

// Outranks reports whether this account may act on something standing at the given position — ADR 0008
// layer 4's second sentence, which is the whole of the hierarchy rule.
//
// **Strictly greater**, and the strictness is load-bearing: equal standing must not permit acting, or two
// members whose highest role is the same role could kick each other, and a role at your own highest
// position would be editable by you. It also makes the floor behave: @everyone is position 0, so a member
// holding no other role cannot act on another member holding no other role.
//
// The guild owner outranks everything, unconditionally and without their position being consulted, because
// they have none — layer 2 sits above the hierarchy rather than in it. That check is inside this method
// rather than at its call sites for the reason highestPosition documents.
//
// Layer 1 is not here and cannot be: an Instance Admin is never resolved against a guild, so there is no
// Resolution to ask. Callers hold that tier separately and check it alongside this — see guilds.decision.
//
// This is the form for acting on a **role**, whose position is all there is to it. Acting on a *member*
// goes through [Resolution.OutranksMember], which has one more question to ask.
func (r Resolution) Outranks(position int32) bool {
	return r.IsOwner() || r.highestPosition > position
}

// OutranksMember reports whether this account may act on another member of the same guild.
//
// The extra question is the guild owner, and it cannot be answered by comparing standings — which is the
// trap this method exists to remove rather than document. An owner's own standing is a meaningless zero
// (see highestPosition), so passing it to [Resolution.Outranks] reports that anybody holding any role
// outranks the guild's owner: the exact inversion of layer 2. That is not a subtle failure, it is a
// moderator kicking the owner out of their own guild, and the first version of the primitive had it.
//
// Found by writing the test rather than by reading the code, which is worth recording: the assertion
// "a moderator does not outrank the owner" is the natural thing to write, and it is the one that fails.
func (r Resolution) OutranksMember(targetID snowflake.ID, targetStanding int32) bool {
	if targetID == r.OwnerID {
		// Nobody inside the guild acts on its owner. An Instance Admin still may, and holds that tier
		// outside this type entirely.
		return false
	}
	return r.Outranks(targetStanding)
}

// InChannel applies one channel's overwrites to an already-resolved guild-level result.
//
// This exists for the channel listing, which has to answer "can this account see it" for every channel in
// a guild. Calling [Resolve] once per channel would be one authority query and one overwrite query each —
// the N+1 §15.2 names — where the authority is identical for all of them. So the listing resolves once at
// guild level, reads every channel's overwrites in one query, and calls this per channel.
//
// An owner or an administrator resolved through a short-circuit is returned unchanged, because layers 2
// and 3 sit above layer 5: no overwrite denies them anything. That check lives here rather than at the
// call site so a caller cannot filter a channel away from the one account that must always see it.
func (r Resolution) InChannel(overwrites []db.PermissionOverwrite) Permission {
	if r.bypassed {
		return r.Permissions
	}
	return applyOverwrites(r.base, overwrites, r.heldRoleIDs, r.everyoneRoleID, r.UserID)
}

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
func Resolve(
	ctx context.Context, q db.Querier, guildID, userID, channelID snowflake.ID,
) (Resolution, error) {
	rows, err := q.ListGuildMemberAuthority(ctx, db.ListGuildMemberAuthorityParams{
		ID:     int64(guildID),
		UserID: int64(userID),
	})
	if err != nil {
		return Resolution{}, fmt.Errorf("roles: load guild authority: %w", err)
	}

	// No rows means no guild. The LEFT joins guarantee a row for a guild that exists even when the actor
	// is in it with no roles, so an empty result is unambiguous.
	if len(rows) == 0 {
		return Resolution{}, ErrNotAMember
	}

	res := Resolution{UserID: userID, OwnerID: snowflake.ID(rows[0].OwnerID)}

	// Layer 2. Checked before membership on purpose: an owner is always a member in practice, but a
	// resolution that depended on that would fail closed in exactly the situation — a half-written guild,
	// a membership row removed by hand — where an owner is the only person who could repair it.
	//
	// HighestPosition stays zero here and means nothing; see its own comment for why that is stated rather
	// than papered over with a sentinel.
	if res.IsOwner() {
		res.Permissions = permAll
		res.base = permAll
		res.bypassed = true
		return res, nil
	}

	if !rows[0].IsMember {
		return Resolution{}, ErrNotAMember
	}

	// Layer 4, the standing layer 4's second sentence needs, and the ids layer 5 will need. RoleID is nil
	// when the member holds no roles and the guild has no default — which should not happen, since guild
	// creation writes @everyone in the same transaction, but a nil dereference here would be a panic in
	// the middle of a permission check.
	res.heldRoleIDs = make(map[int64]struct{}, len(rows))

	for _, row := range rows {
		if row.RoleID == nil {
			continue
		}

		res.heldRoleIDs[*row.RoleID] = struct{}{}

		if row.RolePermissions != nil {
			res.base = res.base.Add(PermissionFromInt64(*row.RolePermissions))
		}

		// Standing is the highest position held, and @everyone is included rather than excluded: it sits
		// at 0, so a member holding nothing else lands on the floor, which is exactly where the hierarchy
		// rules put them.
		if row.RolePosition != nil && *row.RolePosition > res.highestPosition {
			res.highestPosition = *row.RolePosition
		}

		if row.RoleIsDefault != nil && *row.RoleIsDefault {
			res.everyoneRoleID = *row.RoleID
		}
	}

	res.Permissions = res.base

	// Layer 3. After the OR rather than inside the loop, because the bit may come from any role the member
	// holds and short-circuiting on the first one that has it would give the same answer more obscurely.
	//
	// HighestPosition survives this return. An administrator is above every *permission* check and is
	// placed in the hierarchy like anybody else, so somebody above them can still act on them — and this
	// return sits after the loop, so the position is already computed and discarding it would silently put
	// every administrator on the floor.
	if res.base.Has(PermAdministrator) {
		res.Permissions = permAll
		res.bypassed = true
		return res, nil
	}

	if channelID == 0 {
		return res, nil
	}

	overwrites, err := q.ListChannelPermissionOverwrites(ctx, db.ListChannelPermissionOverwritesParams{
		ChannelID: int64(channelID),
		GuildID:   int64(guildID),
	})
	if err != nil {
		return Resolution{}, fmt.Errorf("roles: load channel overwrites: %w", err)
	}

	res.Permissions = res.InChannel(overwrites)

	return res, nil
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
	overwrites []db.PermissionOverwrite,
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
