// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"context"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/guildauth"
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
	_, err := guildauth.Authorize(ctx, s.queries, actor, guildID, channelID, need)
	return err
}
