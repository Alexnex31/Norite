// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package jitter holds one slot's packets between the network and the decoder, and decides, each time the
// speakers need audio, what that audio is: a packet to decode, a lost frame to recover from the next
// packet's FEC or to conceal, or silence.
//
// What it holds is a stranger's: the SFU forwards whatever another participant sends, so sequence numbers,
// timestamps and lengths are read as claims. Every comparison is of a signed difference, so nothing stops
// guarding where a counter wraps (CLAUDE.md, M26), and the buffer is a fixed ring that never grows.
package jitter

import (
	"sync"
	"time"
)

const (
	// FrameSamples is 20 ms at 48 kHz, the frame every Norite sender sends and the step of the timeline.
	FrameSamples = 960
	// MaxPacket is the longest payload kept; Opus allows no longer.
	MaxPacket = 1275
	// capacity is how many packets the ring holds: 1.28 s of 20 ms frames.
	capacity = 64
	// maxAhead is how far ahead of what is playing a packet may claim to be before the buffer starts over
	// from it: a sender whose clock leapt, or one lying.
	maxAhead = 50 * FrameSamples
	// MinDelay and MaxDelay bound the target, in frames, of what is buffered before playing starts.
	MinDelay = 2
	MaxDelay = 10
	// concealFrames is how long a sender that stopped is concealed before the slot falls silent and waits
	// to buffer again: 100 ms, past which concealment is a drone and not a voice.
	concealFrames = 5
	// shrinkAfter is how long without a late packet before the target delay comes down a frame.
	shrinkAfter = 10 * time.Second
)

// Kind is what to play for the next stretch of audio.
type Kind int

const (
	// Silence is nothing to decode: nobody is in the slot, or they stopped long enough ago.
	Silence Kind = iota
	// Play decodes Payload.
	Play
	// Recover decodes the frame lost before Payload from Payload's FEC. Payload is played next.
	Recover
	// Conceal stands in for a frame with nothing to recover it from, or for a DTX gap, which the decoder
	// fills with comfort noise.
	Conceal
)

// Step is what Next decided. Payload is the buffer's own copy, valid until the next call to Next: Push
// may overwrite the packet's place in the ring the moment Next returns, from another goroutine.
type Step struct {
	Kind    Kind
	Payload []byte
	// Reset is set on the first step after the slot changed sender: the decoder holds the previous one's
	// state, and must forget it before decoding anything. A slot's first sender asks for none.
	Reset bool
}

type entry struct {
	used    bool
	seq     uint16
	ts      uint32
	n       int
	payload [MaxPacket]byte
}

// Stats counts what the buffer refused or covered for, for the call's log.
type Stats struct {
	Late, Oversize, Resyncs, Concealed, Recovered int
	// Senders is how many senders the slot has carried.
	Senders int
}

// Buffer is one slot's. Push is called from the goroutine reading the slot, Next and Done from the one
// playing, so it locks.
type Buffer struct {
	mu   sync.Mutex
	ring [capacity]entry

	sender     uint32
	haveSender bool
	reset      bool

	started bool
	next    uint32 // the timestamp of the next sample to play, once started
	lastSeq uint16 // the last packet played or covered for
	missing int    // frames in a row with nothing buffered

	target   int
	lastLate time.Time
	// out is what Next hands the decoder, copied out of the ring under the lock. Only Next writes it.
	out   [MaxPacket]byte
	stats Stats
	// pending is the duration Next handed out and Done has not yet confirmed.
	pending bool
}

// New is an empty buffer.
func New() *Buffer { return &Buffer{target: MinDelay} }

// Push adds a packet from sender, the CSRC the SFU wrote, at time now.
func (b *Buffer) Push(sender uint32, seq uint16, ts uint32, payload []byte, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(payload) > MaxPacket {
		b.stats.Oversize++
		return
	}
	if !b.haveSender || sender != b.sender {
		// The first sender's decoder is a fresh one already; only a change asks for another.
		b.reset = b.haveSender
		b.clear()
		b.sender, b.haveSender = sender, true
		b.stats.Senders++
	}
	if b.started {
		d := int32(ts - b.next) //nolint:gosec // a signed difference is the point: it survives the wrap
		if d < 0 {
			b.stats.Late++
			b.late(now)
			return
		}
		if d > maxAhead {
			b.stats.Resyncs++
			b.clear()
		}
	} else if ref, ok := b.any(); ok {
		if d := int32(ts - ref.ts); d > maxAhead || d < -maxAhead { //nolint:gosec // signed difference
			b.stats.Resyncs++
			b.clear()
		}
	}
	e := &b.ring[seq%capacity]
	if e.used && int16(seq-e.seq) <= 0 { //nolint:gosec // signed difference
		return // a duplicate, or older than what holds the place
	}
	e.used, e.seq, e.ts, e.n = true, seq, ts, copy(e.payload[:], payload)
	if b.lastLate.IsZero() {
		b.lastLate = now
	}
	if b.target > MinDelay && now.Sub(b.lastLate) > shrinkAfter {
		b.target--
		b.lastLate = now
	}
}

