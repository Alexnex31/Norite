// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package audiotest

import (
	"context"
	"io"
	"math"
	"math/bits"
	"sync"
	"time"

	"github.com/Alexnex31/Norite/voiceworker/internal/audio"
)

// Echoing is a test device whose microphone hears its own speakers through a room, as a laptop's does:
// what the far end says comes back to it unless the DSP cancels it. Source is what the near end says, as
// for Device, and the microphone records it with the echo added. The speakers keep what they play, as
// Device's do. Each runs on its own 20 ms clock, so the echo's delay includes where one clock's tick falls
// against the other's, as two devices' buffers do.
type Echoing struct {
	Device
	Room Room

	once    sync.Once
	n       int          // the transform's length
	out     int          // how many samples of echo one frame makes
	h       []complex128 // the room's response, transformed
	echoMu  sync.Mutex
	pending []float64 // echo not yet recorded; pending[0] is the next sample the microphone takes
}

// maxPending bounds the echo waiting for a microphone that is not recording: ten seconds.
const maxPending = 10 * audio.SampleRate

func (e *Echoing) setup() {
	e.once.Do(func() {
		h := e.Room.Response()
		e.out = audio.FrameSamples + len(h) - 1
		e.n = 1 << bits.Len(uint(e.out))
		e.h = make([]complex128, e.n)
		for i, v := range h {
			e.h[i] = complex(v, 0)
		}
		fft(e.h, false)
	})
}

// Capture records Source with the echo of what has been played added, clipped as a microphone clips.
func (e *Echoing) Capture(_ context.Context, frame func([]int16)) (io.Closer, error) {
	e.setup()
	near := make([]int16, audio.FrameSamples)
	return run(func(i int, buf []int16) {
		clear(near)
		if e.Source != nil {
			e.Source(i, near)
		}
		e.echoMu.Lock()
		for k := range buf {
			v := float64(near[k])
			if k < len(e.pending) {
				v += e.pending[k]
			}
			buf[k] = int16(max(math.MinInt16, min(math.MaxInt16, math.Round(v))))
		}
		e.pending = e.pending[min(len(buf), len(e.pending)):]
		e.echoMu.Unlock()
		frame(buf)
	}), nil
}

// Play asks fill for a frame every 20 ms, keeps it, and sends it into the room.
func (e *Echoing) Play(_ context.Context, fill func([]int16)) (io.Closer, error) {
	e.setup()
	x := make([]complex128, e.n)
	return run(func(_ int, buf []int16) {
		fill(buf)
		e.mu.Lock()
		if len(e.heard)+len(buf) <= MaxHeard {
			e.heard = append(e.heard, buf...)
			e.heardAt = append(e.heardAt, time.Now())
		}
		e.mu.Unlock()
		// The frame through the room, by FFT: overlap-add into what the microphone has yet to record.
		clear(x)
		for k, v := range buf {
			x[k] = complex(float64(v), 0)
		}
		fft(x, false)
		for k := range x {
			x[k] *= e.h[k]
		}
		fft(x, true)
		e.echoMu.Lock()
		for len(e.pending) < e.out && len(e.pending) < maxPending {
			e.pending = append(e.pending, 0)
		}
		for k := range min(e.out, len(e.pending)) {
			e.pending[k] += real(x[k]) / float64(e.n)
		}
		e.echoMu.Unlock()
	}), nil
}
