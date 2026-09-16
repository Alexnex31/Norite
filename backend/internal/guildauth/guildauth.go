// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package guildauth decides whether an actor may act inside a guild or one of its channels.
//
// # Why this is not in guilds
//
// It was, until M15. M12 built the chokepoint before its call sites so that authority is decided in one
// place, and that worked while `guilds` was the only package asking the question. It is not any more:
// `architecture.md`'s package tree gives `messages`, `reports`, `tags` and `whispers` each their own
// package, and every one of them acts on an object inside a channel and needs the same answer.
//
// The alternative was exporting the chokepoint from `guilds` and having the other four import it, which
// makes `guilds` the import root of the whole domain — how a modular monolith becomes one package with
// satellites. Extracting costs one refactor now, against four packages coupled to a fifth forever.
//
// The move changed no behavior. Every function body is byte-identical to what it replaced, checked by
// re-applying the renames to the old files and diffing. The documentation did not survive as cleanly and
// the first version of this comment overstated it: AuthorizeChannel's doc was left behind in `guilds`
// attached to an unrelated function, guildOf lost its scope-boundary paragraph, and the refusal contract
// stayed on the wrapper. A review found all three and they are restored here.
//
// What the boundary genuinely forced: exported names, an InstanceAdmin accessor for the two callers that
// read the field directly, and — added after the extraction, not moved — AuthorizeChannelUnlocked, because
// "all four callers are mutations" stopped being true the moment this became importable.
package guildauth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// Decision is what Authorize resolved on the way to allowing a request.
//
// It exists so a caller that needs the *same* facts again does not fetch them again. CreateRole and
// UpdateRole both have to ask a second question — "may this actor grant these permissions" — and asking it
// independently meant re-running IsInstanceAdmin and a whole roles.Resolve inside a transaction that had
// just run both, five round trips for one INSERT.
//
// The two fields are kept separate rather than collapsed into a permission value, because an Instance
// Admin holds no guild permissions at all (ADR 0008) and representing them as "holds everything" is the
// conflation the layer separation exists to prevent.
type Decision struct {
	// instanceAdmin is layer 1: authority from outside the guild entirely.
	instanceAdmin bool
	// resolution is what layers 2..5 worked out: the permissions, the owner, and the actor's standing.
	//
	// The zero value when instanceAdmin is true, because an Instance Admin is not a member and was never
	// resolved — and that matters more than an empty permission set would. An empty permission set reads
	// as absence; a standing of zero reads as data, because zero is @everyone's position and a real place
	// in the hierarchy. Which is why nothing reads the standing directly: roles.Resolution.Outranks asks
	// about ownership first, and the wrapper described below asks about the tier before that.
	//
	// This was two fields until a review pointed out they always held the same value — a `permissions`
	// copy read by allows, and the resolution read by owns — each with its own explanation of when it was
	// meaningless. One fact, one field.
	resolution roles.Resolution
}

// A note on a round trip that was considered, kept, and then removed by something else.
//
// RemoveMember and Delete each called GetGuild and then Authorize, which runs
// ListGuildMemberAuthority — a query returning owner_id as its first column. Both paid a redundant read
// for the one fact they needed, inside an open write transaction against a deliberately small pool
// (§15.3). M12 considered removing it and kept it, because doing so meant widening roles.Resolve's return
// so that two cold paths could each save one indexed lookup: a small win that makes a security-critical
// function carry a field it does not need.
//
// M13 widened that return anyway, for a reason M12 did not have — layer 4's standing, which every
// hierarchy check needs and which no second query can supply as cheaply. The owner id came along, so
// Delete's redundant read is gone as a side effect rather than as its own argument.
//
// RemoveMember keeps its GetGuild, and the difference is worth naming rather than looking like an
// oversight. Delete asks whether the *actor* owns the guild, which the actor's own resolution answers.
// RemoveMember asks whether the *target* does — and an Instance Admin short-circuits before any
// resolution happens, so on the path that most needs the answer there is no resolution to read it from.

