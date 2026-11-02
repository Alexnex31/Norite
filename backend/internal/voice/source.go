// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/Alexnex31/Norite/backend/voiceproto"
)

// source is one track a connection sends. The SFU forwards tracks, never "the participant's track": a
// connection sends a bounded number of them, each of a kind, and video raises the bound (M105–M107).
type source struct {
	conn  *conn
	id    uint32
	kind  string
	codec string
	track *webrtc.TrackRemote
	// levelID is the audio level's extension id in the sender's own mapping, zero if not negotiated.
	levelID uint8
	// level is the last packet's audio level as the SFU re-encodes it; the sender's bytes are never
	// forwarded as they came.
	level [1]byte

	// slots are the slots this source fills now, swapped whole when the room reassigns them, so the
	// read loop never takes the room's lock.
	slots atomic.Pointer[[]*slot]

	// loudness is how loud the sender has been lately, by the levels it reports, and speaking whether it
	// counts as speaking. Both decay when packets stop.
	mu       sync.Mutex
	loudness float64
	heardAt  time.Time
	speaking bool
}

// Loudness and speaking are judged from the RFC 6464 level each packet reports, counted per packet, so the
// level is believed only while packets flow: Opus DTX sends almost nothing in silence, and a sender that
// stops sending stops counting within a second whatever it last claimed. That does not stop a sender
// claiming to be loud while it sends; the receiver's drop and M74's moderation answer that, and the rate
// bound keeps the claim from being worth more than one sender's share.
const (
	// loudnessDecay is the time constant of the decay. At 50 packets a second the loudness of a steady
	// level L settles near 15.5 L.
	loudnessDecay = 300 * time.Millisecond
	// speakingFrom and speakingUntil are the two thresholds, apart so a voice near one does not flicker.
	speakingFrom  = 5.0
	speakingUntil = 2.5
)

// hear counts one packet's level: 0 is the loudest a sender can claim, 127 silence.
func (src *source) hear(level []byte, now time.Time) {
	if level == nil {
		return
	}
	loud := float64(127-(level[0]&0x7f)) / 127
	src.mu.Lock()
	defer src.mu.Unlock()
	src.loudness = src.decayedLocked(now) + loud
	src.heardAt = now
}

// judge returns the source's loudness now, and whether it is speaking, updating the latter.
func (src *source) judge(now time.Time) (float64, bool) {
	src.mu.Lock()
	defer src.mu.Unlock()
	l := src.decayedLocked(now)
	switch {
	case l >= speakingFrom:
		src.speaking = true
	case l < speakingUntil:
		src.speaking = false
	}
	return l, src.speaking
}

func (src *source) decayedLocked(now time.Time) float64 {
	if src.heardAt.IsZero() {
		return 0
	}
	return src.loudness * math.Exp(-float64(now.Sub(src.heardAt))/float64(loudnessDecay))
}

func newSource(c *conn, id uint32, track *webrtc.TrackRemote, r *webrtc.RTPReceiver) *source {
	kind := voiceproto.KindAudio
	if track.Kind() == webrtc.RTPCodecTypeVideo {
		kind = voiceproto.KindVideo
	}
	src := &source{
		conn:  c,
		id:    id,
		kind:  kind,
		codec: strings.ToLower(track.Codec().MimeType),
		track: track,
	}
	for _, e := range r.GetParameters().HeaderExtensions {
		if e.URI == voiceproto.AudioLevelURI {
			src.levelID = uint8(e.ID)
		}
	}
	return src
}

// run reads the track until it ends, and forwards each packet to the slots that hold it. One goroutine per
// source, and the packet and its buffer are reused: the forward is synchronous, so nothing outlives the
// next read.
//
// Each packet is metered first: past the bound it is dropped unforwarded and its level is not counted, so
// a sender cannot buy a slot by sending more, and one that keeps on is closed.
func (src *source) run() {
	defer src.conn.contain("forwarding")
	lim := src.conn.sfu.lim
	buf := make([]byte, 1500)
	var p rtp.Packet
	var m meter
	for {
		n, _, err := src.track.Read(buf)
		if err != nil {
			return
		}
		now := time.Now()
		ok, out := m.admit(now, n, lim.PacketsPerSecond, lim.BitsPerSecond, lim.StrikesToEvict)
		if out {
			go src.conn.close("sending past its bound")
			return
		}
		if !ok || p.Unmarshal(buf[:n]) != nil {
			continue
		}
		panicHook("forwarding", p.Payload)
		level := src.readLevel(&p)
		src.hear(level, now)
		if slots := src.slots.Load(); slots != nil {
			for _, s := range *slots {
				s.write(src, &p, level, now)
			}
		}
	}
}

// readLevel re-reads the packet's audio level, RFC 6464's one byte, and returns it as the SFU will forward
// it, or nil when the packet carries none or carries something else under that id.
func (src *source) readLevel(p *rtp.Packet) []byte {
	if src.levelID == 0 {
		return nil
	}
	var lvl rtp.AudioLevelExtension
	if lvl.Unmarshal(p.GetExtension(src.levelID)) != nil {
		return nil
	}
	src.level[0] = lvl.Level & 0x7f
	if lvl.Voice {
		src.level[0] |= 0x80
	}
	return src.level[:]
}
