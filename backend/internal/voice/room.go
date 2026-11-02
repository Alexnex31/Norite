// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"slices"
	"sync"

	"github.com/pion/webrtc/v4"

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
	sources  map[uint32]*source
	// nextSource numbers the room's sources; a source id is a CSRC, unique in the room.
	nextSource uint32
	gone       bool
}

func newRoom(s *SFU, id snowflake.ID) *room {
	return &room{
		sfu:      s,
		id:       id,
		conns:    map[snowflake.ID]*conn{},
		perUser:  map[snowflake.ID]int{},
		excluded: map[snowflake.ID]map[snowflake.ID]bool{},
		sources:  map[uint32]*source{},
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
	for id, src := range r.sources {
		if src.conn == c {
			delete(r.sources, id)
		}
	}
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

// addSource admits a track a connection sends. It is refused once the connection is gone, since a track
// can arrive while a connection is being closed.
func (r *room) addSource(c *conn, track *webrtc.TrackRemote, recv *webrtc.RTPReceiver) *source {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gone || r.conns[c.id] != c {
		return nil
	}
	r.nextSource++
	src := newSource(c, r.nextSource, track, recv)
	r.sources[src.id] = src
	return src
}

func (r *room) removeSource(src *source) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sources[src.id] == src {
		delete(r.sources, src.id)
	}
}

// changed chooses the room's slots again, after anything that can change who fills them.
func (r *room) changed() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.assignLocked()
}

// assignLocked fills every receiver's slots and tells each source which slots it fills.
func (r *room) assignLocked() {
	fills := map[*source][]*slot{}
	if !r.gone {
		sources := make([]*source, 0, len(r.sources))
		for _, src := range r.sources {
			sources = append(sources, src)
		}
		slices.SortFunc(sources, func(a, b *source) int { return int(a.id) - int(b.id) })

		for _, c := range r.conns {
			for kind, slots := range c.slotsByKind() {
				var candidates []*source
				for _, src := range sources {
					if src.kind == kind && r.mayReceive(c, src) {
						candidates = append(candidates, src)
					}
				}
				for _, s := range place(slots, candidates) {
					if src := s.current(); src != nil {
						fills[src] = append(fills[src], s)
					}
				}
			}
		}
	}
	for _, src := range r.sources {
		slots := fills[src]
		src.slots.Store(&slots)
	}
}

// mayReceive says whether c may be sent src at all: never its own user's tracks, and never a user the
// server has excluded for it. Neither is the client's to override.
func (r *room) mayReceive(c *conn, src *source) bool {
	return src.conn.user != c.user && !r.excluded[c.user][src.conn.user]
}

// place puts the first len(slots) of chosen, in order of preference, into slots. A source already in a slot
// stays in it, so a receiver's slots change only as much as the choice does; a source no longer chosen
// leaves its slot, and newcomers fill the slots left free in order.
func place(slots []*slot, chosen []*source) []*slot {
	if len(chosen) > len(slots) {
		chosen = chosen[:len(slots)]
	}
	want := make(map[*source]bool, len(chosen))
	for _, src := range chosen {
		want[src] = true
	}
	placed := make(map[*source]bool, len(chosen))
	var free []*slot
	for _, s := range slots {
		if cur := s.current(); cur != nil && want[cur] && !placed[cur] {
			placed[cur] = true
			continue
		}
		free = append(free, s)
	}
	for _, src := range chosen {
		if placed[src] {
			continue
		}
		free[0].assign(src)
		free = free[1:]
	}
	for _, s := range free {
		s.assign(nil)
	}
	return slots
}