// Allows reports whether the Decision covers a permission, from either authority.
func (d Decision) Allows(need roles.Permission) bool {
	return d.instanceAdmin || d.resolution.Permissions.Has(need)
}

// Outranks reports whether the actor may act on something standing at the given position.
//
// The full ADR 0008 answer rather than layer 4's half: an Instance Admin is above every guild's hierarchy
// and is never resolved against one, so there is no standing to compare and the tier answers first. Below
// that, roles.Resolution.Outranks handles the owner and the strictly-greater comparison.
//
// One function, for the reason authorize is one function: a rule written as N call sites has N chances to
// miss one, and this package has already shipped three member operations that checked a permission and
// never asked about standing.
func (d Decision) Outranks(position int32) bool {
	return d.instanceAdmin || d.resolution.Outranks(position)
}

// AllowsInChannel is Allows, resolved within one channel — layer 1 first, then layer 5.
//
// A predicate rather than a permission set, because permAll is deliberately unexported: nothing outside
// the roles package should be able to name "everything", and a wrapper that had to would be reaching for
// it. This mirrors Allows exactly, one scope down.
//
// The tier has to be asked first. An Instance Admin is never resolved against a guild, so `d.resolution`
// is the zero value on that path — calling InChannel on it resolves to no permissions at all, which would
// hide every channel in the guild from the one account that must always see them. Both callers guarded
// that by hand, which is the shape this package has three times decided not to rely on: Allows, Outranks
// and OutranksMember each exist so a caller cannot forget the tier, and this is the fourth.
func (d Decision) AllowsInChannel(
	channelID snowflake.ID, overwrites []db.PermissionOverwrite, need roles.Permission,
) bool {
	return d.instanceAdmin || d.resolution.InChannel(channelID, overwrites).Has(need)
}

// OutranksMember is Outranks for a target that is a person rather than a role.
//
// Separate because the guild owner cannot be reached by a positional comparison — their own standing is a
// meaningless zero, so comparing it reports that any role-holder outranks them. An Instance Admin still
// may act on the owner, which is why the tier is checked here and not inside roles.Resolution.
func (d Decision) OutranksMember(targetID snowflake.ID, targetStanding int32) bool {
	return d.instanceAdmin || d.resolution.OutranksMember(targetID, targetStanding)
}

