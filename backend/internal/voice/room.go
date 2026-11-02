// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"cmp"
	"fmt"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/voiceproto"
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
	stop       chan struct{}
	// panicked is set by the first panic in the room's selection, which alone is logged.
	panicked atomic.Bool
}

// selectEvery is how often a room chooses its slots again as loudness changes, and so how quickly a new
// speaker takes a slot and the speaking indicator follows.
const selectEvery = 100 * time.Millisecond

// incumbency is how much louder than a slot's current sender somebody must be to take it, so two voices
// near the boundary do not trade the slot back and forth.
const incumbency = 1.25

func newRoom(s *SFU, id snowflake.ID) *room {
	return &room{
		sfu:      s,
		id:       id,
		conns:    map[snowflake.ID]*conn{},
		perUser:  map[snowflake.ID]int{},
		excluded: map[snowflake.ID]map[snowflake.ID]bool{},
		sources:  map[uint32]*source{},
		stop:     make(chan struct{}),
	}
}

// run chooses the room's slots again every selectEvery until the room is gone.
func (r *room) run() {
	defer r.sfu.wg.Done()
	t := time.NewTicker(selectEvery)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			r.changed()
		}
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

// shut marks the room gone and stops its selection. A room is freed when its last connection leaves.
func (r *room) shut() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.gone {
		r.gone = true
		close(r.stop)
	}
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

// changed chooses the room's slots again, after anything that can change who fills them, and sends each
// connection whose slot map changed its new one. The sending happens after the room's lock is released:
// a connection too slow to take it is closed, and closing one takes the SFU's lock, which comes first.
//
// It contains a panic itself, because it runs on whatever goroutine changed the room: the room's tick, a
// close (and so every eviction, and the goroutine a contained panic starts), a track ending, a hello, and
// the backend's own SetExclusion. Contained only in the tick, the panic the tick survived came back on the
// next close, in a goroutine nothing recovers, and ended norite-server.
func (r *room) changed() {
	defer r.contain()
	for _, c := range r.assign() {
		c.flushSlots()
		c.flushSpeaking()
	}
}

// contain recovers a panic in the room's selection. One connection's state is not a reason to end the
// process, and the room goes on as well as it can. The first is logged with its stack and the rest are
// not: a panic on the room's own state recurs on every tick, ten a second, for as long as the room lasts.
func (r *room) contain() {
	v := recover()
	if v == nil || r.panicked.Swap(true) {
		return
	}
	r.sfu.log.Error().Stringer("room", r.id).Str("panic", fmt.Sprint(v)).
		Bytes("stack", debug.Stack()).Msg("a voice room's selection panicked; later panics in it are not logged")
}

// assign runs assignLocked under the room's lock, released by a defer: a panic in it is recovered by
// changed, and a lock it left held would stop the room, then the SFU, whose lock is taken first.
func (r *room) assign() []*conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.assignLocked(time.Now())
}

