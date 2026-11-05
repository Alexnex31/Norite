// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package audio is the worker's microphone and speakers: one backend per OS, in pure Go, opened in the
// worker's own process (ADR 0034, ADR 0036).
//
// Everything is 48 kHz, mono, 16-bit, in frames of 20 ms. The device's clock drives both directions:
// capture calls the session with each frame as it is recorded, and playback asks the session for a frame
// whenever the device needs one, so nothing in the worker keeps time of its own and no two clocks drift
// apart.
package audio

import (
	"context"
	"errors"
	"io"
)

const (
	// SampleRate is the only rate the worker runs at.
	SampleRate = 48000
	// FrameSamples is one 20 ms frame.
	FrameSamples = SampleRate / 50
)

// A Device opens the default microphone and speakers.
type Device interface {
	// Capture starts recording and calls frame with each FrameSamples samples, on a goroutine of the
	// device's. frame must return quickly, and must not keep the slice, which is reused.
	Capture(ctx context.Context, frame func([]int16)) (io.Closer, error)
	// Play starts playback and calls fill whenever the device needs FrameSamples samples, on a goroutine
	// of the device's. fill writes every sample of the slice.
	Play(ctx context.Context, fill func([]int16)) (io.Closer, error)
}

// ErrUnsupported is an OS with no backend.
var ErrUnsupported = errors.New("audio: this OS has no audio backend")

// framer cuts what a device delivers, in whatever lengths it chooses, into whole frames.
type framer struct {
	buf   [FrameSamples]int16
	n     int
	frame func([]int16)
}

func (f *framer) write(samples []int16) {
	for len(samples) > 0 {
		c := copy(f.buf[f.n:], samples)
		f.n += c
		samples = samples[c:]
		if f.n == FrameSamples {
			f.frame(f.buf[:])
			f.n = 0
		}
	}
}

// filler answers a device's requests, in whatever lengths it asks, from whole frames.
type filler struct {
	buf  [FrameSamples]int16
	off  int // the next sample of buf to hand out; FrameSamples when buf is spent
	fill func([]int16)
}

func newFiller(fill func([]int16)) *filler { return &filler{off: FrameSamples, fill: fill} }

func (f *filler) read(out []int16) {
	for len(out) > 0 {
		if f.off == FrameSamples {
			f.fill(f.buf[:])
			f.off = 0
		}
		c := copy(out, f.buf[f.off:])
		f.off += c
		out = out[c:]
	}
}