// Authorize decides whether an actor may act, against an explicit querier, so a check can run inside a caller's
// transaction rather than on a separate connection.
//
// # The two questions, and why they are not one function
//
// ADR 0008 puts Instance Admin at layer 1 and says it "sits outside any guild's role hierarchy entirely,
// not resolved via roles.Resolve" — and rejects the synthetic "super role" design by name. So the tier
// check lives here and the guild resolution lives in roles.
//
// Folding layer 1 into Resolve would not merely be untidy, it would be wrong in a way that fails closed
// and then gets "fixed" in the wrong direction: an Instance Admin is not a guild member, holds no roles,
// and resolves to exactly zero permissions. Resolve returning a non-guild authority as though it were a
// guild one is the conflation the ADR exists to prevent.
//
// # What the caller learns
//
// Two outcomes, and the split is an anti-enumeration decision rather than a convenience:
//
//   - [httpx.ErrNotFound] when the actor holds no membership — which covers both "no such guild" and "not
//     in it". Guild ids are snowflakes: sequential, and carrying their own creation time. Answering 404
//     for one and 403 for the other turns any list of plausible ids into a map of which guilds exist on
//     the instance, which is the oracle M11 closed for session ids.
//   - [httpx.ErrForbidden] when the actor is a member but lacks the permission. They already know the
//     guild exists, so a distinct answer discloses nothing, and reporting "not found" to somebody looking
//     at a guild in their own sidebar would be a bug rather than a defense.
//
// Neither carries the permission that was missing. A message naming the bit is a small map of the guild's
// configuration, and every caller of this function is a mutation that has already decided to refuse.
//
// Rule 1 requires resolution "using data freshly loaded for the specific guild/channel in the request
// path". A mutation that resolves permissions on the pool and then writes in a transaction reads a
// snapshot that predates its own BEGIN — narrow, but it is the exact window in which a demotion committed
// between the two would be missed, and closing it costs a parameter.
func Authorize(
	ctx context.Context,
	q db.Querier,
	actor auth.Actor,
	guildID, channelID snowflake.ID,
	need roles.Permission,
) (Decision, error) {
	// Layer 1, before anything else touches the guild. An Instance Admin acts on guilds they are not in —
	// that is the point of the tier — so resolving first and returning ErrNotFound on a non-member would
	// answer 404 for every guild on the instance and never reach this check. There is a test for it.
	//
	// # The round trip this costs, measured rather than assumed
	//
	// This is a second query on every guild request, including the channel and member listings rule 7
	// names as hot paths — and ListGuildMemberAuthority exists precisely to argue against extra round
	// trips. So the cost is worth stating: on PostgreSQL 16.14 against 20,000 users and 5,000 guilds it is
	// 2.85 us and one buffer, against 19.42 us for the authority query it precedes. It is the cheapest
	// query in this package: a primary-key lookup on a table that holds a handful of rows on any instance.
	//
	// Two ways to remove it were considered and both cost more than they save. Folding the flag into
	// ListGuildMemberAuthority as a fourth column puts layer-1 data inside the query that feeds
	// roles.Resolve, which is the coupling ADR 0008 spends its alternatives section rejecting. Checking
	// lazily — resolve first, consult the tier only when the permission check fails — reads as strictly
	// better and is not: an Instance Admin who *is* a member and passes on their own permissions would
	// come back with instanceAdmin false, and refuseEscalation would then refuse them a grant the tier
	// entitles them to. Both are real options for a milestone that has load to point at; neither is worth
	// reshaping a security boundary for 2.85 us today.
	admin, err := q.IsInstanceAdmin(ctx, int64(actor.UserID))
	if err != nil {
		return Decision{}, fmt.Errorf("guildauth: check instance admin: %w", err)
	}
	if admin {
		return Decision{instanceAdmin: true}, nil
	}

	res, err := roles.Resolve(ctx, q, guildID, actor.UserID, channelID)
	if err != nil {
		if errors.Is(err, roles.ErrNotAMember) {
			return Decision{}, httpx.ErrNotFound
		}
		return Decision{}, err
	}

	if !res.Permissions.Has(need) {
		return Decision{}, httpx.ErrForbidden
	}

	return Decision{resolution: res}, nil
}

// Owns reports whether the actor is the guild's owner — ADR 0008 layer 2.
//
// False for an Instance Admin, who is never resolved against a guild at all and holds layer 1 instead.
// A caller wanting "may act as the guild's own authority" wants `d.instanceAdmin || d.owns()`, and both
// halves are load-bearing: the tier acts on guilds it is not in, and the owner is not a tier.
func (d Decision) Owns() bool {
	return d.resolution.IsOwner()
}

// InstanceAdmin reports whether this decision came from layer 1 rather than from a guild resolution.
//
// Exported reluctantly, and only because two callers in `guilds` genuinely ask this rather than asking
// about authority. Delete needs it twice: once for "may act as the guild's own authority", which is
// `InstanceAdmin() || Owns()` and is the form Owns's own comment prescribes, and once for "the resolution
// was skipped, so nothing has yet established that this guild exists".
//
// Prefer Allows, Outranks, AllowsInChannel and OutranksMember. Each exists so a caller cannot forget the
// tier, and this accessor is the one way back to forgetting it — a check written as
// `if d.InstanceAdmin()` and nothing else is a check that ignores layers 2 through 5.
func (d Decision) InstanceAdmin() bool { return d.instanceAdmin }

