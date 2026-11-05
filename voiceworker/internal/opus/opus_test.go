// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package opus

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/tetratelabs/wazero"
)

// native is set by native_test.go under the norite_opusnative tag, so that every test here runs against both
// implementations when it is passed (ADR 0036: switching is changing the default, so the twin is held to the
// same results).
var native func() Codec

func codecs(t *testing.T) map[string]Codec {
	t.Helper()
	ctx := context.Background()
	c, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(ctx) })
	all := map[string]Codec{"wasm": c}
	if native != nil {
		n := native()
		t.Cleanup(func() { _ = n.Close(ctx) })
		all["native"] = n
	}
	return all
}

// tone is a sine of hz at amplitude amp, n samples, starting at sample offset.
func tone(hz, amp float64, offset, n int) []int16 {
	pcm := make([]int16, n)
	for i := range pcm {
		pcm[i] = int16(amp * math.Sin(2*math.Pi*hz*float64(offset+i)/SampleRate))
	}
	return pcm
}

// similarity is the best normalized correlation between want and got over the lags a codec's delay can
// take: Opus delays by its lookahead, 6.5 ms, and somewhat more in the decoder.
func similarity(want, got []int16) float64 {
	best := 0.0
	for lag := 0; lag < SampleRate/50; lag++ {
		var xy, xx, yy float64
		for i := 0; i+lag < len(got) && i < len(want); i++ {
			x, y := float64(want[i]), float64(got[i+lag])
			xy, xx, yy = xy+x*y, xx+x*x, yy+y*y
		}
		if xx > 0 && yy > 0 {
			best = max(best, xy/math.Sqrt(xx*yy))
		}
	}
	return best
}

// roundTrip encodes frames of in and decodes them all, returning what came out and the packets.
func roundTrip(t *testing.T, c Codec, in []int16) ([]int16, [][]byte) {
	t.Helper()
	ctx := context.Background()
	enc, err := c.NewEncoder(ctx, DefaultEncoderOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = enc.Close() }()
	dec, err := c.NewDecoder(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dec.Close() }()
	var out []int16
	var packets [][]byte
	buf := make([]byte, MaxPacket)
	pcm := make([]int16, MaxDecodeSamples)
	for f := 0; f+FrameSamples <= len(in); f += FrameSamples {
		n, err := enc.Encode(in[f:f+FrameSamples], buf)
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, append([]byte(nil), buf[:n]...))
		m, err := dec.Decode(ctx, buf[:n], pcm)
		if err != nil {
			t.Fatal(err)
		}
		if m != FrameSamples {
			t.Fatalf("a 20 ms packet decoded to %d samples", m)
		}
		out = append(out, pcm[:m]...)
	}
	return out, packets
}

func TestATonePassesThroughTheCodec(t *testing.T) {
	for name, c := range codecs(t) {
		t.Run(name, func(t *testing.T) {
			in := tone(440, 8000, 0, SampleRate)
			out, packets := roundTrip(t, c, in)
			// The first frames are the encoder settling; judge the second half.
			if s := similarity(in[SampleRate/2:], out[SampleRate/2:]); s < 0.9 {
				t.Errorf("a 440 Hz tone came back with similarity %.3f", s)
			}
			bytes := 0
			for _, p := range packets {
				bytes += len(p)
			}
			// 64 kbps is 160 bytes a frame; VBR wanders, but not by a factor of two.
			if avg := bytes / len(packets); avg < 80 || avg > 320 {
				t.Errorf("packets average %d bytes at 64 kbps", avg)
			}
		})
	}
}

func TestSilenceIsSentAsDTX(t *testing.T) {
	for name, c := range codecs(t) {
		t.Run(name, func(t *testing.T) {
			_, packets := roundTrip(t, c, make([]int16, SampleRate))
			small := 0
			for _, p := range packets {
				if len(p) <= 2 {
					small++
				}
			}
			if small < len(packets)/2 {
				t.Errorf("%d of %d silent frames were sent as DTX", small, len(packets))
			}
		})
	}
}

// What the two implementations produce must agree closely enough that switching is not audible: the same
// tone through each, compared with each other.
func TestTheTwinsAgree(t *testing.T) {
	all := codecs(t)
	if len(all) < 2 {
		t.Skip("the native twin needs -tags norite_opusnative")
	}
	in := tone(300, 6000, 0, SampleRate)
	w, _ := roundTrip(t, all["wasm"], in)
	n, _ := roundTrip(t, all["native"], in)
	if s := similarity(w[SampleRate/2:], n[SampleRate/2:]); s < 0.99 {
		t.Errorf("the two implementations' outputs have similarity %.3f", s)
	}
}

func TestALostFrameIsConcealedOrRecovered(t *testing.T) {
	ctx := context.Background()
	for name, c := range codecs(t) {
		t.Run(name, func(t *testing.T) {
			// Speech-like rather than a pure tone, so the encoder uses SILK and puts FEC in its packets.
			in := make([]int16, SampleRate)
			for i := range in {
				env := 0.5 + 0.5*math.Sin(2*math.Pi*3*float64(i)/SampleRate)
				in[i] = int16(env * (4000*math.Sin(2*math.Pi*180*float64(i)/SampleRate) +
					2000*math.Sin(2*math.Pi*720*float64(i)/SampleRate)))
			}
			_, packets := roundTrip(t, c, in)
			dec, err := c.NewDecoder(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = dec.Close() }()
			pcm := make([]int16, MaxDecodeSamples)
			for i, p := range packets {
				switch i {
				case 20:
					if n, err := dec.Conceal(ctx, pcm); err != nil || n != FrameSamples {
						t.Fatalf("concealing: %d, %v", n, err)
					}
					continue
				case 30:
					continue // lost, and recovered from the next packet's FEC
				case 31:
					if n, err := dec.Recover(ctx, p, pcm); err != nil || n != FrameSamples {
						t.Fatalf("recovering: %d, %v", n, err)
					}
				}
				if n, err := dec.Decode(ctx, p, pcm); err != nil || n != FrameSamples {
					t.Fatalf("packet %d: %d, %v", i, n, err)
				}
			}
		})
	}
}

