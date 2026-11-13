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
	b.target = 2 // a buffer deepened by lateness, which waits for two
	push(b, 7, 10, 1000)
	check(t, play(b, 1), ".")
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
	check(t, play(b, 6), "p10 p11 r13 p13 p14 p15")
	if s := b.Stats(); s.Recovered != 1 || s.Concealed != 0 {
		t.Errorf("stats %+v", s)
	}
}

func TestTwoLostFramesAreConcealedThenRecovered(t *testing.T) {
	b := New()
	for _, i := range []int{0, 1, 4, 5} {
		push(b, 7, uint16(10+i), uint32(1000+i*FrameSamples))
	}
	check(t, play(b, 6), "p10 p11 c r14 p14 p15")
}

// DTX: the sender sends nothing for a stretch, so the timestamp jumps while the sequence does not. That is
// no loss, so nothing is recovered, and the gap is concealed, which the decoder fills with comfort noise.
func TestADTXGapIsConcealedNotRecovered(t *testing.T) {
	b := New()
	push(b, 7, 10, 1000)
	push(b, 7, 11, 1000+FrameSamples)
	push(b, 7, 12, 1000+4*FrameSamples)
	check(t, play(b, 5), "p10 p11 c c p12")
	if s := b.Stats(); s.Concealed != 0 || s.Recovered != 0 {
		t.Errorf("a DTX gap was counted as loss: %+v", s)
	}
}

// Packets that waited in a queue on the way arrive bunched once it drains, and the buffer holds more than
// its target. The next DTX gap is cut short, to the target, so the delay the queue added does not stay for
// the rest of the call; a gap in a buffer at its target is left whole (TestADTXGapIsConcealedNotRecovered).
func TestABacklogDrainsInTheNextDTXGap(t *testing.T) {
	b := New()
	for i := range 10 { // ten frames of speech, all at once
		push(b, 7, uint16(10+i), uint32(1000+i*FrameSamples))
	}
	push(b, 7, 20, uint32(1000+15*FrameSamples)) // five frames of DTX, then speech
	push(b, 7, 21, uint32(1000+16*FrameSamples))
	check(t, play(b, 12), "p10 p11 p12 p13 p14 p15 p16 p17 p18 p19 p20 p21")
	if s := b.Stats(); s.Skipped != 5 || s.Concealed != 0 {
		t.Errorf("stats %+v, want 5 frames skipped and none concealed", s)
	}
}

// The gap is cut to the target and no further, and never past the packet that ends it.
func TestADrainedBufferKeepsItsTarget(t *testing.T) {
	b := New()
	b.target = 2
	for i := range 10 {
		push(b, 7, uint16(10+i), uint32(1000+i*FrameSamples))
	}
	for i := range 3 {
		push(b, 7, uint16(20+i), uint32(1000+(20+i)*FrameSamples))
	}
	// The gap is frames 10 to 19 and the newest is frame 22: the timeline goes to frame 20, p20, and keeps
	// two frames behind it.
	check(t, play(b, 13), "p10 p11 p12 p13 p14 p15 p16 p17 p18 p19 p20 p21 p22")
	if s := b.Stats(); s.Skipped != 10 {
		t.Errorf("stats %+v, want the whole gap skipped", s)
	}
	b = New()
	b.target = 2
	for i := range 10 {
		push(b, 7, uint16(10+i), uint32(1000+i*FrameSamples))
	}
	push(b, 7, 20, uint32(1000+20*FrameSamples))
	push(b, 7, 21, uint32(1000+30*FrameSamples))
	// The newest is frame 30, so holding the target would mean frame 28, past p20: the timeline stops at it.
	check(t, play(b, 11), "p10 p11 p12 p13 p14 p15 p16 p17 p18 p19 p20")
}

// A link that delivers slower than the sender sends, as a saturated uplink does: 36 packets a second of a
// sender's 50. A buffer deepened to ten frames still starts once ten consecutive frames are held, though
// they arrived over 280 ms, longer than the ten frames' window.
func TestASlowLinkStillStarts(t *testing.T) {
	b := New()
	b.target = MaxDelay
	for i := range MaxDelay {
		b.Push(7, uint16(10+i), uint32(1000+i*FrameSamples), []byte("p"),
			t0.Add(time.Duration(i)*27750*time.Microsecond))
	}
	if s := b.Next(); s.Kind != Play {
		t.Errorf("ten frames held, delivered at 36 a second, played %v", s.Kind)
	}
}

func TestASenderThatStopsIsConcealedThenSilent(t *testing.T) {
	b := New()
	b.target = 2 // a buffer deepened by lateness, which waits for two
	push(b, 7, 10, 1000)
	push(b, 7, 11, 1000+FrameSamples)
	check(t, play(b, 10), "p10 p11 c c c c c . . .")
	// It buffers again before playing again.
	push(b, 7, 12, 1000+20*FrameSamples)
	check(t, play(b, 1), ".")
	push(b, 7, 13, 1000+21*FrameSamples)
	check(t, play(b, 2), "p12 p13")
}

