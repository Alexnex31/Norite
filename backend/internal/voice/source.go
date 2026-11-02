// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"strings"
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
func (src *source) run() {
	buf := make([]byte, 1500)
	var p rtp.Packet
	for {
		n, _, err := src.track.Read(buf)
		if err != nil {
			return
		}
		if p.Unmarshal(buf[:n]) != nil {
			continue
		}
		level := src.readLevel(&p)
		now := time.Now()
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
