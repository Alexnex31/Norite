// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/Alexnex31/Norite/backend/voiceproto"
)

// slot is one of a receiver's pre-allocated receiving sections. It keeps one SSRC towards its receiver
// whoever fills it, so a join, a leave or a speaker change is a slot-map message and never a renegotiation
// (ADR 0023).
//
// When the sender in a slot changes, sequence numbers and timestamps are rewritten so the stream stays
// continuous, and every packet carries its sender's source id as its CSRC. The data channel and the media
// are separate transports with no order between them, so the slot map says who a source id is and the CSRC
// says which packets are theirs, exact to the packet.
type slot struct {
	conn  *conn
	index int
	kind  string
	codec string
	mid   string
	track *slotTrack

	// mu serializes everything a packet does to the slot: the sender check, the rewrite and the write.
	mu  sync.Mutex
	src *source

	// fresh is true until the current sender's first packet has gone out.
	fresh bool
	// first is the current sender's first sequence number; anything older predates the switch.
	first  uint16
	seqOff uint16
	tsOff  uint32

	// sent is whether anything has gone out, and last* describe the newest packet that has.
	sent    bool
	lastSeq uint16
	lastTS  uint32
	lastAt  time.Time

	hdr  rtp.Header
	csrc [1]uint32
}

// assign puts src in the slot, or empties it for nil. A packet already past the sender check finishes
// first, since it holds mu.
func (s *slot) assign(src *source) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.src == src {
		return
	}
	s.src = src
	s.fresh = true
}

// current is the slot's sender.
func (s *slot) current() *source {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.src
}

// write forwards one packet from src, if src is the slot's sender. It never reads the payload.
func (s *slot) write(src *source, p *rtp.Packet, level []byte, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.src != src {
		return
	}
	b := s.track.binding()
	if b.ws == nil || b.codec != src.codec {
		return
	}

	marker := p.Marker
	if s.fresh {
		s.fresh = false
		s.first = p.SequenceNumber
		if s.sent {
			// Continue from the last packet out: the next sequence number, and the timestamp advanced by
			// the time that has passed, at least one tick, so the receiver's jitter buffer sees one
			// stream with a gap in it rather than a jump back.
			s.seqOff = s.lastSeq + 1 - p.SequenceNumber
			// Capped, since a float past uint32's range converts to whatever the platform makes of it, and
			// a gap of minutes needs no more precision than a gap of seconds.
			gap := min(now.Sub(s.lastAt), maxGap)
			adv := uint32(gap.Seconds() * float64(b.clockRate))
			s.tsOff = s.lastTS + max(adv, 1) - p.Timestamp
		} else {
			s.seqOff, s.tsOff = 0, 0
		}
		// The start of a talkspurt (RFC 3551 §4.1), which is what a receiver resets its playout on.
		marker = true
	} else if int16(p.SequenceNumber-s.first) < 0 {
		// Reordered from before the switch; its rewritten number would collide with one already sent.
		return
	}

	seq := p.SequenceNumber + s.seqOff
	ts := p.Timestamp + s.tsOff
	if !s.sent || int16(seq-s.lastSeq) > 0 {
		s.sent, s.lastSeq, s.lastTS, s.lastAt = true, seq, ts, now
	}

	// A header of the slot's own: its SSRC and payload type, the sender's source id as the CSRC, and no
	// extension of the sender's but the audio level, under this hop's id. Nothing else the sender put in
	// its header reaches the receiver.
	s.csrc[0] = src.id
	s.hdr = rtp.Header{
		Version:          2,
		Marker:           marker,
		PayloadType:      b.payloadType,
		SequenceNumber:   seq,
		Timestamp:        ts,
		SSRC:             b.ssrc,
		CSRC:             s.csrc[:],
		Extensions:       s.hdr.Extensions[:0],
		ExtensionProfile: 0,
	}
	if level != nil && b.levelID != 0 {
		_ = s.hdr.SetExtension(b.levelID, level)
	}
	if _, err := b.ws.WriteRTP(&s.hdr, p.Payload); err != nil {
		s.conn.log.Debug().Err(err).Msg("writing to a slot")
	}
}

// maxGap is the most time a switch's timestamp advance stands for.
const maxGap = 10 * time.Second

// slotTrack is the TrackLocal behind a slot: Pion binds it once the connection is negotiated, and the
// slot writes through the binding.
type slotTrack struct {
	id       string
	kind     webrtc.RTPCodecType
	codecCap webrtc.RTPCodecCapability

	mu sync.Mutex
	b  binding
}

// binding is what negotiation settled for one slot.
type binding struct {
	ws          webrtc.TrackLocalWriter
	ssrc        uint32
	payloadType uint8
	clockRate   uint32
	codec       string
	levelID     uint8
}

func newSlotTrack(kind string, index int, codecCap webrtc.RTPCodecCapability) *slotTrack {
	return &slotTrack{
		id:       kind + "-" + strconv.Itoa(index),
		kind:     mediaKind(kind),
		codecCap: codecCap,
	}
}

func (t *slotTrack) binding() binding {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.b
}

// Bind implements webrtc.TrackLocal.
func (t *slotTrack) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	for _, codec := range ctx.CodecParameters() {
		if !strings.EqualFold(codec.MimeType, t.codecCap.MimeType) {
			continue
		}
		var levelID uint8
		for _, e := range ctx.HeaderExtensions() {
			if e.URI == voiceproto.AudioLevelURI {
				levelID = uint8(e.ID)
			}
		}
		t.mu.Lock()
		t.b = binding{
			ws:          ctx.WriteStream(),
			ssrc:        uint32(ctx.SSRC()),
			payloadType: uint8(codec.PayloadType),
			clockRate:   codec.ClockRate,
			codec:       strings.ToLower(codec.MimeType),
			levelID:     levelID,
		}
		t.mu.Unlock()
		return codec, nil
	}
	return webrtc.RTPCodecParameters{}, webrtc.ErrUnsupportedCodec
}

// Unbind implements webrtc.TrackLocal.
func (t *slotTrack) Unbind(webrtc.TrackLocalContext) error {
	t.mu.Lock()
	t.b = binding{}
	t.mu.Unlock()
	return nil
}

func (t *slotTrack) ID() string                { return t.id }
func (t *slotTrack) RID() string               { return "" }
func (t *slotTrack) StreamID() string          { return "norite" }
func (t *slotTrack) Kind() webrtc.RTPCodecType { return t.kind }
