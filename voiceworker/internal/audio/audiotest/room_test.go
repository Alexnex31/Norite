// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package audiotest

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/Alexnex31/Norite/voiceworker/internal/audio"
)

// The FFT is a means; what the DSP's tests rely on is that it convolves.
func TestConvolveIsConvolution(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // a deterministic test signal
	for _, n := range [][2]int{{1, 1}, {7, 3}, {100, 37}, {1000, 513}} {
		x, h := make([]float64, n[0]), make([]float64, n[1])
		for i := range x {
			x[i] = rng.NormFloat64() * 1000
		}
		for i := range h {
			h[i] = rng.NormFloat64()
		}
		got := convolve(x, h)
		if len(got) != len(x)+len(h)-1 {
			t.Fatalf("%v: %d samples", n, len(got))
		}
		for i := range got {
			var want float64
			for j := range h {
				if k := i - j; k >= 0 && k < len(x) {
					want += x[k] * h[j]
				}
			}
			if math.Abs(got[i]-want) > 1e-6*(1+math.Abs(want)) {
				t.Fatalf("%v: sample %d is %v, not %v", n, i, got[i], want)
			}
		}
	}
}

// A room's response starts at its delay with its direct path, and its tail falls about 60 dB over RT60.
func TestARoomsResponseIsShapedAsAsked(t *testing.T) {
	r := LivingRoom(1)
	h := r.Response()
	d := int(r.Delay * audio.SampleRate)
	for i := range d {
		if h[i] != 0 {
			t.Fatalf("sound at sample %d, before the room's delay at %d", i, d)
		}
	}
	if h[d] != r.Gain {
		t.Fatalf("the direct path is %v, not %v", h[d], r.Gain)
	}
	window := audio.SampleRate / 50
	early, late := 0.0, 0.0
	start := d + audio.SampleRate/200
	for i := start; i < start+window; i++ {
		early += h[i] * h[i]
	}
	for i := len(h) - window; i < len(h); i++ {
		late += h[i] * h[i]
	}
	if fall := 10 * math.Log10(early/late); fall < 50 || fall > 70 {
		t.Fatalf("the tail falls %.1f dB over RT60, not about 60", fall)
	}
}
