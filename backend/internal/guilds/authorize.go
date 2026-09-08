// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"context"
	"errors"
	"fmt"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// authorize decides whether an actor may perform an action, and is the only thing in this package that
// decides it.
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
// channelID may be zero for a guild-level check.
func (s *Service) authorize(
	ctx context.Context,
	actor auth.Actor,
	guildID, channelID snowflake.ID,
	need roles.Permission,
) error {
	_, err := authorizeWith(ctx, s.queries, actor, guildID, channelID, need)
	return err
}

// decision is what authorizeWith resolved on the way to allowing a request.
//
// It exists so a caller that needs the *same* facts again does not fetch them again. CreateRole and
// UpdateRole both have to ask a second question — "may this actor grant these permissions" — and asking it
// independently meant re-running IsInstanceAdmin and a whole roles.Resolve inside a transaction that had
// just run both, five round trips for one INSERT.
//
// The two fields are kept separate rather than collapsed into a permission value, because an Instance
// Admin holds no guild permissions at all (ADR 0008) and representing them as "holds everything" is the
// conflation the layer separation exists to prevent.
type decision struct {
	// instanceAdmin is layer 1: authority from outside the guild entirely.
	instanceAdmin bool
	// permissions is what layers 2..5 resolved. Meaningless when instanceAdmin is true, because an
	// Instance Admin is not a member and was never resolved.
	permissions roles.Permission
}

// allows reports whether the decision covers a permission, from either authority.
func (d decision) allows(need roles.Permission) bool {
	return d.instanceAdmin || d.permissions.Has(need)
}

// authorizeWith is authorize against an explicit querier, so a check can run inside a caller's
// transaction rather than on a separate connection.
//
// Rule 1 requires resolution "using data freshly loaded for the specific guild/channel in the request
// path". A mutation that resolves permissions on the pool and then writes in a transaction reads a
// snapshot that predates its own BEGIN — narrow, but it is the exact window in which a demotion committed
// between the two would be missed, and closing it costs a parameter.
func authorizeWith(
	ctx context.Context,
	q db.Querier,
	actor auth.Actor,
	guildID, channelID snowflake.ID,
	need roles.Permission,
) (decision, error) {
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
		return decision{}, fmt.Errorf("guilds: check instance admin: %w", err)
	}
	if admin {
		return decision{instanceAdmin: true}, nil
	}

	perms, err := roles.Resolve(ctx, q, guildID, actor.UserID, channelID)
	if err != nil {
		if errors.Is(err, roles.ErrNotAMember) {
			return decision{}, httpx.ErrNotFound
		}
		return decision{}, err
	}

	if !perms.Has(need) {
		return decision{}, httpx.ErrForbidden
	}

	return decision{permissions: perms}, nil
}