// A packet after its turn is dropped, and deepens the buffer by a frame: a frame concealed now, and a
// frame more for the next start to wait.
func TestALatePacketIsDroppedAndDeepensTheBuffer(t *testing.T) {
	b := New()
	push(b, 7, 10, 1000)
	push(b, 7, 12, 1000+2*FrameSamples)
	check(t, play(b, 2), "p10 r12")
	push(b, 7, 11, 1000+FrameSamples)
	check(t, play(b, 2), "c p12")
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
// and nothing of the old sender's plays after. Its first sender asks for no reset: that decoder is new.
func TestANewSenderResetsTheDecoder(t *testing.T) {
	b := New()
	push(b, 7, 10, 1000)
	push(b, 7, 11, 1000+FrameSamples)
	push(b, 7, 12, 1000+2*FrameSamples)
	check(t, play(b, 1), "p10")
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
	check(t, play(b, 6), "p65533 p65534 p65535 r1 p1 p2")
}

func TestOversizedAndFarAheadPacketsAreRefused(t *testing.T) {
	b := New()
	b.Push(7, 1, 1000, make([]byte, MaxPacket+1), t0)
	if s := b.Stats(); s.Oversize != 1 {
		t.Errorf("stats %+v", s)
	}
	push(b, 7, 10, 1000)
	push(b, 7, 11, 1000+FrameSamples)
	check(t, play(b, 1), "p10")
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
	check(t, play(b, 4), "p10 p11 c c")
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
					// A new sender: nothing the previous one sent may play from here on, and a change (not
					// the first) owes the decoder a reset.
					pushed, sender, resetOwed = map[string]bool{}, who, started
					started = true
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

// What Next hands the decoder is the buffer's own copy: the slot's goroutine may push into the same place
// in the ring while the decoder reads it, a new sender clearing the ring or a burst wrapping round to it.
func TestAPayloadIsNotOverwrittenByTheNextPush(t *testing.T) {
	b := New()
	push(b, 7, 10, 1000)
	push(b, 7, 11, 1000+FrameSamples)
	s := b.Next()
	if s.Kind != Play || string(s.Payload) != "p10" {
		t.Fatalf("played %v %q", s.Kind, s.Payload)
	}
	b.Push(7, 10+capacity, 1000+capacity*FrameSamples, []byte("XXX"), t0) // the same place in the ring
	if string(s.Payload) != "p10" {
		t.Errorf("the payload being decoded became %q", s.Payload)
	}
}

// In DTX a sender's comfort-noise update arrives alone every 400 ms or so. When speech resumes, playing
// starts from the speech, not from the old update, or everything after it plays 400 ms late (M29's manual
// pass, between two virtual machines).
func TestAStaleComfortNoisePacketDoesNotDelayTheStart(t *testing.T) {
	b := New()
	b.target = 2                         // a buffer deepened by lateness, which waits for two
	b.Push(7, 1, 1000, []byte("p1"), t0) // comfort noise
	check(t, play(b, 3), ". . .")
	later := t0.Add(400 * time.Millisecond)
	b.Push(7, 2, 1000+20*FrameSamples, []byte("p2"), later) // 400 ms later, speech
	check(t, play(b, 1), ".")
	b.Push(7, 3, 1000+21*FrameSamples, []byte("p3"), later.Add(20*time.Millisecond))
	check(t, play(b, 3), "p2 p3 c")
}

// A burst the network delivered at once is all fresh: nothing of it is skipped to save latency, which would
// cut the start of somebody's sentence.
func TestABurstIsPlayedWhole(t *testing.T) {
	b := New()
	for i := range 5 {
		push(b, 7, uint16(10+i), uint32(1000+i*FrameSamples))
	}
	check(t, play(b, 5), "p10 p11 p12 p13 p14")
}

// A late packet in the middle of speech deepens the buffer there and then: one frame is concealed and the
// rest plays a frame later, so the next packet as late as that one is in time.
func TestALatePacketDeepensTheBufferAtOnce(t *testing.T) {
	b := New()
	for i := range 3 {
		push(b, 7, uint16(10+i), uint32(1000+i*FrameSamples))
	}
	check(t, play(b, 1), "p10")
	push(b, 7, 9, 1000-FrameSamples) // after its turn
	check(t, play(b, 3), "c p11 p12")
	if b.Target() != MinDelay+1 {
		t.Errorf("target %d", b.Target())
	}
}

// At its least deep the buffer waits for nothing: a packet plays at the first frame after it arrives. That
// is 20 ms less mouth to ear than waiting for a second; the buffer deepens once lateness shows it must.
func TestAtItsLeastTheBufferPlaysTheFirstPacketAtOnce(t *testing.T) {
	b := New()
	if b.Target() != 1 {
		t.Fatalf("a new buffer waits for %d frames", b.Target())
	}
	push(b, 7, 10, 1000)
	check(t, play(b, 1), "p10")
}

// At one frame deep a lost packet can only be concealed: the packet carrying its FEC has not arrived when it
// is due. The first loss deepens the buffer to two frames, and the next one is recovered.
func TestAFirstLossDeepensTheBufferForFEC(t *testing.T) {
	b := New()
	push(b, 7, 10, 1000)
	check(t, play(b, 2), "p10 c") // 11 is lost, and nothing after it has arrived
	push(b, 7, 12, 1000+2*FrameSamples)
	check(t, play(b, 2), "c p12") // concealed once more, to hold a frame from now on
	push(b, 7, 13, 1000+3*FrameSamples)
	push(b, 7, 15, 1000+5*FrameSamples) // 14 lost, 15 already here
	check(t, play(b, 3), "p13 r15 p15")
	if b.Target() != fecDelay {
		t.Errorf("target %d after a loss", b.Target())
	}
}
