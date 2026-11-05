// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package opus

import (
	"context"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
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
			mine := speechLike(10, 1, 120)
			var streams [3][][]byte
			for s := range streams {
				streams[s] = encodeAll(b, c, speechLike(10, uint64(10+s), 100+float64(s)*60))
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

// speechLike stands in for a voice, deterministically: voiced syllables (a gliding harmonic series through
// two formant resonators), fricatives (differenced noise) and pauses, about 60% active, so that DTX has
// silences to send. The spike that chose ADR 0036 measured with the same signal.
func speechLike(seconds int, seed uint64, f0 float64) []int16 {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)) //nolint:gosec // a deterministic test signal
	out := make([]int16, seconds*SampleRate)
	res := func(f, bw float64) (a1, a2 float64) {
		r := math.Exp(-math.Pi * bw / SampleRate)
		return 2 * r * math.Cos(2*math.Pi*f/SampleRate), -r * r
	}
	for i := 0; i < len(out); {
		var n int
		switch kind := rng.IntN(10); {
		case kind < 6:
			n = SampleRate * (150 + rng.IntN(150)) / 1000
			p0, p1 := f0*(0.85+0.3*rng.Float64()), f0*(0.85+0.3*rng.Float64())
			a1, a2 := res(400+400*rng.Float64(), 80)
			b1, b2 := res(1100+1200*rng.Float64(), 120)
			var y1, y2, z1, z2, ph float64
			for k := 0; k < n && i+k < len(out); k++ {
				p := p0 + (p1-p0)*float64(k)/float64(n)
				ph += 2 * math.Pi * p / SampleRate
				src := 0.0
				for h := 1.0; h*p < 4000; h++ {
					src += math.Sin(h*ph) / h
				}
				y := src + a1*y1 + a2*y2
				y2, y1 = y1, y
				z := src + b1*z1 + b2*z2
				z2, z1 = z1, z
				env := math.Sin(math.Pi * float64(k) / float64(n))
				out[i+k] = int16(math.Max(-32767, math.Min(32767, 900*env*(0.02*y+0.015*z))))
			}
		case kind < 8:
			n = SampleRate * (50 + rng.IntN(60)) / 1000
			var prev float64
			for k := 0; k < n && i+k < len(out); k++ {
				x := rng.NormFloat64()
				out[i+k] = int16(2500 * math.Sin(math.Pi*float64(k)/float64(n)) * (x - prev))
				prev = x
			}
		default:
			n = SampleRate * (200 + rng.IntN(500)) / 1000
			for k := 0; k < n && i+k < len(out); k++ {
				out[i+k] = int16(rng.NormFloat64() * 3)
			}
		}
		i += n
	}
	return out
}
