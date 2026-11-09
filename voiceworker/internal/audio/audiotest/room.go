// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package audiotest

import (
	"math"
	"math/bits"
	"math/cmplx"
	"math/rand/v2"

	"github.com/Alexnex31/Norite/voiceworker/internal/audio"
)

// A Room is the path from a speaker to a microphone: what the microphone records of what was played. An
// echo canceller's linear filter models exactly this, so a test of one needs a path shaped like a room's
// rather than a delay and a gain, which any filter cancels (M30's plan).
type Room struct {
	// Delay is how long the first sound takes to arrive, devices' buffers included.
	Delay float64 // seconds
	// Gain is the direct path's, in the linear scale of samples.
	Gain float64
	// RT60 is how long the reverberation takes to fall 60 dB.
	RT60 float64 // seconds
	// Drift is how far the microphone's clock runs from the speaker's, in parts per million: two devices
	// are two clocks, and an echo canceller has to follow the echo as it slides.
	Drift float64
	// Seed makes the reflections and the tail.
	Seed uint64
}

// LivingRoom is a room an echo canceller meets: 30 ms to the first sound, the direct path at -10 dB,
// reverberation falling 60 dB in 300 ms, and two clocks 50 ppm apart.
func LivingRoom(seed uint64) Room {
	return Room{Delay: 0.030, Gain: 0.3, RT60: 0.3, Drift: 50, Seed: seed}
}

// Response is the room's impulse response, sampled at 48 kHz: the direct path, six early reflections in
// the next 25 ms, and a tail of noise decaying at RT60, cut where it has fallen 60 dB. The tail carries as
// much energy as the direct path.
func (r Room) Response() []float64 {
	rng := rand.New(rand.NewPCG(r.Seed, 0x5eed)) //nolint:gosec // a deterministic test signal
	const fs = audio.SampleRate
	d := int(r.Delay * fs)
	h := make([]float64, d+int(r.RT60*fs)+1)
	h[d] = r.Gain
	for range 6 {
		at := d + int((0.002+0.023*rng.Float64())*fs)
		h[at] += r.Gain * (0.2 + 0.5*rng.Float64()) * float64(1-2*rng.IntN(2))
	}
	// exp(-6.9 t / RT60) is -60 dB at RT60.
	var tail float64
	start := d + int(0.005*fs)
	tailAt := make([]float64, len(h))
	for i := start; i < len(h); i++ {
		v := rng.NormFloat64() * math.Exp(-6.9*float64(i-start)/(r.RT60*fs))
		tailAt[i] = v
		tail += v * v
	}
	scale := r.Gain / math.Sqrt(tail)
	for i := start; i < len(h); i++ {
		h[i] += scale * tailAt[i]
	}
	return h
}

// Echo is what the microphone records of played: played through the room's response, then read on the
// microphone's clock, which drifts from the speaker's by Drift. It is as long as played, and unclipped.
func (r Room) Echo(played []int16) []float64 {
	x := make([]float64, len(played))
	for i, v := range played {
		x[i] = float64(v)
	}
	e := convolve(x, r.Response())[:len(played)]
	if r.Drift == 0 {
		return e
	}
	out := make([]float64, len(e))
	step := 1 + r.Drift*1e-6
	for i := range out {
		t := float64(i) * step
		j := int(t)
		if j+1 >= len(e) {
			break
		}
		f := t - float64(j)
		out[i] = (1-f)*e[j] + f*e[j+1]
	}
	return out
}

// convolve is x * h, by FFT: a direct convolution of 12 s with a 330 ms response is seven billion
// multiplications, and this is a few million.
func convolve(x, h []float64) []float64 {
	n := 1 << bits.Len(uint(len(x)+len(h)-1))
	a, b := make([]complex128, n), make([]complex128, n)
	for i, v := range x {
		a[i] = complex(v, 0)
	}
	for i, v := range h {
		b[i] = complex(v, 0)
	}
	fft(a, false)
	fft(b, false)
	for i := range a {
		a[i] *= b[i]
	}
	fft(a, true)
	out := make([]float64, len(x)+len(h)-1)
	for i := range out {
		out[i] = real(a[i]) / float64(n)
	}
	return out
}

// fft transforms a in place, its length a power of two; inverse leaves the 1/n to the caller.
func fft(a []complex128, inverse bool) {
	n := len(a)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	sign := -1.0
	if inverse {
		sign = 1
	}
	for size := 2; size <= n; size <<= 1 {
		w := cmplx.Exp(complex(0, sign*2*math.Pi/float64(size)))
		for start := 0; start < n; start += size {
			wk := complex(1, 0)
			for k := range size / 2 {
				u, v := a[start+k], a[start+k+size/2]*wk
				a[start+k], a[start+k+size/2] = u+v, u-v
				wk *= w
			}
		}
	}
}
