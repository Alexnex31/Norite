// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package opus

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/Alexnex31/Norite/voiceworker/internal/audio/audiotest"
)

// BenchmarkFrame is ADR 0036's measurement: one 20 ms frame's codec work in a three-way call, one encode
// and three decodes, reported as p50 and p99 in microseconds alongside Go's mean. Run it against both
// implementations, pinned to one core:
//
//	taskset -c 2 go test -tags norite_opusnative -run '^$' -bench Frame -benchtime 20000x ./internal/opus/
func BenchmarkFrame(b *testing.B) {
	ctx := context.Background()
	impls := map[string]func() (Codec, error){"wasm": func() (Codec, error) { return New(ctx) }}
	if native != nil {
		impls["native"] = func() (Codec, error) { return native(), nil }
	}
	for name, mk := range impls {
		b.Run(name, func(b *testing.B) {
			c, err := mk()
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = c.Close(ctx) }()
			mine := audiotest.SpeechLike(10, 1, 120)
			var streams [3][][]byte
			for s := range streams {
				streams[s] = encodeAll(b, c, audiotest.SpeechLike(10, uint64(10+s), 100+float64(s)*60))
			}
			enc, err := c.NewEncoder(ctx, DefaultEncoderOptions())
			if err != nil {
				b.Fatal(err)
			}
			var decs [3]Decoder
			for s := range decs {
				if decs[s], err = c.NewDecoder(ctx); err != nil {
					b.Fatal(err)
				}
			}
			out := make([]byte, MaxPacket)
			pcm := make([]int16, MaxDecodeSamples)
			frames := len(mine) / FrameSamples
			tEnc := make([]time.Duration, 0, b.N)
			tDec := make([]time.Duration, 0, b.N)
			b.ResetTimer()
			for i := range b.N {
				f := i % frames
				t0 := time.Now()
				if _, err := enc.Encode(mine[f*FrameSamples:(f+1)*FrameSamples], out); err != nil {
					b.Fatal(err)
				}
				t1 := time.Now()
				for s, d := range decs {
					if _, err := d.Decode(ctx, streams[s][f], pcm); err != nil {
						b.Fatal(err)
					}
				}
				t2 := time.Now()
				tEnc, tDec = append(tEnc, t1.Sub(t0)), append(tDec, t2.Sub(t1))
			}
			b.StopTimer()
			all := make([]time.Duration, len(tEnc))
			for i := range all {
				all[i] = tEnc[i] + tDec[i]
			}
			for _, m := range []struct {
				name string
				d    []time.Duration
			}{{"encode", tEnc}, {"decode3", tDec}, {"frame", all}} {
				b.ReportMetric(percentile(m.d, 0.5), m.name+"-p50-µs")
				b.ReportMetric(percentile(m.d, 0.99), m.name+"-p99-µs")
			}
		})
	}
}

// BenchmarkStart is what a call pays before its first frame: compiling the module, then one instance.
func BenchmarkStart(b *testing.B) {
	ctx := context.Background()
	for range b.N {
		c, err := New(ctx)
		if err != nil {
			b.Fatal(err)
		}
		d, err := c.NewDecoder(ctx)
		if err != nil {
			b.Fatal(err)
		}
		_ = d.Close()
		_ = c.Close(ctx)
	}
}

func percentile(d []time.Duration, p float64) float64 {
	s := slices.Clone(d)
	slices.Sort(s)
	return float64(s[int(p*float64(len(s)-1))].Nanoseconds()) / 1000
}

func encodeAll(b *testing.B, c Codec, in []int16) [][]byte {
	b.Helper()
	ctx := context.Background()
	enc, err := c.NewEncoder(ctx, DefaultEncoderOptions())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = enc.Close() }()
	buf := make([]byte, MaxPacket)
	var out [][]byte
	for f := 0; f+FrameSamples <= len(in); f += FrameSamples {
		n, err := enc.Encode(in[f:f+FrameSamples], buf)
		if err != nil {
			b.Fatal(err)
		}
		out = append(out, append([]byte(nil), buf[:n]...))
	}
	return out
}

// BenchmarkReset is a slot changing sender: a new instance. A voice server that changed a slot's sender on
// every frame would make three slots pay this 150 times a second (the security ledger, M29).
func BenchmarkReset(b *testing.B) {
	ctx := context.Background()
	c, err := New(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	d, err := c.NewDecoder(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	b.ReportAllocs()
	for range b.N {
		if err := d.Reset(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