func TestAPacketLongerThanOpusAllowsIsRefused(t *testing.T) {
	ctx := context.Background()
	for name, c := range codecs(t) {
		t.Run(name, func(t *testing.T) {
			dec, err := c.NewDecoder(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = dec.Close() }()
			pcm := make([]int16, MaxDecodeSamples)
			if _, err := dec.Decode(ctx, make([]byte, MaxPacket+1), pcm); !errors.Is(err, ErrTooLong) {
				t.Errorf("a %d-byte packet: %v", MaxPacket+1, err)
			}
			if _, err := dec.Recover(ctx, make([]byte, MaxPacket+1), pcm); !errors.Is(err, ErrTooLong) {
				t.Errorf("a %d-byte packet to recover from: %v", MaxPacket+1, err)
			}
		})
	}
}

// ADR 0036 rests on the module importing nothing: it is run with no host module, so the only thing a libopus
// bug can reach is the instance's own memory.
func TestTheModuleImportsNothing(t *testing.T) {
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	defer func() { _ = rt.Close(ctx) }()
	cm, err := rt.CompileModule(ctx, module)
	if err != nil {
		t.Fatal(err)
	}
	if fns, mems := cm.ImportedFunctions(), cm.ImportedMemories(); len(fns)+len(mems) != 0 {
		for _, f := range fns {
			m, n, _ := f.Import()
			t.Errorf("the module imports %s.%s", m, n)
		}
		t.Errorf("and %d memories", len(mems))
	}
	if importsNothing(cm) != nil {
		t.Error("New's own check refuses the module")
	}
	mem := cm.ExportedMemories()["memory"]
	if mem == nil {
		t.Fatal("the module exports no memory")
	}
	if max, ok := mem.Max(); !ok || max > MemoryPages {
		t.Errorf("the module declares at most %d pages (%v), past the cap of %d", max, ok, MemoryPages)
	}
}

// A decoder whose instance traps is replaced: the call that trapped reports it, and the next packet decodes
// from a fresh instance rather than failing for the rest of the call.
func TestADecoderThatTrapsIsReplaced(t *testing.T) {
	ctx := context.Background()
	c, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	_, packets := roundTrip(t, c, tone(440, 8000, 0, FrameSamples*5))
	d, err := c.NewDecoder(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	dec := d.(*wasmDecoder)
	trapped := dec.in
	// A state pointer past the end of linear memory, which the module never grows into: libopus reads
	// through it, and the access traps.
	dec.in.state = dec.in.mem.Size() + 0x10000
	pcm := make([]int16, MaxDecodeSamples)
	if _, err := dec.Decode(ctx, packets[0], pcm); !errors.Is(err, ErrReplaced) {
		t.Fatalf("the trapping call returned %v", err)
	}
	if dec.in == trapped {
		t.Fatal("the instance that trapped is still in use")
	}
	for i, p := range packets[1:] {
		if n, err := dec.Decode(ctx, p, pcm); err != nil || n != FrameSamples {
			t.Fatalf("packet %d after the trap: %d, %v", i+1, n, err)
		}
	}
}

func TestEncoderOptionsAreChecked(t *testing.T) {
	ctx := context.Background()
	for name, c := range codecs(t) {
		t.Run(name, func(t *testing.T) {
			for _, o := range []EncoderOptions{{Bitrate: 5999}, {Bitrate: 64000, Complexity: 11},
				{Bitrate: 64000, LossPercent: 101}} {
				if e, err := c.NewEncoder(ctx, o); err == nil {
					_ = e.Close()
					t.Errorf("%+v was accepted", o)
				}
			}
			e, err := c.NewEncoder(ctx, DefaultEncoderOptions())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = e.Close() }()
			if err := e.SetBitrate(32000); err != nil {
				t.Error(err)
			}
			if err := e.SetBitrate(1); err == nil {
				t.Error("a bitrate of 1 was accepted")
			}
		})
	}
}

// A reset is a new sender in the slot, and it gets an instance of its own: one the previous sender took over
// through a libopus bug would otherwise ignore a reset asked of it, and speak for the next sender.
func TestAResetDecoderIsANewInstance(t *testing.T) {
	ctx := context.Background()
	c, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	_, packets := roundTrip(t, c, tone(440, 8000, 0, FrameSamples*3))
	d, err := c.NewDecoder(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	dec := d.(*wasmDecoder)
	before := dec.in
	if err := d.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if dec.in == before {
		t.Fatal("the reset decoder runs in the instance it had")
	}
	if !before.mod.IsClosed() {
		t.Error("the previous sender's instance is still open")
	}
	pcm := make([]int16, MaxDecodeSamples)
	if n, err := d.Decode(ctx, packets[1], pcm); err != nil || n != FrameSamples {
		t.Fatalf("after the reset: %d, %v", n, err)
	}
}
