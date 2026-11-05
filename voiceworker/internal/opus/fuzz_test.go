// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package opus

import (
	"context"
	"errors"
	"testing"
)

// FuzzDecode hands a decoder what a stranger can send: any bytes at all, mostly not Opus. It must not
// panic, must write no more than its bound, and must still decode a real packet afterwards, so that one
// hostile packet costs that packet and nothing after it.
func FuzzDecode(f *testing.F) {
	ctx := context.Background()
	c, err := New(ctx)
	if err != nil {
		f.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	valid, err := lastPacket(ctx, c, tone(440, 8000, 0, FrameSamples*4))
	if err != nil {
		f.Fatal(err)
	}
	for _, seed := range [][]byte{nil, {0}, {0xff}, {0xff, 0xff, 0xff, 0xff}, valid, valid[:1],
		// Code 3, 48 frames: the most frames a packet may claim, and more than its bytes hold.
		{0xfb, 0xb0}, {0xfb, 0xff, 0xff, 0xff}, make([]byte, MaxPacket), []byte("RTP\x80\x6f\x00\x01not opus")} {
		f.Add(seed)
	}
	dec, err := c.NewDecoder(ctx)
	if err != nil {
		f.Fatal(err)
	}
	defer func() { _ = dec.Close() }()
	pcm := make([]int16, MaxDecodeSamples+1)
	f.Fuzz(func(t *testing.T, b []byte) {
		pcm[MaxDecodeSamples] = 0x7a7a
		n, err := dec.Decode(ctx, b, pcm[:MaxDecodeSamples])
		switch {
		case len(b) > MaxPacket:
			if !errors.Is(err, ErrTooLong) {
				t.Fatalf("a %d-byte packet: %v", len(b), err)
			}
		case err == nil && (n < 0 || n > MaxDecodeSamples):
			t.Fatalf("decoded %d samples", n)
		}
		if pcm[MaxDecodeSamples] != 0x7a7a {
			t.Fatal("the decoder wrote past its bound")
		}
		if errors.Is(err, ErrReplaced) {
			t.Logf("the decoder trapped and was replaced: %v", err)
		}
		// The same bytes as the packet after a loss, which reads their FEC rather than their audio.
		pcm[FrameSamples] = 0x7a7a
		_, _ = dec.Recover(ctx, b, pcm[:FrameSamples])
		if pcm[FrameSamples] != 0x7a7a {
			t.Fatal("recovering wrote past its bound")
		}
		if err := dec.Reset(ctx); err != nil {
			t.Fatalf("resetting after it: %v", err)
		}
		if n, err := dec.Decode(ctx, valid, pcm); err != nil || n != FrameSamples {
			t.Fatalf("a valid packet after it: %d, %v", n, err)
		}
	})
}

// lastPacket encodes in and returns its last packet, by then the encoder's settled output.
func lastPacket(ctx context.Context, c Codec, in []int16) ([]byte, error) {
	enc, err := c.NewEncoder(ctx, DefaultEncoderOptions())
	if err != nil {
		return nil, err
	}
	defer func() { _ = enc.Close() }()
	buf := make([]byte, MaxPacket)
	n := 0
	for f := 0; f+FrameSamples <= len(in); f += FrameSamples {
		if n, err = enc.Encode(in[f:f+FrameSamples], buf); err != nil {
			return nil, err
		}
	}
	return buf[:n], nil
}
