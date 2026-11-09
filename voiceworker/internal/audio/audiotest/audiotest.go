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
