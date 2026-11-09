// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package opus

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/tetratelabs/wazero"

	"github.com/Alexnex31/Norite/voiceworker/internal/audio/audiotest"
)

// wazero's compiler must run the module exactly as its interpreter does. libopus is deterministic, so the
// two give the same bytes or one of them is wrong, and the interpreter, which translates nothing to the
// host's instructions, is the reference. M29's macOS pass found the compiler wrong on arm64: an encode at
// complexity 5 or more trapped from the second frame on, which built that file for size works around
// (scripts/build-opus-wasm.sh). A mistranslation need not trap: it can make other bytes silently, and this
// is what would see it. It runs on whatever architecture the test does, so CI runs it on amd64 and arm64.
func TestTheCompilerAgreesWithTheInterpreter(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the codec through wazero's interpreter, about 30 ms a frame")
	}
	ctx := context.Background()
	compiled, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = compiled.Close(ctx) }()
	interpreted, err := newWASM(ctx, wazero.NewRuntimeConfigInterpreter())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = interpreted.Close(ctx) }()

	const second = SampleRate
	rng := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // a deterministic test signal
	noise := make([]int16, second)
	for i := range noise {
		noise[i] = int16(rng.NormFloat64() * 3000)
	}
	quiet := make([]int16, second)
	for i := range quiet {
		quiet[i] = int16(rng.IntN(3) - 1)
	}
	sweep := make([]int16, second)
	for i := range sweep {
		f := 100 + 7000*float64(i)/second
		sweep[i] = int16(9000 * math.Sin(2*math.Pi*f*float64(i)/second))
	}
	signals := map[string][]int16{
		"speech": audiotest.SpeechLike(1, 7, 140), "noise": noise, "quiet": quiet, "sweep": sweep,
		"loud": tone(440, 32000, 0, second),
	}
	for name, signal := range signals {
		for _, o := range []EncoderOptions{
			{Bitrate: 64000, Complexity: 10, FEC: true, DTX: true, LossPercent: 5},
			{Bitrate: 16000, Complexity: 5, FEC: true, DTX: false, LossPercent: 20},
			{Bitrate: 128000, Complexity: 0, FEC: false, DTX: true},
		} {
			t.Run(fmt.Sprintf("%s/%dk-c%d", name, o.Bitrate/1000, o.Complexity), func(t *testing.T) {
				want := encodeWith(t, interpreted, o, signal)
				got := encodeWith(t, compiled, o, signal)
				for i := range want {
					if !bytes.Equal(want[i], got[i]) {
						t.Fatalf("frame %d encodes to %d bytes compiled and %d interpreted, and they differ",
							i, len(got[i]), len(want[i]))
					}
				}
				// Decoding the same stream with every seventh packet lost: played, recovered from FEC,
				// or concealed when the next packet is lost too.
				if a, b := decodeWith(t, interpreted, want), decodeWith(t, compiled, want); !slices.Equal(a, b) {
					t.Fatal("the stream decodes to different samples compiled and interpreted")
				}
			})
		}
	}
	t.Run("not-opus", func(t *testing.T) {
		var junk [][]byte
		for n := range 40 {
			p := make([]byte, 1+rng.IntN(MaxPacket))
			for i := range p {
				p[i] = byte(rng.IntN(256))
			}
			if n%5 == 0 {
				p[0] = byte(n) // every TOC configuration in turn
			}
			junk = append(junk, p)
		}
		if a, b := decodeWith(t, interpreted, junk), decodeWith(t, compiled, junk); !slices.Equal(a, b) {
			t.Fatal("bytes that are not Opus decode differently compiled and interpreted")
		}
	})
}

func encodeWith(t *testing.T, c Codec, o EncoderOptions, signal []int16) [][]byte {
	t.Helper()
	enc, err := c.NewEncoder(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = enc.Close() }()
	buf := make([]byte, MaxPacket)
	var out [][]byte
	for f := 0; f+FrameSamples <= len(signal); f += FrameSamples {
		n, err := enc.Encode(signal[f:f+FrameSamples], buf)
		if err != nil {
			t.Fatalf("frame %d: %v", f/FrameSamples, err)
		}
		out = append(out, slices.Clone(buf[:n]))
	}
	return out
}

// decodeWith decodes packets in order, every seventh one lost, and returns the samples with each call's
// outcome folded in, so an error on one side and not the other is a difference too.
func decodeWith(t *testing.T, c Codec, packets [][]byte) []int16 {
	t.Helper()
	ctx := context.Background()
	dec, err := c.NewDecoder(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dec.Close() }()
	pcm := make([]int16, MaxDecodeSamples)
	var out []int16
	keep := func(n int, err error) {
		if err != nil {
			out = append(out, math.MinInt16, int16(len(err.Error())))
			return
		}
		out = append(out, pcm[:n]...)
	}
	for i, p := range packets {
		switch {
		case i%7 == 3 && i+1 < len(packets) && (i+1)%7 != 3:
			keep(dec.Recover(ctx, packets[i+1], pcm))
		case i%7 == 3:
			keep(dec.Conceal(ctx, pcm))
		default:
			keep(dec.Decode(ctx, p, pcm))
		}
	}
	return out
}
