// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/rs/zerolog"
)

// written records what a slot writes, in place of the peer connection.
type written struct{ out []rtp.Header }

func (w *written) WriteRTP(h *rtp.Header, _ []byte) (int, error) {
	c := *h
	c.CSRC = append([]uint32(nil), h.CSRC...)
	w.out = append(w.out, c)
	return 0, nil
}

func (w *written) Write([]byte) (int, error) { return 0, nil }

var _ webrtc.TrackLocalWriter = (*written)(nil)

func testSlot(w *written) *slot {
	t := newSlotTrack("audio", 0, opusCapability)
	t.b = binding{ws: w, ssrc: 7, payloadType: 111, clockRate: 48000, codec: "audio/opus", levelID: 1}
	return &slot{conn: &conn{log: zerolog.Nop()}, kind: "audio", codec: "audio/opus", track: t}
}

func pkt(seq uint16, ts uint32) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts}, Payload: []byte{1}}
}

// Reordering can deliver a packet from before a switch after it: rewritten, its number would repeat or
// precede one already sent. It is dropped, and the stream carries on from where it was.
func TestAPacketFromBeforeTheSwitchIsDropped(t *testing.T) {
	w := &written{}
	s := testSlot(w)
	a := &source{id: 1, codec: "audio/opus"}
	b := &source{id: 2, codec: "audio/opus"}
	at := time.Now()

	s.assign(a)
	s.write(a, pkt(100, 1000), nil, at)
	s.write(a, pkt(101, 1960), nil, at.Add(20*time.Millisecond))
	s.assign(b)
	s.write(b, pkt(5000, 90000), nil, at.Add(60*time.Millisecond))
	s.write(b, pkt(4999, 89040), nil, at.Add(61*time.Millisecond)) // b's, from before it had the slot
	s.write(a, pkt(102, 2920), nil, at.Add(62*time.Millisecond))   // a's, after it lost the slot
	s.write(b, pkt(5001, 90960), nil, at.Add(80*time.Millisecond))

	if len(w.out) != 4 {
		t.Fatalf("%d packets written, want 4", len(w.out))
	}
	for i, want := range []struct {
		seq  uint16
		csrc uint32
	}{{100, 1}, {101, 1}, {102, 2}, {103, 2}} {
		h := w.out[i]
		if h.SequenceNumber != want.seq || h.CSRC[0] != want.csrc || h.SSRC != 7 {
			t.Fatalf("packet %d: sequence %d, source %d, SSRC %d", i, h.SequenceNumber, h.CSRC[0], h.SSRC)
		}
	}
	// 40 ms passed between a's last packet and b's first: 1920 ticks.
	if d := w.out[2].Timestamp - w.out[1].Timestamp; d != 1920 {
		t.Fatalf("the timestamp advanced %d ticks across the switch, want 1920", d)
	}
	if d := w.out[3].Timestamp - w.out[2].Timestamp; d != 960 {
		t.Fatalf("the timestamp advanced %d ticks after the switch, want b's own 960", d)
	}
	if !w.out[2].Marker || w.out[3].Marker {
		t.Fatal("the marker is not on the new sender's first packet alone")
	}
}

func TestAnEmptySlotWritesNothing(t *testing.T) {
	w := &written{}
	s := testSlot(w)
	a := &source{id: 1, codec: "audio/opus"}
	s.write(a, pkt(1, 1), nil, time.Now())
	s.assign(a)
	s.assign(nil)
	s.write(a, pkt(2, 2), nil, time.Now())
	if len(w.out) != 0 {
		t.Fatalf("%d packets written by an empty slot", len(w.out))
	}
}

func TestASourceOfAnotherCodecIsNotWritten(t *testing.T) {
	w := &written{}
	s := testSlot(w)
	v := &source{id: 1, codec: "video/vp8"}
	s.assign(v)
	s.write(v, pkt(1, 1), nil, time.Now())
	if len(w.out) != 0 {
		t.Fatal("a slot wrote a codec it was not negotiated for")
	}
}

func TestALongGapAdvancesBoundedly(t *testing.T) {
	w := &written{}
	s := testSlot(w)
	a := &source{id: 1, codec: "audio/opus"}
	b := &source{id: 2, codec: "audio/opus"}
	at := time.Now()
	s.assign(a)
	s.write(a, pkt(1, 0), nil, at)
	s.assign(b)
	s.write(b, pkt(9, 0), nil, at.Add(48*time.Hour))
	if d := w.out[1].Timestamp - w.out[0].Timestamp; d != uint32(maxGap.Seconds()*48000) {
		t.Fatalf("a two-day gap advanced %d ticks", d)
	}
}

// A sender holding a slot for good passes the point where its sequence numbers are half the space from
// its first: eleven minutes at 50 packets a second. Every packet must still go out.
func TestALongTenureIsNotDroppedAtTheWrap(t *testing.T) {
	w := &written{}
	s := testSlot(w)
	a := &source{id: 1, codec: "audio/opus"}
	s.assign(a)
	at := time.Now()
	const n = 70000
	for i := range n {
		s.write(a, pkt(uint16(i), uint32(i)*960), nil, at.Add(time.Duration(i)*20*time.Millisecond))
	}
	if len(w.out) != n {
		t.Fatalf("%d of %d packets written", len(w.out), n)
	}
}

func TestASwitchWhoseClockReadsEarlierStillAdvances(t *testing.T) {
	w := &written{}
	s := testSlot(w)
	a := &source{id: 1, codec: "audio/opus"}
	b := &source{id: 2, codec: "audio/opus"}
	at := time.Now()
	s.assign(a)
	s.write(a, pkt(1, 0), nil, at)
	s.assign(b)
	s.write(b, pkt(9, 0), nil, at.Add(-time.Millisecond))
	if d := int32(w.out[1].Timestamp - w.out[0].Timestamp); d != 960 {
		t.Fatalf("the timestamp moved %d ticks across a switch read a moment early, want one frame", d)
	}
}

// A switch a moment after the last packet starts the new sender's frame where the old sender's ends, not
// inside it: a jitter buffer drops or overlaps a frame that starts before the last one finished.
func TestASwitchStartsAfterTheLastFrameEnds(t *testing.T) {
	for _, step := range []uint32{960, 2880} {
		w := &written{}
		s := testSlot(w)
		a := &source{id: 1, codec: "audio/opus"}
		b := &source{id: 2, codec: "audio/opus"}
		at := time.Now()
		s.assign(a)
		for i := range 5 {
			s.write(a, pkt(uint16(i), uint32(i)*step), nil, at.Add(time.Duration(i)*20*time.Millisecond))
		}
		s.assign(b)
		s.write(b, pkt(700, 12345), nil, at.Add(4*20*time.Millisecond+5*time.Millisecond))
		if d := w.out[5].Timestamp - w.out[4].Timestamp; d != step {
			t.Fatalf("frames of %d ticks, a switch 5 ms on: the new frame starts %d ticks after the last", step, d)
		}
	}
}
