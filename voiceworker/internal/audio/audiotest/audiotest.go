// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package audiotest is a microphone that plays a known signal and speakers that keep what they are given,
// each on a 20 ms clock of its own, as a device's is. Only tests import it: a test in cmd/norite-voice
// holds the release binary to containing none of it.
package audiotest

import (
	"context"
	"io"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/Alexnex31/Norite/voiceworker/internal/audio"
)

// MaxHeard bounds what the speakers keep: a minute.
const MaxHeard = 60 * audio.SampleRate

// Device is the test's microphone and speakers.
type Device struct {
	// Source fills frame number i of the microphone. Nil is silence.
	Source func(i int, frame []int16)

	mu      sync.Mutex
	heard   []int16
	heardAt []time.Time // when each frame was played
}

// Capture calls frame every 20 ms with the next frame of Source.
func (d *Device) Capture(_ context.Context, frame func([]int16)) (io.Closer, error) {
	return run(func(i int, buf []int16) {
		if d.Source != nil {
			d.Source(i, buf)
		} else {
			clear(buf)
		}
		frame(buf)
	}), nil
}

// Play asks fill for a frame every 20 ms and keeps it.
func (d *Device) Play(_ context.Context, fill func([]int16)) (io.Closer, error) {
	return run(func(_ int, buf []int16) {
		fill(buf)
		d.mu.Lock()
		defer d.mu.Unlock()
		if len(d.heard)+len(buf) <= MaxHeard {
			d.heard = append(d.heard, buf...)
			d.heardAt = append(d.heardAt, time.Now())
		}
	}), nil
}

// Heard is what the speakers played, and when each frame of it was played.
func (d *Device) Heard() ([]int16, []time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]int16(nil), d.heard...), append([]time.Time(nil), d.heardAt...)
}

type ticking struct {
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

func (t *ticking) Close() error {
	t.once.Do(func() { close(t.stop) })
	<-t.done
	return nil
}

func run(each func(i int, buf []int16)) *ticking {
	t := &ticking{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(t.done)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		buf := make([]int16, audio.FrameSamples)
		for i := 0; ; i++ {
			select {
			case <-t.stop:
				return
			case <-tick.C:
				each(i, buf)
			}
		}
	}()
	return t
}

// Envelope is the loudness a Tone follows: between 0.1 and 1, rising and falling three times a second,
// so that what a far end hears can be matched against it in time.
func Envelope(sample int) float64 {
	return 0.55 + 0.45*math.Sin(2*math.Pi*3*float64(sample)/audio.SampleRate)
}

// Tone is a sine of hz at peak amplitude amp, its loudness following Envelope.
func Tone(hz, amp float64) func(int, []int16) {
	return func(i int, f []int16) {
		for k := range f {
			n := i*len(f) + k
			f[k] = int16(amp * Envelope(n) * math.Sin(2*math.Pi*hz*float64(n)/audio.SampleRate))
		}
	}
}

// SpeechLike stands in for a voice, deterministically: voiced syllables (a gliding harmonic series through
// two formant resonators), fricatives (differenced noise) and pauses, about 60% active, so that DTX has
// silences to send. The spike that chose ADR 0036 measured with the same signal, and internal/opus's
// benchmarks still do.
func SpeechLike(seconds int, seed uint64, f0 float64) []int16 {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)) //nolint:gosec // a deterministic test signal
	out := make([]int16, seconds*audio.SampleRate)
	res := func(f, bw float64) (a1, a2 float64) {
		r := math.Exp(-math.Pi * bw / audio.SampleRate)
		return 2 * r * math.Cos(2*math.Pi*f/audio.SampleRate), -r * r
	}
	for i := 0; i < len(out); {
		var n int
		switch kind := rng.IntN(10); {
		case kind < 6:
			n = audio.SampleRate * (150 + rng.IntN(150)) / 1000
			p0, p1 := f0*(0.85+0.3*rng.Float64()), f0*(0.85+0.3*rng.Float64())
			a1, a2 := res(400+400*rng.Float64(), 80)
			b1, b2 := res(1100+1200*rng.Float64(), 120)
			var y1, y2, z1, z2, ph float64
			for k := 0; k < n && i+k < len(out); k++ {
				p := p0 + (p1-p0)*float64(k)/float64(n)
				ph += 2 * math.Pi * p / audio.SampleRate
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
			n = audio.SampleRate * (50 + rng.IntN(60)) / 1000
			var prev float64
			for k := 0; k < n && i+k < len(out); k++ {
				x := rng.NormFloat64()
				out[i+k] = int16(2500 * math.Sin(math.Pi*float64(k)/float64(n)) * (x - prev))
				prev = x
			}
		default:
			n = audio.SampleRate * (200 + rng.IntN(500)) / 1000
			for k := 0; k < n && i+k < len(out); k++ {
				out[i+k] = int16(rng.NormFloat64() * 3)
			}
		}
		i += n
	}
	return out
}
