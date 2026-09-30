// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guildauth

import (
	"context"
	"fmt"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/dispatch"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// Audience decides which of a gateway event's candidate recipients may receive it (M18).
//
// It lives here rather than in the gateway because it is an authority decision, and this package is where
// every one of those is made: a REST route and a gateway event answering "may this account see this
// channel" in two places is the drift the chokepoint exists to prevent. The answer comes from
// roles.ResolveMany, which a test holds equal to roles.Resolve account by account.
type Audience struct {
	q db.Querier
}

// NewAudience returns an Audience reading through q, which is the pool: fan-out runs after commit, outside
// any transaction.
func NewAudience(q db.Querier) *Audience {
	return &Audience{q: q}
}

// Allowed returns the candidates who may receive ev, in no particular order.
//
//   - A Guild event reaches current members only, read now, not from the connection's memory of READY. A
//     member removed since READY whose GUILD_DELETE was lost (the bus is at-most-once) is caught here.
//   - With a channel, it reaches members who can view that channel, and nobody else: the same
//     PermViewChannel every REST read of the channel requires. The owner and administrators bypass the
//     overwrites, as layers 2 and 3 do everywhere.
//   - With Need set, it reaches only members who also hold that, resolved the same way. Chosen by the
//     publisher, which knows what reading the event's object costs over REST.
//   - A Users event reaches exactly its named accounts, which the publisher chose because the event is
//     about the account itself.
//   - A FormerMembers event reaches every candidate, because the guild is gone and there are no rows left
//     to check. The candidates are connections that were already told about the guild in READY.
//
// Layer 1 is absent deliberately: an Instance Admin is not a member, holds no connection's guild set, and
// is not an event's candidate. Their authority is exercised through REST, where rule 14 can record it.
func (a *Audience) Allowed(ctx context.Context, ev dispatch.Event, candidates []snowflake.ID) ([]snowflake.ID, error) {
	switch ev.Audience {
	case dispatch.Users, dispatch.FormerMembers:
		return candidates, nil
	case dispatch.Guild:
	default:
		return nil, fmt.Errorf("guildauth: unknown event audience %q", ev.Audience)
	}

	perms, err := roles.ResolveMany(ctx, a.q, ev.GuildID, ev.ChannelID, candidates, ev.Overwrites)
	if err != nil {
		return nil, fmt.Errorf("guildauth: resolving an event's audience: %w", err)
	}
	out := make([]snowflake.ID, 0, len(perms))
	for _, id := range candidates {
		p, member := perms[id]
		if !member {
			continue
		}
		if ev.ChannelID != 0 && !p.Has(roles.PermViewChannel) {
			continue
		}
		if !p.Has(ev.Need) {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}
