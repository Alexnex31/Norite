// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package opus is the worker's codec: libopus 1.6.1 compiled to WebAssembly and run under wazero, with no
// host module and a memory cap, one instance per encoder and per decoder (ADR 0036).
//
// What a decoder reads is a stranger's: the SFU never reads a payload, so another participant's bytes reach
// it as they were sent. Inside wazero, a libopus bug reaches that instance's linear memory and nothing
// else, and an instance that traps is replaced, so the next packet decodes from a fresh state.
//
// Under the norite_opusnative build tag the same shim links natively through cgo, for the benchmark that
// compares the two and for tests that hold them equal. No release passes the tag.
package opus

import (
	"context"
	"errors"
	"fmt"
)

const (
	// SampleRate is the only rate the worker runs at, in and out.
	SampleRate = 48000
	// FrameSamples is one 20 ms frame of mono audio: what is captured, encoded and sent at a time.
	FrameSamples = SampleRate / 50
	// MaxPacket is the largest packet a decoder is handed, Opus's largest frame (RFC 6716 §3.2.1).
	// Anything longer is refused before it is copied anywhere.
	MaxPacket = 1275
	// MaxDecodeSamples is 120 ms, the longest a single Opus packet decodes to (RFC 6716 §3.2.5), and so
	// the most a decoder writes for one packet.
	MaxDecodeSamples = SampleRate * 120 / 1000
)

// EncoderOptions configures an encoder. The defaults are architecture.md §6's: 64 kbps, in-band FEC and DTX.
type EncoderOptions struct {
	Bitrate int // bits per second, 6000 to 510000
	// Complexity is 0 to 10. The encoder is most of a frame's cost under WebAssembly, so this is the lever
	// ADR 0036 names before switching to native.
	Complexity int
	FEC        bool
	DTX        bool
	// LossPercent is the loss the encoder plans its FEC for, 0 to 100.
	LossPercent int
}

// DefaultEncoderOptions are what a call starts with.
func DefaultEncoderOptions() EncoderOptions {
	return EncoderOptions{Bitrate: 64000, Complexity: 10, FEC: true, DTX: true, LossPercent: 5}
}

func (o EncoderOptions) check() error {
	switch {
	case o.Bitrate < 6000 || o.Bitrate > 510000:
		return fmt.Errorf("opus: bitrate %d outside 6000 to 510000", o.Bitrate)
	case o.Complexity < 0 || o.Complexity > 10:
		return fmt.Errorf("opus: complexity %d outside 0 to 10", o.Complexity)
	case o.LossPercent < 0 || o.LossPercent > 100:
		return fmt.Errorf("opus: loss %d%% outside 0 to 100", o.LossPercent)
	}
	return nil
}

// An Encoder turns one frame of mono samples into one packet. It is used from one goroutine at a time.
type Encoder interface {
	// Encode encodes exactly FrameSamples samples into out, which needs MaxPacket bytes, and returns the
	// packet's length. With DTX a silent frame may be a packet of one or two bytes.
	Encode(pcm []int16, out []byte) (int, error)
	// SetBitrate changes the bitrate from the next frame on (M31).
	SetBitrate(bitrate int) error
	Close() error
}

// A Decoder turns one sender's packets into samples. It is used from one goroutine at a time, and is
// reset when the slot it serves changes sender.
type Decoder interface {
	// Decode decodes a packet into pcm, which needs MaxDecodeSamples samples, and returns how many it
	// wrote. A packet past MaxPacket is refused. An error that is ErrReplaced means the decoder trapped and
	// was replaced, so nothing was written and the next packet decodes from a fresh state. A decode in
	// progress cannot be interrupted: ctx bounds starting an instance, not running one.
	Decode(ctx context.Context, packet []byte, pcm []int16) (int, error)
	// Conceal writes FrameSamples samples standing in for a lost frame.
	Conceal(ctx context.Context, pcm []int16) (int, error)
	// Recover writes the lost frame before packet from packet's in-band FEC, or conceals it when packet
	// carries none. packet itself is decoded by the next call to Decode.
	Recover(ctx context.Context, packet []byte, pcm []int16) (int, error)
	// Reset forgets everything decoded so far, for a new sender in the same slot.
	Reset(ctx context.Context) error
	Close() error
}

// A Codec makes encoders and decoders. Closing it closes every one it made.
type Codec interface {
	NewEncoder(ctx context.Context, o EncoderOptions) (Encoder, error)
	NewDecoder(ctx context.Context) (Decoder, error)
	Close(ctx context.Context) error
}

var (
	// ErrReplaced is a decoder that trapped and was replaced by a fresh instance.
	ErrReplaced = errors.New("opus: the decoder failed and was started again")
	// ErrTooLong is a packet past MaxPacket.
	ErrTooLong = errors.New("opus: the packet is longer than any Opus packet")
	errShort   = errors.New("opus: the buffer is too short")
)

// codeNames are libopus's error codes, which the shim's calls return as negative numbers.
var codeNames = map[int32]string{-1: "bad argument", -2: "buffer too small", -3: "internal error",
	-4: "invalid packet", -5: "unimplemented", -6: "invalid state", -7: "allocation failed"}

func codeError(op string, code int32) error {
	if name, ok := codeNames[code]; ok {
		return fmt.Errorf("opus: %s: %s", op, name)
	}
	return fmt.Errorf("opus: %s: error %d", op, code)
}
