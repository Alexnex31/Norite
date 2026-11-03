// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voicetest

import (
	"bytes"
	_ "embed"
	"errors"
	"io"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"

	"github.com/Alexnex31/Norite/backend/voiceproto"
)

// Tone is three seconds of a 440 Hz tone as real Opus: 20 ms frames, 64 kbit/s, one packet to an Ogg page,
// the shape Pion's oggreader reads a packet at a time. Generated once, so it carries no license question:
//
//	ffmpeg -f lavfi -i "sine=frequency=440:duration=3:sample_rate=48000" -ac 2 -c:a libopus -b:a 64k \
//	  -frame_duration 20 -page_duration 20000 -map_metadata -1 -fflags +bitexact -flags:a +bitexact \
//	  -vn tone.ogg
//
//go:embed tone.ogg
var Tone []byte

// FrameSamples is one 20 ms Opus frame at the RTP clock.
const FrameSamples = voiceproto.ClockRateAudio / 50

// ReadOgg returns the Opus packets of an Ogg file, in order, without its two header pages.
func ReadOgg(r io.Reader) ([][]byte, error) {
	ogg, _, err := oggreader.NewWith(r)
	if err != nil {
		return nil, err
	}
	var packets [][]byte
	for {
		payload, header, err := ogg.ParseNextPage()
		if errors.Is(err, io.EOF) {
			return packets, nil
		}
		if err != nil {
			return nil, err
		}
		if _, isHeader := header.HeaderType(payload); isHeader {
			continue
		}
		packets = append(packets, append([]byte(nil), payload...))
	}
}

// TonePackets is Tone's packets.
func TonePackets() [][]byte {
	p, err := ReadOgg(bytes.NewReader(Tone))
	if err != nil {
		panic(err)
	}
	return p
}

// WriteOgg writes received packets as an Ogg file a person can play.
func WriteOgg(w io.Writer, packets []Packet) error {
	ogg, err := oggwriter.NewWith(w, voiceproto.ClockRateAudio, 2)
	if err != nil {
		return err
	}
	for _, p := range packets {
		if err := ogg.WriteRTP(&rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: p.Seq, Timestamp: p.TS},
			Payload: p.Payload,
		}); err != nil {
			return err
		}
	}
	return ogg.Close()
}
