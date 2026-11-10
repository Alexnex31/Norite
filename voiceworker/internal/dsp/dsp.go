// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package dsp is the worker's audio processing: WebRTC's AEC3 echo canceller, RNNoise's noise suppressor
// and WebRTC's AGC2 gain control, in that order, compiled together to WebAssembly and run under wazero
// with no host module and a memory cap, one instance per call (M30, ADR 0036).
//
// The order is the roadmap's and is fixed here, in Go: echo cancellation runs on the linear signal the
// microphone recorded, before RNNoise's non-linear processing changes it, and the gain is set last, on
// what is sent. The module exports each component on its own (csrc/shim.cc), so nothing in it could run
// them in another order.
//
// What the echo canceller reads as its reference is what the speakers played, which is other people's
// decoded audio and so shaped by a stranger. Inside wazero, a bug that input reaches is confined to the
// call's own instance. A chain whose call fails is not used again: the session goes on without it.
package dsp

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	// SampleRate is the only rate the chain runs at.
	SampleRate = 48000
	// ChunkSamples is 10 ms, what every call into the chain takes: the APM's and RNNoise's frame alike. A
	// 20 ms frame of the worker's is two of them.
	ChunkSamples = SampleRate / 100
	// DelaySamples is how far behind its input the whole chain's output is. 20 ms of it are RNNoise's: a
	// chunk of overlap-add, and a chunk of lookahead, since 0.2 applies the gains it computes from each
	// chunk to the one before (denoise.c's delayed_X). About 9 ms are the AEC object's, measured with it
	// alone; AGC2 adds none. Measured as a whole and held by a test, and the session's latency budget
	// adds it (M29's TestMouthToEarStaysWithinItsBudget).
	DelaySamples = 1385
	// Delay is DelaySamples as time.
	Delay = DelaySamples * time.Second / SampleRate
)

// Settings say which of the three components process the microphone. A component off is skipped, except
// RNNoise, which runs whatever its setting for the voice probability it returns: with noise suppression
// off, its output is discarded.
type Settings struct {
	EchoCancellation bool
	NoiseSuppression bool
	AutoGain         bool
}

// AllOn is what a call gets unless somebody turned a component off.
func AllOn() Settings {
	return Settings{EchoCancellation: true, NoiseSuppression: true, AutoGain: true}
}

// A Chain is one call's processing. It is used from one goroutine, which feeds it both directions in the
// order they happened: each chunk the speakers played, and each chunk the microphone recorded, as they
// came. Once a call has failed, every later call fails with ErrBroken, and the chain is closed and dropped.
type Chain interface {
	// Render takes ChunkSamples of what the speakers played: the echo canceller's reference.
	Render(ref []int16) error
	// Capture processes ChunkSamples of the microphone in place, and returns RNNoise's probability that
	// they hold a voice, from 0 to 1.
	Capture(pcm []int16) (voice float32, err error)
	Close() error
}

// A Module makes chains. Closing it closes every chain it made.
type Module interface {
	NewChain(ctx context.Context, s Settings) (Chain, error)
	Close(ctx context.Context) error
}

var (
	// ErrBroken is a chain a call has already failed on.
	ErrBroken = errors.New("dsp: the audio processing failed earlier in this call")
	errShort  = errors.New("dsp: a call takes exactly ChunkSamples samples")
)

// apmError is an error code from the APM, which returns negative numbers (audio_processing.h).
func apmError(op string, code int32) error {
	return fmt.Errorf("dsp: %s: the audio processing module returned error %d", op, code)
}
