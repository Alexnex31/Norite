// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package jitter

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var t0 = time.Unix(1_800_000_000, 0)

// pkt is packet i of a sender starting at seq0 and ts0, its payload naming it.
func push(b *Buffer, sender uint32, seq uint16, ts uint32) {
	b.Push(sender, seq, ts, []byte(fmt.Sprintf("p%d", seq)), t0)
}

// play runs n steps, each decoded as 20 ms, and describes them: "p5" for a packet played, "r5" for a frame
// recovered from packet 5's FEC, "c" concealed, "." silence, with "!" before a step that resets.
func play(b *Buffer, n int) string {
	var out []string
	for range n {
		s := b.Next()
		var d string
		switch s.Kind {
		case Play:
			d = string(s.Payload)
		case Recover:
			d = "r" + string(s.Payload[1:])
		case Conceal:
			d = "c"
		case Silence:
			d = "."
		}
		if s.Reset {
			d = "!" + d
		}
		out = append(out, d)
		b.Done(FrameSamples)
	}
	return strings.Join(out, " ")
}

func check(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("played %q\n          want %q", got, want)
	}
}

func TestPacketsPlayInOrderOnceTheTargetIsBuffered(t *testing.T) {
	b := New()
	push(b, 7, 10, 1000)
	check(t, play(b, 1), "!.")
	push(b, 7, 12, 1000+2*FrameSamples) // arrives before 11
	push(b, 7, 11, 1000+FrameSamples)
	check(t, play(b, 4), "p10 p11 p12 c")
}

func TestALostFrameIsRecoveredFromTheNextPacketsFEC(t *testing.T) {
	b := New()
	for i := range 6 {
		if i != 2 {
			push(b, 7, uint16(10+i), uint32(1000+i*FrameSamples))
		}
	}
	check(t, play(b, 6), "!p10 p11 r13 p13 p14 p15")
	if s := b.Stats(); s.Recovered != 1 || s.Concealed != 0 {
		t.Errorf("stats %+v", s)
	}
}

func TestTwoLostFramesAreConcealedThenRecovered(t *testing.T) {
	b := New()
	for _, i := range []int{0, 1, 4, 5} {
		push(b, 7, uint16(10+i), uint32(1000+i*FrameSamples))
	}
	check(t, play(b, 6), "!p10 p11 c r14 p14 p15")
}

// DTX: the sender sends nothing for a stretch, so the timestamp jumps while the sequence does not. That is
// no loss, so nothing is recovered, and the gap is concealed, which the decoder fills with comfort noise.
func TestADTXGapIsConcealedNotRecovered(t *testing.T) {
	b := New()
	push(b, 7, 10, 1000)
	push(b, 7, 11, 1000+FrameSamples)
	push(b, 7, 12, 1000+4*FrameSamples)
	check(t, play(b, 5), "!p10 p11 c c p12")
	if s := b.Stats(); s.Concealed != 0 || s.Recovered != 0 {
		t.Errorf("a DTX gap was counted as loss: %+v", s)
	}
}

func TestASenderThatStopsIsConcealedThenSilent(t *testing.T) {
	b := New()
	push(b, 7, 10, 1000)
	push(b, 7, 11, 1000+FrameSamples)
	check(t, play(b, 10), "!p10 p11 c c c c c . . .")
	// It buffers again before playing again.
	push(b, 7, 12, 1000+20*FrameSamples)
	check(t, play(b, 1), ".")
	push(b, 7, 13, 1000+21*FrameSamples)
	check(t, play(b, 2), "p12 p13")
}

// A packet after its turn is dropped, and makes the next start wait one frame more.
func TestALatePacketIsDroppedAndDeepensTheBuffer(t *testing.T) {
	b := New()
	push(b, 7, 10, 1000)
	push(b, 7, 12, 1000+2*FrameSamples)
	check(t, play(b, 2), "!p10 r12")
	push(b, 7, 11, 1000+FrameSamples)
	check(t, play(b, 1), "p12")
	if s := b.Stats(); s.Late != 1 {
		t.Errorf("stats %+v", s)
	}
	if b.Target() != MinDelay+1 {
		t.Errorf("target %d after a late packet", b.Target())
	}
	// And comes back down after a quiet stretch.
	b.Push(7, 13, 1000+3*FrameSamples, []byte("p13"), t0.Add(shrinkAfter+time.Second))
	if b.Target() != MinDelay {
		t.Errorf("target %d after %v without a late packet", b.Target(), shrinkAfter)
	}
}