// AuthorizeChannel resolves a channel to its guild and authorizes a permission within it.
//
// # Viewing is required alongside whatever else is asked for
//
// Every caller gets PermViewChannel added to its `need`, and that is a correction to M13 rather than a
// convenience. M13 taught the channel listing to hide channels and did not teach these routes the same
// thing, so the two disagreed about whether a channel existed: a moderator holding PermManageChannels and
// denied PermViewChannel — an @everyone view-deny removes only the view bit — got the channel omitted
// from their listing and could still rename it, delete it, and write its permission overwrites.
//
// Found by driving a real guild by hand after M13 was tagged, and it needed that: every test that hid a
// channel hid it from somebody holding nothing else, and every test that managed one managed a channel
// that was visible. The divergence needs an actor who holds a management permission and lacks view in the
// same channel, which no unit test constructed and four review passes did not think to ask for.
//
// Requiring it here rather than at each call site is the same argument this package has made four times:
// a rule written as N call sites has N chances to miss one. It also composes with the refusal below —
// a caller who fails for want of the view bit is answered as though the channel were not there, which is
// what the listing already told them.
//
// The guild comes off the channel row and never from the caller, because these routes carry no guild in
// their path — the same reason UpdateChannel loads its own (rule 1). The channel id is passed to
// guildauth.Authorize so the decision is what the caller holds in *this* channel.
//
// # The row is read FOR UPDATE
//
// Its four callers are the `guilds` channel mutations — UpdateChannel, DeleteChannel, SetOverwrite and
// DeleteOverwrite — and each writes an audit entry describing this row. A diff that reads prior state and
// then writes has a window under READ COMMITTED where a concurrent commit lands in between, and the entry
// then records a transition that never happened — reproduced on this branch, and the reason the locking
// query exists. Locking here rather than at each call site keeps the read single.
//
// **That is the test for belonging here, not "is this a mutation".** Every mutation in `messages` takes
// [AuthorizeChannelUnlocked] instead, because none of them describes the channel row — they read two
// fields off it and write elsewhere. Getting that axis wrong is what named the other function
// AuthorizeChannelForRead and made the name false at three of its four call sites inside one milestone.
//
// It serializes concurrent mutations of one channel, which is what anybody would expect of them, and it
// closes a race the ledger records as accepted: the per-channel overwrite ceiling was a read-then-insert
// with no lock, so two concurrent writes could both read 49 and land the channel at 51. They now queue.
// That entry is left in place rather than deleted, because the reasoning it records — a ceiling overshoot
// is not a corrupted ordering — is why nobody had to fix it, and this closing it is a side effect.
func AuthorizeChannel(
	ctx context.Context, q db.Querier, actor auth.Actor, channelID snowflake.ID, need roles.Permission,
) (db.Channel, snowflake.ID, Decision, error) {
	return authorizeChannel(ctx, q, actor, channelID, need, true)
}

// AuthorizeChannelUnlocked is AuthorizeChannel without the row lock.
//
// # The axis is not read-versus-write
//
// This was called AuthorizeChannelUnlocked when M15 extracted the package, on the assumption that the lock
// divides reads from mutations. It does not, and the name was false at three of its four call sites
// within one milestone: `messages` Send, Update and Delete all mutate and all belong here.
//
// The real question is **whether the caller needs the channel row to hold still for its transaction**,
// and only two kinds of caller do. One diffs the row — every `guilds` mutation writes an audit entry
// carrying a from/to over fields of this channel, and M14 reproduced the race where a concurrent commit
// lands between the prior-state read and the write, so the entry records a transition that never
// happened. The other needs the row's own consistency for something it is about to write into that row.
//
// A caller that merely reads a field off the channel and writes somewhere else does not qualify. `Send`
// takes `type` and `guild_id` and inserts into `messages`; holding an exclusive lock for that serialized
// every send in a channel behind every other, measured at 777 sends/s against 2,294 without it. Note that
// the permission read is not why anyone would lock: [Authorize] reads guild_members, roles and
// permission_overwrites unlocked either way, so the lock has never protected rule 1's freshness.
//
// # Which one is the safe default, and why the names are this way round
//
// [AuthorizeChannel] keeps the lock and the shorter name deliberately. The two failure modes are not
// symmetric: taking the lock when you did not need it costs throughput, which is measurable and
// recoverable, while skipping it when you did costs a wrong audit entry, which is silent and permanent.
// So the unconsidered choice is the safe one, and giving it up is a call you have to spell out.
//
// Everything else is identical, including the PermViewChannel fold and the hidden-channel refusal.
func AuthorizeChannelUnlocked(
	ctx context.Context, q db.Querier, actor auth.Actor, channelID snowflake.ID, need roles.Permission,
) (db.Channel, snowflake.ID, Decision, error) {
	return authorizeChannel(ctx, q, actor, channelID, need, false)
}

