// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package gateway

import (
	"sync"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// sessionIndex finds the sessions an event could be for without visiting the others.
//
// Fan-out first scanned every session on the process for every event, under the server's lock and each
// session's own: measured at 21 µs an event with 1,000 sessions, 240 µs with 10,000 and 3.2 ms with 50,000,
// for the same hundred candidates each time. Through the index an event costs in proportion to the sessions
// that are its candidates, which is what it costs to deliver to them anyway.
//
// It is a pre-filter and never an authority, exactly as the guild sets it is built from are: the audience
// check decides who receives an event, against the database (onEvent).
//
// Its lock is its own and is taken last. A session updates the index while holding its own lock, when
// GUILD_CREATE or GUILD_DELETE changes its guild set, and the server registers sessions while holding its
// lock, so mu is never held while either of those is taken.
type sessionIndex struct {
	mu      sync.Mutex
	byUser  map[snowflake.ID]map[*session]struct{}
	byGuild map[snowflake.ID]map[*session]struct{}
	// unready holds sessions between registering and READY, which do not know their guilds yet and are
	// therefore candidates for every guild event (session.pending holds what they receive).
	unready map[*session]struct{}
}

func addTo(m map[snowflake.ID]map[*session]struct{}, key snowflake.ID, s *session) map[snowflake.ID]map[*session]struct{} {
	if m == nil {
		m = map[snowflake.ID]map[*session]struct{}{}
	}
	set := m[key]
	if set == nil {
		set = map[*session]struct{}{}
		m[key] = set
	}
	set[s] = struct{}{}
	return m
}

func removeFrom(m map[snowflake.ID]map[*session]struct{}, key snowflake.ID, s *session) {
	set := m[key]
	delete(set, s)
	if len(set) == 0 {
		delete(m, key)
	}
}

// register adds a session that has not yet sent READY.
func (x *sessionIndex) register(s *session) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.byUser = addTo(x.byUser, s.userID, s)
	if x.unready == nil {
		x.unready = map[*session]struct{}{}
	}
	x.unready[s] = struct{}{}
}

// ready moves a session from every guild to the guilds READY listed.
func (x *sessionIndex) ready(s *session, guilds map[snowflake.ID]struct{}) {
	x.mu.Lock()
	defer x.mu.Unlock()
	delete(x.unready, s)
	for g := range guilds {
		x.byGuild = addTo(x.byGuild, g, s)
	}
}

// joined and left follow a session's guild set as GUILD_CREATE and GUILD_DELETE change it.
func (x *sessionIndex) joined(s *session, guildID snowflake.ID) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.byGuild = addTo(x.byGuild, guildID, s)
}

func (x *sessionIndex) left(s *session, guildID snowflake.ID) {
	x.mu.Lock()
	defer x.mu.Unlock()
	removeFrom(x.byGuild, guildID, s)
}

// remove forgets a dropped session; guilds is its guild set.
func (x *sessionIndex) remove(s *session, guilds map[snowflake.ID]struct{}) {
	x.mu.Lock()
	defer x.mu.Unlock()
	removeFrom(x.byUser, s.userID, s)
	delete(x.unready, s)
	for g := range guilds {
		removeFrom(x.byGuild, g, s)
	}
}

// ofUser returns userID's sessions.
func (x *sessionIndex) ofUser(userID snowflake.ID) []*session {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := make([]*session, 0, len(x.byUser[userID]))
	for s := range x.byUser[userID] {
		out = append(out, s)
	}
	return out
}

// ofUsers returns the sessions of every account in users, each once.
func (x *sessionIndex) ofUsers(users []snowflake.ID) []*session {
	x.mu.Lock()
	defer x.mu.Unlock()
	var out []*session
	seen := make(map[snowflake.ID]struct{}, len(users))
	for _, u := range users {
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		for s := range x.byUser[u] {
			out = append(out, s)
		}
	}
	return out
}

// ofGuild returns the sessions whose guild set holds guildID, and every session not yet READY.
func (x *sessionIndex) ofGuild(guildID snowflake.ID) []*session {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := make([]*session, 0, len(x.byGuild[guildID])+len(x.unready))
	for s := range x.byGuild[guildID] {
		out = append(out, s)
	}
	for s := range x.unready {
		out = append(out, s)
	}
	return out
}
