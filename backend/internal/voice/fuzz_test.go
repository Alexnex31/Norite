// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/voiceproto"
)

// FuzzOnMessage feeds the data-channel handler arbitrary messages, helloed and not: it must refuse or
// accept, never panic. Run beyond its seeds with go test -fuzz=FuzzOnMessage.
func FuzzOnMessage(f *testing.F) {
	for _, seed := range []string{
		`{"op":"hello","version":"dev"}`,
		`{"op":"hello","version":"0.1.0"}`,
		`{"op":"subscribe","pin":["1"],"drop":["2"],"cap":1}`,
		`{"op":"subscribe","pin":[],"drop":[]}`,
		`{"op":"subscribe","pin":["99999999999999999999999"],"drop":[]}`,
		`{"op":"slots","map":[]}`,
		`{}`, `[]`, `null`, `{"op":`,
	} {
		f.Add([]byte(seed), true)
		f.Add([]byte(seed), false)
	}
	s := &SFU{lim: DefaultLimits(), opts: Options{Version: "dev"}, log: zerolog.Nop()}
	f.Fuzz(func(_ *testing.T, data []byte, helloed bool) {
		r := newRoom(s, ids.MustNext())
		c := newConn(s, r, ids.MustNext(), ids.MustNext(), ids.MustNext())
		c.timer.Stop()
		c.helloed = helloed
		_ = c.onMessage(webrtc.DataChannelMessage{IsString: true, Data: data})
	})
}

// FuzzDecode feeds every message type arbitrary bytes.
func FuzzDecode(f *testing.F) {
	f.Add([]byte(`{"op":"ready","version":"dev","slots":[{"slot":0,"kind":"audio","mid":"1"}]}`))
	f.Add([]byte(`{"op":"speaking","users":["1"]}`))
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = voiceproto.OpOf(data)
		for _, v := range []any{
			&voiceproto.Hello{}, &voiceproto.Ready{}, &voiceproto.Slots{}, &voiceproto.Speaking{},
			&voiceproto.Subscribe{}, &voiceproto.Refused{},
		} {
			_ = voiceproto.Decode(data, v)
		}
	})
}

// FuzzForward feeds the forward path arbitrary RTP: the level's re-read, and a slot's rewrite, switch and
// write. It never reads the payload, and must never panic on one.
func FuzzForward(f *testing.F) {
	good, _ := (&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 1, Timestamp: 960}, Payload: []byte{1}}).Marshal()
	withLevel := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 2}, Payload: []byte{1}}
	_ = withLevel.SetExtension(1, []byte{0x95})
	lvl, _ := withLevel.Marshal()
	f.Add(good, false)
	f.Add(lvl, true)
	f.Add([]byte{0x90, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0xbe, 0xde, 0, 1, 0x10, 0xff, 0, 0}, true)
	f.Fuzz(func(_ *testing.T, data []byte, switchFirst bool) {
		var p rtp.Packet
		if p.Unmarshal(data) != nil {
			return
		}
		w := &written{}
		s := testSlot(w)
		a := &source{id: 1, codec: "audio/opus", levelID: 1}
		b := &source{id: 2, codec: "audio/opus", levelID: 1}
		s.assign(a)
		now := time.Now()
		s.write(a, &p, a.readLevel(&p), now)
		if switchFirst {
			s.assign(b)
		}
		a.hear(a.readLevel(&p), p.Timestamp, now)
		s.write(b, &p, b.readLevel(&p), now.Add(time.Duration(p.Timestamp)))
		s.write(a, &p, nil, now)
	})
}