// late grows the target: a packet that came after its turn is one a deeper buffer would have played.
func (b *Buffer) late(now time.Time) {
	b.target = min(b.target+1, MaxDelay)
	b.lastLate = now
}

func (b *Buffer) clear() {
	for i := range b.ring {
		b.ring[i].used = false
	}
	b.started, b.missing = false, 0
}

func (b *Buffer) any() (*entry, bool) {
	for i := range b.ring {
		if b.ring[i].used {
			return &b.ring[i], true
		}
	}
	return nil, false
}

// earliest is the buffered packet with the lowest timestamp after from, or at it when at is set.
func (b *Buffer) earliest(from uint32, at bool) *entry {
	var best *entry
	for i := range b.ring {
		e := &b.ring[i]
		if !e.used {
			continue
		}
		d := int32(e.ts - from) //nolint:gosec // signed difference
		if d < 0 || (d == 0 && !at) {
			continue
		}
		if best == nil || int32(e.ts-best.ts) < 0 { //nolint:gosec // signed difference
			best = e
		}
	}
	return best
}

func (b *Buffer) buffered() int {
	n := 0
	for i := range b.ring {
		if b.ring[i].used {
			n++
		}
	}
	return n
}

// Next decides what plays next. The caller decodes it and reports how many samples it played with Done,
// before calling Next again.
func (b *Buffer) Next() Step {
	b.mu.Lock()
	defer b.mu.Unlock()
	reset := b.reset
	b.reset = false
	b.pending = true
	if !b.started {
		if b.buffered() < b.target {
			b.pending = false
			return Step{Kind: Silence, Reset: reset}
		}
		first, _ := b.any()
		first = b.earliest(first.ts-maxAhead, true)
		b.started, b.next, b.lastSeq, b.missing = true, first.ts, first.seq-1, 0
	}
	// A packet the timeline has passed will never play: one whose timestamp fell between frames, or
	// that a resync left behind. It would otherwise hold its place, and count towards the next start.
	for i := range b.ring {
		if e := &b.ring[i]; e.used && int32(e.ts-b.next) < 0 { //nolint:gosec // signed difference
			e.used = false
		}
	}
	if e := b.earliest(b.next, true); e != nil && e.ts == b.next {
		e.used = false
		b.lastSeq, b.missing = e.seq, 0
		return Step{Kind: Play, Payload: b.copyOut(e), Reset: reset}
	} else if e != nil {
		b.missing = 0
		if e.seq == b.lastSeq+1 {
			// Nothing was lost: the sender sent nothing for this stretch, which is DTX.
			return Step{Kind: Conceal, Reset: reset}
		}
		b.stats.Concealed++
		if e.ts == b.next+FrameSamples {
			b.stats.Concealed--
			b.stats.Recovered++
			b.lastSeq = e.seq - 1
			return Step{Kind: Recover, Payload: b.copyOut(e), Reset: reset}
		}
		return Step{Kind: Conceal, Reset: reset}
	}
	b.missing++
	if b.missing > concealFrames {
		b.started, b.missing, b.pending = false, 0, false
		return Step{Kind: Silence, Reset: reset}
	}
	b.stats.Concealed++
	return Step{Kind: Conceal, Reset: reset}
}

func (b *Buffer) copyOut(e *entry) []byte {
	return b.out[:copy(b.out[:], e.payload[:e.n])]
}

// Done moves the timeline on by what the step played. A step of Silence needs none.
func (b *Buffer) Done(samples int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.pending || !b.started {
		b.pending = false
		return
	}
	b.pending = false
	if samples > 0 {
		b.next += uint32(samples) //nolint:gosec // at most 120 ms
	}
}

// Stats is what the buffer has refused or covered for so far.
func (b *Buffer) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stats
}

// Senders is how many senders the slot has carried: it changes the moment a new one's packet arrives.
func (b *Buffer) Senders() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stats.Senders
}

// Target is the delay, in frames, the buffer waits for before it starts playing.
func (b *Buffer) Target() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.target
}