func authorizeChannel(
	ctx context.Context, q db.Querier, actor auth.Actor, channelID snowflake.ID,
	need roles.Permission, lock bool,
) (db.Channel, snowflake.ID, Decision, error) {
	var row db.Channel
	var err error
	if lock {
		row, err = q.GetChannelForUpdate(ctx, int64(channelID))
	} else {
		row, err = q.GetChannel(ctx, int64(channelID))
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Channel{}, 0, Decision{}, httpx.ErrNotFound
		}
		return db.Channel{}, 0, Decision{}, fmt.Errorf("guildauth: get channel: %w", err)
	}

	guildID, err := guildOf(row)
	if err != nil {
		return db.Channel{}, 0, Decision{}, err
	}

	allowed, err := Authorize(ctx, q, actor, guildID, channelID, need.Add(roles.PermViewChannel))
	if err != nil {
		// A member of the guild who cannot *see* this channel is refused as though it were not there.
		//
		// Authorize answers 403 for a member lacking a permission and 404 for a non-member, and that
		// split was right while every channel in a member's guild was listed to them: they already knew
		// it existed, so naming it disclosed nothing. M13's channel listing hides channels, so the two
		// answers became an oracle — 403 confirms a hidden channel to anybody holding its id, which is
		// exactly what the filter withholds.
		//
		// Only the view permission is consulted here. Somebody who can see the channel and merely lacks
		// PermManageRoles still gets 403, because for them the channel's existence was never a secret.
		if errors.Is(err, httpx.ErrForbidden) {
			_, viewErr := Authorize(ctx, q, actor, guildID, channelID, roles.PermViewChannel)
			switch {
			case viewErr == nil:
				// They can see it, so its existence was never a secret. The original 403 stands.
			case errors.Is(viewErr, httpx.ErrForbidden), errors.Is(viewErr, httpx.ErrNotFound):
				return db.Channel{}, 0, Decision{}, httpx.ErrNotFound
			default:
				// A database failure during the second check is not evidence about the channel. Reporting
				// it as missing would turn a connection blip into a 404 the caller would cache as truth.
				return db.Channel{}, 0, Decision{}, viewErr
			}
		}
		return db.Channel{}, 0, Decision{}, err
	}

	return row, guildID, allowed, nil
}

// guildOf returns the guild a channel belongs to, refusing one that belongs to none.
//
// A DM or group DM has a NULL guild_id and no permission model at all — ADR 0008's hierarchy is
// guild-scoped, and a DM's access rule is membership in channel_recipients, which is M57's table. So the
// guild-channel endpoints refuse them rather than resolving against a guild that is not there.
//
// This paragraph was dropped in the M15 move and restored by the review that caught it. It matters for
// M57 specifically: without it the refusal reads as an unhandled case, and the obvious "fix" is to
// resolve a DM against a guild it does not have.
func guildOf(row db.Channel) (snowflake.ID, error) {
	if row.GuildID == nil {
		// 404 rather than 400: whether a channel id names a DM is not something a caller who cannot see it
		// should learn, and these routes simply do not serve that channel.
		return 0, httpx.ErrNotFound
	}
	return snowflake.ID(*row.GuildID), nil
}
