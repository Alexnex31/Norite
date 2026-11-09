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
// for Device, and the microphone records it with the echo added, over a noise floor of a sample or two, as
// a real microphone's: digital silence would let a canceled echo go out as nothing at all, which DTX sends
// as nothing, and hide what a real room leaves (M29's lesson). The speakers keep what they play, as
// Device's do. Each runs on its own 20 ms clock, so the echo's delay includes where one clock's tick falls
// against the other's, as two devices' buffers do, and that delay stays put: each frame played lands 20 ms
// after the one before it in what the microphone records, whichever clock ticked first.
type Echoing struct {
	Device
	Room Room

	once    sync.Once
	n       int          // the transform's length
	out     int          // how many samples of echo one frame makes
	h       []complex128 // the room's response, transformed
	echoMu  sync.Mutex
	pending []float64 // echo not yet recorded; pending[0] is the microphone's sample number read
	read    int64     // samples the microphone has recorded
	write   int64     // where the next frame played starts, in the microphone's samples; -1 before any
	noise   uint32
}

// maxPending bounds the echo waiting for a microphone that is not recording: ten seconds. Echo that would
// land past it is not kept.
const maxPending = 10 * audio.SampleRate

func (e *Echoing) setup() {
	e.once.Do(func() {
		h := e.Room.Response()
		e.write = -1
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
		e.record(near, buf)
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
		e.played(buf, x)
	}), nil
}

// record is the microphone taking a frame: near, the echo due now, and the noise floor.
func (e *Echoing) record(near, buf []int16) {
	e.echoMu.Lock()
	defer e.echoMu.Unlock()
	for k := range buf {
		e.noise = e.noise*1664525 + 1013904223
		v := float64(near[k]) + float64(int32(e.noise>>30)-1) // -1, 0, 1 or 2
		if k < len(e.pending) {
			v += e.pending[k]
		}
		buf[k] = int16(max(math.MinInt16, min(math.MaxInt16, math.Round(v))))
	}
	e.pending = e.pending[min(len(buf), len(e.pending)):]
	e.read += int64(len(buf))
}

// played sends a frame the speakers played through the room, by FFT, overlap-adding its echo into what
// the microphone has yet to record, 20 ms after the frame played before it. x is the transform's buffer.
func (e *Echoing) played(buf []int16, x []complex128) {
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
	defer e.echoMu.Unlock()
	// One frame of output buffer, as a device has: the two clocks slip a tick either way, and a frame
	// recorded just before this one played must not find its echo already due.
	if e.write < 0 {
		e.write = e.read + int64(len(buf))
	}
	at := e.write - e.read // where this frame's echo starts in pending; below 0 it was due already
	e.write += int64(len(buf))
	end := min(at+int64(e.out), maxPending)
	for len(e.pending) < int(end) {
		e.pending = append(e.pending, 0)
	}
	for k := max(at, 0); k < end; k++ {
		e.pending[k] += real(x[k-at]) / float64(e.n)
	}
}
