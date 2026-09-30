// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package gateway

import (
	"context"
	"encoding/json"

	"github.com/Alexnex31/Norite/backend/internal/dispatch"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// AudienceResolver decides which candidates may receive an event. guildauth.Audience is the one this server
// uses; an interface so the decision stays in the authorization chokepoint rather than moving here.
type AudienceResolver interface {
	Allowed(ctx context.Context, ev dispatch.Event, candidates []snowflake.ID) ([]snowflake.ID, error)
}

// onEvent fans one published event out to this process's connections.
//
// Three steps, and each has a reason to be a separate one:
//
//  1. **Candidates** come from memory: connections whose account an event names, or whose READY listed the
//     guild. That is only a pre-filter, so a guild event is never checked against every connection on the
//     process.
//  2. **Allowed** comes from the database, now: membership, and view permission for a channel event (rule
//     1). Memory is never the authority, because a membership removed since READY is exactly what memory
//     would get wrong.
//  3. **Blocks** is rule 20's stage, and it is empty until M70 builds the table. It is a named step rather
//     than nothing, so M70 adds a filter here instead of discovering where one would go.
//
// Runs on the bus's delivery goroutine, one event at a time, which is what keeps a channel's events in the
// order they were published.
func (s *Server) onEvent(payload []byte) {
	var ev dispatch.Event
	if err := json.Unmarshal(payload, &ev); err != nil {
		s.opts.Logger.Error().Err(err).Msg("gateway received an event it could not decode")
		return
	}

	conns := s.candidates(ev)
	if len(conns) == 0 {
		return
	}

	if s.opts.Audience == nil {
		s.opts.Logger.Error().Str("type", ev.Type).Msg("gateway has no audience resolver; dropping an event")
		return
	}
	users := distinctUsers(conns)
	allowed, err := s.opts.Audience.Allowed(context.Background(), ev, users)
	if err != nil {
		// Dropped, not delivered unchecked: an event nobody could authorize is one nobody receives. The
		// connections that miss it resync on their next READY, which is what the bus's at-most-once
		// already asks of them.
		s.opts.Logger.Error().Err(err).Str("type", ev.Type).Msg("gateway could not resolve an event's audience")
		return
	}
	permitted := make(map[snowflake.ID]struct{}, len(allowed))
	for _, id := range allowed {
		permitted[id] = struct{}{}
	}

	for _, c := range withoutBlocked(ev, conns) {
		if _, ok := permitted[c.identity()]; !ok {
			continue
		}
		c.deliver(ev)
	}
}

// candidates is step 1: the connections on this process an event could be for.
func (s *Server) candidates(ev dispatch.Event) []*conn {
	var named map[snowflake.ID]struct{}
	if ev.Audience == dispatch.Users {
		named = make(map[snowflake.ID]struct{}, len(ev.Users))
		for _, id := range ev.Users {
			named[id] = struct{}{}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*conn
	for c := range s.conns {
		id := c.identity()
		if id == 0 {
			continue // not identified: nothing is delivered before READY
		}
		if named != nil {
			if _, ok := named[id]; ok {
				out = append(out, c)
			}
			continue
		}
		if c.inGuild(ev.GuildID) {
			out = append(out, c)
		}
	}
	return out
}

// withoutBlocked is rule 20's stage in guild-channel fan-out, and it filters nothing yet: the blocks table
// is M70's (ADR 0013, docs/architecture.md §14.13). M70's entry names this function as where the
// per-connection block set is applied, so a blocked author's messages never reach the blocker's socket.
// Until then every connection passes, which is correct only because no block can exist.
func withoutBlocked(_ dispatch.Event, conns []*conn) []*conn {
	return conns
}

func distinctUsers(conns []*conn) []snowflake.ID {
	seen := make(map[snowflake.ID]struct{}, len(conns))
	out := make([]snowflake.ID, 0, len(conns))
	for _, c := range conns {
		id := c.identity()
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
