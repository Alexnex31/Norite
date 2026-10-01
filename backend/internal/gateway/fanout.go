// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package gateway

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Alexnex31/Norite/backend/internal/dispatch"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// AudienceResolver decides which candidates may receive an event. guildauth.Audience is the one this server
// uses; an interface so the decision stays in the authorization chokepoint rather than moving here.
type AudienceResolver interface {
	Allowed(ctx context.Context, ev dispatch.Event, candidates []snowflake.ID) ([]snowflake.ID, error)
}

// audienceTimeout bounds one event's audience resolution: three indexed reads, milliseconds when the
// database is well.
const audienceTimeout = 5 * time.Second

// laneDepth is how many decoded events may wait for one lane. A lane that falls this far behind holds the
// bus's delivery goroutine until it catches up, and the bus drops past its own buffer, which is the
// at-most-once every subscriber already accepts (events package).
const laneDepth = 256

func (s *Server) startLanes() {
	s.lanes = make([]chan dispatch.Event, s.opts.FanoutLanes)
	for i := range s.lanes {
		ch := make(chan dispatch.Event, laneDepth)
		s.lanes[i] = ch
		s.lanesDone.Add(1)
		go func() {
			defer s.lanesDone.Done()
			for ev := range ch {
				s.fanOut(ev)
			}
		}()
	}
}

// closeLanes stops the fan-out workers once every queued event has been fanned out.
func (s *Server) closeLanes() {
	s.stopLanes.Do(func() {
		for _, ch := range s.lanes {
			close(ch)
		}
		s.lanesDone.Wait()
	})
}

// onEvent decodes a published event and hands it to its guild's lane.
//
// Lanes because the audience check is three database reads and fan-out ran them for one event at a time,
// for the whole process: measured at about 770 µs for a channel event with a hundred candidates against a
// local database, which caps a process at roughly 1,300 events a second, and fewer when the database is a
// network hop away. A guild's events all take the same lane, so they reach every session in the order they
// were published, which is the ordering the protocol promises; events of two guilds may now pass each other,
// and nothing promised they would not.
//
// Decoding the whole event here is also what makes its Data valid JSON, which dispatchLocked relies on to
// copy it into each frame without encoding it again.
func (s *Server) onEvent(payload []byte) {
	var ev dispatch.Event
	if err := json.Unmarshal(payload, &ev); err != nil {
		s.opts.Logger.Error().Err(err).Msg("gateway received an event it could not decode")
		return
	}
	s.lanes[uint64(ev.GuildID)%uint64(len(s.lanes))] <- ev
}

// fanOut sends one event to this process's connections.
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
// Runs on the event's lane, one event at a time, which is what keeps a guild's events in the order they were
// published.
func (s *Server) fanOut(ev dispatch.Event) {
	sessions := s.candidates(ev)
	if len(sessions) == 0 {
		return
	}

	if s.opts.Audience == nil {
		s.opts.Logger.Error().Str("type", ev.Type).Msg("gateway has no audience resolver; dropping an event")
		return
	}
	users := distinctUsers(sessions)
	// Bounded, because this is the one goroutine every event on the process passes through: a query left to
	// hang on a lock or a wedged pooled connection would stop all fan-out here, and the bus would drop what
	// queued behind it. A timeout drops this event instead, the at-most-once the bus already promises.
	ctx, cancel := context.WithTimeout(context.Background(), audienceTimeout)
	defer cancel()
	allowed, err := s.opts.Audience.Allowed(ctx, ev, users)
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

	for _, sess := range withoutBlocked(ev, sessions) {
		if _, ok := permitted[sess.userID]; !ok {
			continue
		}
		sess.deliver(ev)
	}
}

// candidates is step 1: the sessions on this process an event could be for, from the index rather than a
// scan (index.go). Sessions rather than connections, so one whose client has disconnected keeps receiving into
// its buffer and can be resumed without a gap.
func (s *Server) candidates(ev dispatch.Event) []*session {
	if ev.Audience == dispatch.Users {
		return s.idx.ofUsers(ev.Users)
	}
	return s.idx.ofGuild(ev.GuildID)
}

// withoutBlocked is rule 20's stage in guild-channel fan-out, and it filters nothing yet: the blocks table
// is M70's (ADR 0013, docs/architecture.md §14.13). M70's entry names this function as where the
// per-connection block set is applied, so a blocked author's messages never reach the blocker's socket.
// Until then every connection passes, which is correct only because no block can exist.
func withoutBlocked(_ dispatch.Event, sessions []*session) []*session {
	return sessions
}

func distinctUsers(sessions []*session) []snowflake.ID {
	seen := make(map[snowflake.ID]struct{}, len(sessions))
	out := make([]snowflake.ID, 0, len(sessions))
	for _, sess := range sessions {
		id := sess.userID
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
