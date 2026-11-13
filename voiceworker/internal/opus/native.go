// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build norite_opusnative && cgo

package opus

// The native twin: shim.c linked against a libopus pkg-config finds, so that the two implementations can
// be measured against each other and held to the same results (ADR 0036). Point PKG_CONFIG_PATH at a
// libopus built with the module's flags for a like-for-like measurement; a distribution's may build in
// features the module leaves out, such as Arch's DNN, which make its decoder slower.

/*
#cgo pkg-config: opus
#include <opus.h>
OpusEncoder *shim_encoder(int, int, int, int, int);
void shim_encoder_free(OpusEncoder *);
int shim_set_bitrate(OpusEncoder *, int);
int shim_set_loss(OpusEncoder *, int);
int shim_encode(OpusEncoder *, const opus_int16 *, int, unsigned char *, int);
OpusDecoder *shim_decoder(void);
void shim_decoder_free(OpusDecoder *);
int shim_decode(OpusDecoder *, const unsigned char *, int, opus_int16 *, int, int);
int shim_reset(OpusDecoder *);
*/
import "C"

import (
	"context"
	"errors"
	"sync"
	"unsafe"
)

// NewNative is the native codec. Nothing in a release reaches it, since no release passes the tag.
func NewNative() Codec { return &nativeCodec{open: map[interface{ Close() error }]struct{}{}} }

type nativeCodec struct {
	mu   sync.Mutex
	open map[interface{ Close() error }]struct{}
}

func (c *nativeCodec) track(x interface{ Close() error }) {
	c.mu.Lock()
	c.open[x] = struct{}{}
	c.mu.Unlock()
}

func (c *nativeCodec) Close(context.Context) error {
	c.mu.Lock()
	open := c.open
	c.open = map[interface{ Close() error }]struct{}{}
	c.mu.Unlock()
	for x := range open {
		_ = x.Close()
	}
	return nil
}

type nativeEncoder struct{ e *C.OpusEncoder }

func (c *nativeCodec) NewEncoder(_ context.Context, o EncoderOptions) (Encoder, error) {
	if err := o.check(); err != nil {
		return nil, err
	}
	e := C.shim_encoder(C.int(o.Bitrate), C.int(o.Complexity), C.int(flag(o.FEC)), C.int(flag(o.DTX)),
		C.int(o.LossPercent))
	if e == nil {
		return nil, errors.New("opus: the encoder could not be created")
	}
	enc := &nativeEncoder{e: e}
	c.track(enc)
	return enc, nil
}

func (e *nativeEncoder) Encode(pcm []int16, out []byte) (int, error) {
	if len(pcm) != FrameSamples || len(out) < MaxPacket || e.e == nil {
		return 0, errShort
	}
	n := C.shim_encode(e.e, (*C.opus_int16)(unsafe.Pointer(&pcm[0])), FrameSamples,
		(*C.uchar)(unsafe.Pointer(&out[0])), MaxPacket)
	if n < 0 {
		return 0, codeError("encoding", int32(n))
	}
	return int(n), nil
}

func (e *nativeEncoder) SetBitrate(bitrate int) error {
	if err := (EncoderOptions{Bitrate: bitrate}).check(); err != nil {
		return err
	}
	if r := C.shim_set_bitrate(e.e, C.int(bitrate)); r != 0 {
		return codeError("setting the bitrate", int32(r))
	}
	return nil
}

func (e *nativeEncoder) SetLoss(percent int) error {
	if err := (EncoderOptions{Bitrate: 6000, LossPercent: percent}).check(); err != nil {
		return err
	}
	if r := C.shim_set_loss(e.e, C.int(percent)); r != 0 {
		return codeError("setting the loss", int32(r))
	}
	return nil
}

func (e *nativeEncoder) Close() error {
	if e.e != nil {
		C.shim_encoder_free(e.e)
		e.e = nil
	}
	return nil
}

type nativeDecoder struct{ d *C.OpusDecoder }

func (c *nativeCodec) NewDecoder(context.Context) (Decoder, error) {
	d := C.shim_decoder()
	if d == nil {
		return nil, errors.New("opus: the decoder could not be created")
	}
	dec := &nativeDecoder{d: d}
	c.track(dec)
	return dec, nil
}

func (d *nativeDecoder) run(packet []byte, pcm []int16, frame int, fec bool) (int, error) {
	if len(packet) > MaxPacket {
		return 0, ErrTooLong
	}
	if d.d == nil {
		return 0, errShort
	}
	var data *C.uchar
	if len(packet) > 0 {
		data = (*C.uchar)(unsafe.Pointer(&packet[0]))
	}
	n := C.shim_decode(d.d, data, C.int(len(packet)), (*C.opus_int16)(unsafe.Pointer(&pcm[0])), C.int(frame),
		C.int(flag(fec)))
	if n < 0 {
		return 0, codeError("decoding", int32(n))
	}
	return int(n), nil
}

func (d *nativeDecoder) Decode(_ context.Context, packet []byte, pcm []int16) (int, error) {
	if len(pcm) < MaxDecodeSamples {
		return 0, errShort
	}
	frame := MaxDecodeSamples
	if len(packet) == 0 {
		frame = FrameSamples
	}
	return d.run(packet, pcm, frame, false)
}

func (d *nativeDecoder) Conceal(_ context.Context, pcm []int16) (int, error) {
	if len(pcm) < FrameSamples {
		return 0, errShort
	}
	return d.run(nil, pcm, FrameSamples, false)
}

func (d *nativeDecoder) Recover(_ context.Context, packet []byte, pcm []int16) (int, error) {
	if len(pcm) < FrameSamples {
		return 0, errShort
	}
	return d.run(packet, pcm, FrameSamples, len(packet) > 0)
}

func (d *nativeDecoder) Reset(context.Context) error {
	if r := C.shim_reset(d.d); r != 0 {
		return codeError("resetting", int32(r))
	}
	return nil
}

func (d *nativeDecoder) Close() error {
	if d.d != nil {
		C.shim_decoder_free(d.d)
		d.d = nil
	}
	return nil
}
