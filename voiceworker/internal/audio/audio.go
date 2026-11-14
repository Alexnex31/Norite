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

// Outcome is why a device carries no audio, as the call's state names it (M32, ADR 0034 §1). The client
// words each for its OS; the backend says which it is, since only the backend can tell.
type Outcome int

const (
	// Failed is anything below does not name.
	Failed Outcome = iota
	// Refused is the OS, or the person through it, refusing the program the device.
	Refused
	// NoDevice is no device to open.
	NoDevice
	// NoSession is nobody logged in at the machine, so nothing may use its devices (Linux).
	NoSession
)

// Failure is an error that says which outcome it is. A backend returns one where it knows, from Capture,
// Play, Authorize or Where; any other error is Failed.
type Failure struct {
	Outcome Outcome
	Err     error
}

func (f *Failure) Error() string { return f.Err.Error() }
func (f *Failure) Unwrap() error { return f.Err }

// OutcomeOf is err's outcome: a Failure's, or Failed.
func OutcomeOf(err error) Outcome {
	var f *Failure
	if errors.As(err, &f) {
		return f.Outcome
	}
	return Failed
}

// An Authorizer is a device whose OS asks the person before a program may capture: macOS, where samples
// arriving prove nothing (ADR 0034). The session asks it as the call starts, beside ICE, so that the
// question is on the screen while the call connects, and opens the microphone once it has said yes.
type Authorizer interface {
	// Authorize returns nil once the microphone may be used, the refusal, or ctx's error. It calls asking,
	// on any goroutine, when the OS has put the question to the person and waits on the answer.
	Authorize(ctx context.Context, asking func()) error
}

// Watched is a stream whose backend can tell, off the audio path, whether it still reaches a device. What
// Capture and Play return may implement it. PulseAudio needs it: with nobody logged in it moves a stream to
// a stand-in for no device and delivers zeros there, with no error, and moves it back once somebody logs in
// (M32's spike), so neither the open nor the samples can tell.
type Watched interface {
	// Where returns nil while the stream reaches a device, or why it does not. The session asks it once a
	// second, on a goroutine of its own, which may be while Close runs or after: it must then return at
	// once, and must not panic.
	Where() error
}

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
