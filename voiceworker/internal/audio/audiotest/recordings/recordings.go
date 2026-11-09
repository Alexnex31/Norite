// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package recordings holds real audio for the DSP's tests (M30): two people reading and a street in the
// rain, 12 s each, which no synthetic signal stands in for. RNNoise is trained to keep speech and remove
// the rest, so a test of what it keeps has to feed it a voice, and a test of what it removes a real room.
//
// Each is stored as Opus packets made by internal/opus at 96 kbps, a few hundred kilobytes for the three
// rather than 3.4 MB of samples, and decoded on first use. testdata/SOURCES.md says where each came from,
// under which terms, and how it was cut; gen.go made the packets. Only tests import this package: the
// release is held to containing none of audiotest (cmd/norite-voice's release_test.go).
package recordings

import (
	"context"
	_ "embed"
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/Alexnex31/Norite/voiceworker/internal/opus"
)

var (
	//go:embed testdata/near.opus
	near []byte
	//go:embed testdata/far.opus
	far []byte
	//go:embed testdata/noise.opus
	noise []byte
)

// Seconds is how long each recording is.
const Seconds = 12

// Near is a man reading an encyclopedia article, close to his microphone, with the pauses of reading
// aloud: the near-end talker.
func Near() []int16 { return decoded("near", near) }

// Far is a woman reading a poem: the far end, whose voice comes back as echo.
func Far() []int16 { return decoded("far", far) }

// Noise is traffic passing on a wet street, with rain on hard surfaces close by: no voice in it.
func Noise() []int16 { return decoded("noise", noise) }

var (
	mu    sync.Mutex
	cache = map[string][]int16{}
)

// decoded returns a copy of the recording's samples, decoding it the first time. A recording that does not
// decode is a broken checkout, and the test calling this cannot go on, so it panics.
func decoded(name string, packets []byte) []int16 {
	mu.Lock()
	defer mu.Unlock()
	if pcm, ok := cache[name]; ok {
		return append([]int16(nil), pcm...)
	}
	pcm, err := decode(packets)
	if err != nil {
		panic(fmt.Sprintf("recordings: %s: %v", name, err))
	}
	cache[name] = pcm
	return append([]int16(nil), pcm...)
}

// decode reads the packets gen.go wrote, each behind a 2-byte big-endian length.
func decode(b []byte) ([]int16, error) {
	ctx := context.Background()
	c, err := opus.New(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close(ctx) }()
	d, err := c.NewDecoder(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]int16, 0, Seconds*opus.SampleRate)
	pcm := make([]int16, opus.MaxDecodeSamples)
	for len(b) > 0 {
		if len(b) < 2 || int(binary.BigEndian.Uint16(b)) > len(b)-2 {
			return nil, fmt.Errorf("a packet runs past the end, %d bytes before it", len(b))
		}
		n := int(binary.BigEndian.Uint16(b))
		got, err := d.Decode(ctx, b[2:2+n], pcm)
		if err != nil {
			return nil, err
		}
		out = append(out, pcm[:got]...)
		b = b[2+n:]
	}
	if len(out) != Seconds*opus.SampleRate {
		return nil, fmt.Errorf("decoded %d samples, not %d", len(out), Seconds*opus.SampleRate)
	}
	return out, nil
}
