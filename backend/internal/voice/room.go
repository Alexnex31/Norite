// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"sync"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// room is one voice channel's connections.
//
// Lock order: the SFU's mu, then a room's mu, never the reverse.
type room struct {
	sfu *SFU
	id  snowflake.ID

	mu      sync.Mutex
	conns   map[snowflake.ID]*conn
	perUser map[snowflake.ID]int
	// excluded is, per receiving user, the users never forwarded to them (Control.SetExclusion).
	excluded map[snowflake.ID]map[snowflake.ID]bool
	gone     bool
}

func newRoom(s *SFU, id snowflake.ID) *room {
	return &room{
		sfu:      s,
		id:       id,
		conns:    map[snowflake.ID]*conn{},
		perUser:  map[snowflake.ID]int{},
		excluded: map[snowflake.ID]map[snowflake.ID]bool{},
	}
}

// admit says whether one more connection for user fits.
func (r *room) admit(user snowflake.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns) < r.sfu.lim.ParticipantsPerRoom && r.perUser[user] < r.sfu.lim.ConnsPerUser
}

func (r *room) add(c *conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conns[c.id] = c
	r.perUser[c.user]++
}

func (r *room) remove(c *conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns[c.id] != c {
		return
	}
	delete(r.conns, c.id)
	if r.perUser[c.user]--; r.perUser[c.user] <= 0 {
		delete(r.perUser, c.user)
		// Kept only while the receiver is here, so the set is bounded by the room's own bound.
		delete(r.excluded, c.user)
	}
}

func (r *room) empty() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns) == 0
}

// members is a snapshot of the room's connections.
func (r *room) members() []*conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*conn, 0, len(r.conns))
	for _, c := range r.conns {
		out = append(out, c)
	}
	return out
}

// shut marks the room gone. A room is freed when its last connection leaves.
func (r *room) shut() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gone = true
}

// exclude replaces receiver's exclusion set. The receiver must hold a connection in the room: the set lives
// as long as they do, which bounds it. M70 sets it after Provision returns and before the answer reaches
// the client, so no packet can be forwarded before it applies.
func (r *room) exclude(receiver snowflake.ID, users []snowflake.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.perUser[receiver] == 0 {
		return false
	}
	if len(users) == 0 {
		delete(r.excluded, receiver)
		return true
	}
	set := make(map[snowflake.ID]bool, len(users))
	for _, u := range users {
		set[u] = true
	}
	r.excluded[receiver] = set
	return true
}

// changed asks the room to choose its slots again. Until selection exists it has nothing to do.
func (r *room) changed() {}
