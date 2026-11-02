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
	lastTS   uint32
	heardTS  bool
	// frame is the shortest timestamp advance between two packets heard, the sender's frame as far as
	// the SFU can tell without reading a payload; zero until two have been heard.
	frame float64
	// credit is the time, in clock ticks, the sender may still claim: it fills with the wall clock up to
	// maxCredit, and each packet spends what it counts for.
	credit   float64
	creditAt time.Time
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
	// speechFloor is the quietest level, in -dBov, that counts at all. At 50 frames a second a steady
	// level L settles near 15.5 (60-L)/60, so speaking starts around -40 dBov and noise at -55 is far off.
	speechFloor = 60.0
	// frameTicks is one 20 ms frame at the audio clock: what the loudness counts in, and what a sender's
	// first packet counts for.
	frameTicks = float64(voiceproto.ClockRateAudio / 50)
	// maxCredit is the most time a sender may claim ahead of the wall clock: Opus's longest packet,
	// 120 ms, so one of those counts whole.
	maxCredit = float64(voiceproto.ClockRateAudio * 120 / 1000)
)

// hear counts one packet's level: 0 is the loudest a sender can claim, 127 silence.
//
// Only a level above speechFloor counts. A microphone's noise floor sits around -50 to -70 dBov, and the
// first version, which summed (127-level)/127 per packet, counted a client sending packets through
// silence as speaking for ever (/code-review).
//
// Each packet counts for the time it covers, so 10, 20 and 60 ms frames at one level are equally loud.
// The time is read off the timestamp's advance, which the sender writes, and held by two bounds it does
// not: no more than the sender's frame, the shortest advance it has sent, so the first packet after a
// silence does not count the silence; and no more than the wall clock has given, through credit, so a
// sender claiming more time than passes counts as no more than one speaking all the time. The first
// version capped every packet at one 20 ms frame instead, which made an honest sender of 60 ms frames a
// third as loud and never speaking (/code-review, second pass).
func (src *source) hear(level []byte, ts uint32, now time.Time) {
	if level == nil {
		return
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.creditAt.IsZero() {
		src.credit = maxCredit
	} else {
		earned := max(float64(now.Sub(src.creditAt)), 0) / float64(time.Second) * voiceproto.ClockRateAudio
		src.credit = min(src.credit+earned, maxCredit)
	}
	src.creditAt = now

	weight := frameTicks
	if src.heardTS {
		weight = 0
		if adv := float64(int32(ts - src.lastTS)); adv > 0 {
			if src.frame == 0 || adv < src.frame {
				src.frame = min(adv, maxCredit)
			}
			weight = min(adv, src.frame)
			src.lastTS = ts
		}
	} else {
		src.lastTS, src.heardTS = ts, true
	}
	weight = min(weight, src.credit)
	src.credit -= weight

	dBov := float64(level[0] & 0x7f)
	loud := max(speechFloor-dBov, 0) / speechFloor
	src.loudness = src.decayedLocked(now) + loud*weight/frameTicks
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
		src.hear(level, p.Timestamp, now)
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