// assignLocked fills every receiver's slots, tells each source which slots it fills, and returns the
// connections whose slot map or speaking list changed.
func (r *room) assignLocked(now time.Time) []*conn {
	fills := map[*source][]*slot{}
	var moved []*conn
	if !r.gone {
		panicHook("selection", nil)
		// Only a connection that has said hello takes part, as a receiver or as a sender (takesPart).
		inRoom := make(map[*conn]bool, len(r.conns))
		for _, c := range r.conns {
			inRoom[c] = c.takesPart()
		}
		sources := make([]*source, 0, len(r.sources))
		loudness := make(map[*source]float64, len(r.sources))
		speaking := map[snowflake.ID]string{}
		for _, src := range r.sources {
			if !inRoom[src.conn] {
				continue
			}
			sources = append(sources, src)
			l, sp := src.judge(now)
			loudness[src] = l
			if sp {
				speaking[src.conn.user] = src.conn.userText
			}
		}
		slices.SortFunc(sources, func(a, b *source) int { return cmp.Compare(a.id, b.id) })

		// In id order, not the map's: each source's slots are then listed the same way tick after tick,
		// which is what lets a tick that changed nothing publish nothing.
		conns := make([]*conn, 0, len(r.conns))
		for _, c := range r.conns {
			conns = append(conns, c)
		}
		slices.SortFunc(conns, func(a, b *conn) int { return cmp.Compare(a.id, b.id) })
		for _, c := range conns {
			sub := c.subscription()
			changed := false
			entries := []voiceproto.SlotEntry{}
			// The cap is over the connection's slots of every kind together, as the contract says, so
			// what one kind fills is spent before the next.
			left := sub.cap
			for _, kind := range []string{voiceproto.KindAudio, voiceproto.KindVideo} {
				slots := c.slotsByKind()[kind]
				if len(slots) == 0 {
					continue
				}
				var candidates []*source
				for _, src := range sources {
					if inRoom[c] && src.kind == kind && r.mayReceive(c, src) && !sub.drop[src.conn.user] {
						candidates = append(candidates, src)
					}
				}
				chosen := choose(candidates, sub, loudness, slots)
				if left >= 0 {
					chosen = chosen[:min(len(chosen), left)]
				}
				for _, s := range place(slots, chosen) {
					src := s.current()
					if src == nil {
						continue
					}
					if left > 0 {
						left--
					}
					fills[src] = append(fills[src], s)
					entries = append(entries, voiceproto.SlotEntry{
						Slot: s.index, Kind: s.kind, Source: src.id,
						User: src.conn.userText, Conn: src.conn.idText,
					})
				}
			}
			if c.setSlotMap(entries) {
				changed = true
			}
			heard := make([]string, 0, len(speaking))
			for user, text := range speaking {
				if !r.excluded[c.user][user] {
					heard = append(heard, text)
				}
			}
			slices.Sort(heard)
			if c.setSpeaking(heard) {
				changed = true
			}
			if changed {
				moved = append(moved, c)
			}
		}
	}
	for _, src := range r.sources {
		slots := fills[src]
		// Only when it changed: most ticks change nothing, and a new slice per source per tick is garbage.
		if old := src.slots.Load(); old != nil && slices.Equal(*old, slots) {
			continue
		}
		src.slots.Store(&slots)
	}
	return moved
}

// choose orders the candidates for a receiver's slots: the users it pinned first, in the order it pinned
// them, then everybody else from the loudest down. Whoever already holds one of these slots counts as
// louder by incumbency and wins a tie; otherwise equal loudness keeps the order sources arrived in.
func choose(candidates []*source, sub subscription, loudness map[*source]float64, slots []*slot) []*source {
	chosen := make([]*source, 0, len(candidates))
	taken := make(map[*source]bool, len(candidates))
	for _, user := range sub.pin {
		for _, src := range candidates {
			if src.conn.user == user && !taken[src] {
				chosen = append(chosen, src)
				taken[src] = true
			}
		}
	}
	holding := make(map[*source]bool, len(slots))
	for _, s := range slots {
		if src := s.current(); src != nil {
			holding[src] = true
		}
	}
	score := func(src *source) float64 {
		if holding[src] {
			return loudness[src] * incumbency
		}
		return loudness[src]
	}
	rest := make([]*source, 0, len(candidates)-len(chosen))
	for _, src := range candidates {
		if !taken[src] {
			rest = append(rest, src)
		}
	}
	// Stable, over candidates already in arrival order. A tie goes to the holder first: in silence every
	// score is zero, and without that the slot would move to the earliest arrival each time the room
	// went quiet.
	slices.SortStableFunc(rest, func(a, b *source) int {
		if c := cmp.Compare(score(b), score(a)); c != 0 {
			return c
		}
		switch {
		case holding[a] && !holding[b]:
			return -1
		case holding[b] && !holding[a]:
			return 1
		}
		return 0
	})
	return append(chosen, rest...)
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
