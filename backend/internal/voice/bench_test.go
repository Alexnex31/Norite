// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"fmt"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/rs/zerolog"
)

// discard is a slot's writer with nothing behind it, so the benchmark measures the SFU's own work per
// packet: the level's re-read, the loudness, and per slot the sender check, the rewrite and the header.
// What it leaves out is SRTP, Pion's per-packet encryption, which is per receiver and costs more than
// everything measured here.
type discard struct{}

func (discard) WriteRTP(*rtp.Header, []byte) (int, error) { return 0, nil }
func (discard) Write([]byte) (int, error)                 { return 0, nil }

// BenchmarkForward forwards one sender's packet to each receiver holding it in a slot: the per-packet
// path of source.run after the read. Allocations per packet must stay at zero; a regression there is one
// garbage object per packet per receiver on the one real-time path the stack has.
func BenchmarkForward(b *testing.B) {
	for _, receivers := range []int{1, 8, 16, 63} {
		b.Run(fmt.Sprintf("receivers=%d", receivers), func(b *testing.B) {
			src := &source{id: 1, codec: "audio/opus", levelID: 1}
			slots := make([]*slot, receivers)
			for i := range slots {
				t := newSlotTrack("audio", 0, opusCapability)
				t.b = binding{ws: discard{}, ssrc: uint32(100 + i), payloadType: 111, clockRate: 48000,
					codec: "audio/opus", levelID: 1}
				slots[i] = &slot{conn: &conn{}, kind: "audio", codec: "audio/opus", track: t}
				slots[i].assign(src)
			}
			src.slots.Store(&slots)

			p := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SSRC: 9}, Payload: make([]byte, 160)}
			_ = p.SetExtension(1, []byte{0x95})
			now := time.Now()
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				p.SequenceNumber = uint16(i)
				p.Timestamp = uint32(i) * 960
				level := src.readLevel(p)
				src.hear(level, now)
				for _, s := range *src.slots.Load() {
					s.write(src, p, level, now)
				}
			}
			b.ReportMetric(float64(b.N*receivers)/b.Elapsed().Seconds(), "writes/s")
		})
	}
}

// benchRoom is a room of n connections, each sending one track at a different level and receiving on
// slotsEach slots, with no peer connection behind any of it.
func benchRoom(n, slotsEach int) *room {
	s := &SFU{lim: DefaultLimits(), log: zerolog.Nop()}
	r := newRoom(s, ids.MustNext())
	for i := range n {
		uid, cid := ids.MustNext(), ids.MustNext()
		c := &conn{sfu: s, room: r, id: cid, user: uid, userText: uid.String(), idText: cid.String(), sub: noSubscription, log: zerolog.Nop()}
		var sl []*slot
		for j := range slotsEach {
			sl = append(sl, &slot{conn: c, index: j, kind: "audio", codec: "audio/opus", track: newSlotTrack("audio", j, opusCapability)})
		}
		c.slots = map[string][]*slot{"audio": sl}
		r.conns[c.id] = c
		r.perUser[c.user]++
		src := &source{conn: c, id: uint32(i + 1), kind: "audio", codec: "audio/opus"}
		src.hear([]byte{byte(i % 127)}, time.Now())
		r.sources[src.id] = src
	}
	return r
}

// BenchmarkAssignTick is one selection tick of a room: run every 100 ms per room, so its cost is per room
// rather than per packet. A full room, 64 connections of 16 slots, measured 0.85 ms and 3,899 allocations
// a tick until the ids' text was made once per connection rather than per receiver per tick: 0.44 ms and
// 1,851.
func BenchmarkAssignTick(b *testing.B) {
	for _, tc := range []struct{ n, slots int }{{2, 8}, {8, 8}, {64, 8}, {64, 16}} {
		b.Run(fmt.Sprintf("conns=%d/slots=%d", tc.n, tc.slots), func(b *testing.B) {
			r := benchRoom(tc.n, tc.slots)
			now := time.Now()
			b.ReportAllocs()
			for b.Loop() {
				r.mu.Lock()
				r.assignLocked(now)
				r.mu.Unlock()
			}
		})
	}
}