// When the slot changes sender the decoder is told to reset, before anything of the new sender's decodes,
// and nothing of the old sender's plays after.
func TestANewSenderResetsTheDecoder(t *testing.T) {
	b := New()
	push(b, 7, 10, 1000)
	push(b, 7, 11, 1000+FrameSamples)
	push(b, 7, 12, 1000+2*FrameSamples)
	check(t, play(b, 1), "!p10")
	// The SFU keeps the slot's sequence going, and the new sender's audio starts after the old one's.
	push(b, 9, 13, 1000+3*FrameSamples)
	push(b, 9, 14, 1000+4*FrameSamples)
	check(t, play(b, 3), "!p13 p14 c")
}

func TestCountersWrap(t *testing.T) {
	b := New()
	for i := range 6 {
		if i != 3 {
			push(b, 7, uint16(65533+i), uint32(0xffffffff-2*FrameSamples+uint32(i*FrameSamples)))
		}
	}
	check(t, play(b, 6), "!p65533 p65534 p65535 r1 p1 p2")
}

func TestOversizedAndFarAheadPacketsAreRefused(t *testing.T) {
	b := New()
	b.Push(7, 1, 1000, make([]byte, MaxPacket+1), t0)
	if s := b.Stats(); s.Oversize != 1 {
		t.Errorf("stats %+v", s)
	}
	push(b, 7, 10, 1000)
	push(b, 7, 11, 1000+FrameSamples)
	check(t, play(b, 1), "!p10")
	// A packet claiming to be minutes ahead starts the buffer over from it rather than holding a place.
	push(b, 7, 12, 1000+10_000*FrameSamples)
	if s := b.Stats(); s.Resyncs != 1 {
		t.Errorf("stats %+v", s)
	}
	push(b, 7, 13, 1000+10_001*FrameSamples)
	check(t, play(b, 2), "p12 p13")
}

// A packet whose timestamp falls between frames is passed by the timeline, and is then removed rather than
// holding its place in the ring and counting towards the next start.
func TestAPacketBetweenFramesDoesNotLinger(t *testing.T) {
	b := New()
	push(b, 7, 10, 1000)
	push(b, 7, 11, 1000+FrameSamples)
	push(b, 7, 13, 1000+2*FrameSamples+500)
	check(t, play(b, 4), "!p10 p11 c c")
	if n := b.buffered(); n != 0 {
		t.Errorf("%d packets still held", n)
	}
}

// FuzzSlot sends arbitrary RTP through the buffer: senders, sequence numbers, timestamps and lengths a
// stranger chooses, interleaved with playing. Nothing may panic, a step's payload is always one that was
// pushed, Reset comes before anything of a new sender's, and the ring never holds more than its capacity.
func FuzzSlot(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, ops []byte) {
		b := New()
		var pushed map[string]bool
		sender, started := uint32(0), false
		resetOwed := false
		for len(ops) >= 8 {
			op := ops[0]
			if op%3 == 0 {
				s := b.Next()
				if s.Reset {
					resetOwed = false
				}
				if (s.Kind == Play || s.Kind == Recover) && (resetOwed || !pushed[string(s.Payload)]) {
					t.Fatalf("played %q, pushed %v, reset owed %v", s.Payload, pushed, resetOwed)
				}
				b.Done(int(ops[1]) * 40)
				ops = ops[2:]
				continue
			}
			who := uint32(ops[1] % 3)
			seq := uint16(ops[2])<<8 | uint16(ops[3])
			ts := uint32(ops[4])<<24 | uint32(ops[5])<<16 | uint32(ops[6])*FrameSamples
			n := int(ops[7]) * 6
			payload := []byte(fmt.Sprintf("%d:%d:%d:%d", who, seq, ts, n))
			if n > MaxPacket {
				payload = make([]byte, n) // refused before the buffer looks at its sender
			} else {
				if !started || who != sender {
					// A new sender: nothing the previous one sent may play from here on.
					pushed, sender, started, resetOwed = map[string]bool{}, who, true, true
				}
				pushed[string(payload)] = true
			}
			b.Push(who, seq, ts, payload, t0.Add(time.Duration(ops[0])*time.Millisecond))
			if b.buffered() > capacity {
				t.Fatal("the ring outgrew itself")
			}
			ops = ops[8:]
		}
	})
}
