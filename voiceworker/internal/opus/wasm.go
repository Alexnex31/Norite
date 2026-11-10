// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package opus

import (
	"context"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/Alexnex31/Norite/voiceworker/internal/wasmrt"
)

// module is libopus and shim.c, built by scripts/build-opus-wasm.sh, which CI runs again and compares.
//
//go:embed opus.wasm
var module []byte

// MemoryPages caps each instance's linear memory, in pages of 64 KiB: 4 MiB, what the module declares. It
// starts at 1 MiB and a decoder never grows it, so the cap is a bound on a bug rather than a budget.
const MemoryPages = 64

// wasmCodec is one worker's runtime: the module compiled once, and instantiated per encoder and decoder.
type wasmCodec struct {
	rt wazero.Runtime
	cm wazero.CompiledModule
}

// New compiles the module, which takes about 100 ms, so a worker starts it while ICE connects. It is
// compiled in memory and never cached on disk: a cache would be native code the account can write and the
// worker would run (the security ledger, M29).
//
// wazero compiles to native code on every platform the release builds for; on an amd64 CPU without SSE4.1,
// or where the OS refuses executable memory, it interprets instead, slower and otherwise the same.
func New(ctx context.Context) (Codec, error) {
	// Not WithCloseOnContextDone: it checks for cancellation through every loop and call, and measured five
	// times the cost of a frame (2.9 ms against 0.59). So a call into an instance cannot be interrupted, and
	// what bounds a decoder that never returns is the session's watchdog, which ends the call, and the
	// worker with it.
	return newWASM(ctx, wazero.NewRuntimeConfig())
}

// newWASM is New on a runtime of the caller's choosing: the differential test runs the module through
// wazero's interpreter as well, as the reference its compiler must agree with.
func newWASM(ctx context.Context, cfg wazero.RuntimeConfig) (Codec, error) {
	wasmrt.Warm() // wazero races on its version cache otherwise (internal/wasmrt)
	rt := wazero.NewRuntimeWithConfig(ctx, cfg.WithMemoryLimitPages(MemoryPages))
	cm, err := rt.CompileModule(ctx, module)
	if err == nil {
		err = importsNothing(cm)
	}
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("opus: %w", err)
	}
	return &wasmCodec{rt: rt, cm: cm}, nil
}

// importsNothing holds the module to what ADR 0036 rests on: it is given no host module, so nothing it
// imports could be satisfied anyway, and this says why rather than failing at the first instance.
func importsNothing(cm wazero.CompiledModule) error {
	if n := len(cm.ImportedFunctions()) + len(cm.ImportedMemories()); n > 0 {
		return fmt.Errorf("the module imports %d functions or memories, and runs with nothing to import", n)
	}
	return nil
}

func (c *wasmCodec) Close(ctx context.Context) error { return c.rt.Close(ctx) }

// instance is one module instance, holding one encoder or one decoder, and the two buffers every call
// passes through: samples, and a packet.
type instance struct {
	mod                                    api.Module
	mem                                    api.Memory
	newEncoder, setBitrate, encode, newDec api.Function
	decode                                 api.Function
	pcm, pkt                               uint32
	state                                  uint32
	stack                                  [8]uint64
}

func (c *wasmCodec) instantiate(ctx context.Context) (*instance, error) {
	// Anonymous, so any number can run at once; _initialize runs libc's constructors.
	mod, err := c.rt.InstantiateModule(ctx, c.cm, wazero.NewModuleConfig().WithName("").
		WithStartFunctions("_initialize"))
	if err != nil {
		return nil, fmt.Errorf("opus: starting an instance: %w", err)
	}
	in := &instance{mod: mod, mem: mod.Memory()}
	fns := map[string]*api.Function{"shim_encoder": &in.newEncoder, "shim_set_bitrate": &in.setBitrate,
		"shim_encode": &in.encode, "shim_decoder": &in.newDec, "shim_decode": &in.decode}
	for name, f := range fns {
		if *f = mod.ExportedFunction(name); *f == nil {
			_ = mod.Close(ctx)
			return nil, fmt.Errorf("opus: the module exports no %s", name)
		}
	}
	alloc := mod.ExportedFunction("shim_alloc")
	if alloc == nil || in.mem == nil {
		_ = mod.Close(ctx)
		return nil, errors.New("opus: the module exports no allocator or memory")
	}
	for _, b := range []struct {
		at   *uint32
		size uint64
	}{{&in.pcm, MaxDecodeSamples * 2}, {&in.pkt, MaxPacket}} {
		p, err := in.call(ctx, alloc, b.size)
		if err != nil || p == 0 {
			_ = mod.Close(ctx)
			return nil, errors.New("opus: an instance could not allocate its buffers")
		}
		*b.at = uint32(p)
	}
	return in, nil
}

// call runs f with args and returns its one result, which every export has, as libopus's int.
func (in *instance) call(ctx context.Context, f api.Function, args ...uint64) (int32, error) {
	copy(in.stack[:], args)
	if err := f.CallWithStack(ctx, in.stack[:]); err != nil {
		return 0, err
	}
	return int32(uint32(in.stack[0])), nil //nolint:gosec // an i32 result, carried in a uint64
}

func (in *instance) close(ctx context.Context) {
	if in != nil {
		_ = in.mod.Close(ctx)
	}
}

// putSamples writes samples into linear memory, which is little-endian whatever the host is.
func (in *instance) putSamples(pcm []int16) error {
	b, ok := in.mem.Read(in.pcm, uint32(len(pcm)*2)) //nolint:gosec // at most MaxDecodeSamples
	if !ok {
		return errShort
	}
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(s)) //nolint:gosec // a sample's bits
	}
	return nil
}

func (in *instance) samples(pcm []int16, n int32) (int, error) {
	if n < 0 {
		return 0, codeError("decoding", n)
	}
	if int(n) > len(pcm) {
		return 0, fmt.Errorf("opus: the decoder wrote %d samples into %d", n, len(pcm))
	}
	b, ok := in.mem.Read(in.pcm, uint32(n)*2) //nolint:gosec // bounded above
	if !ok {
		return 0, errShort
	}
	for i := range int(n) {
		pcm[i] = int16(binary.LittleEndian.Uint16(b[2*i:])) //nolint:gosec // a sample's bits
	}
	return int(n), nil
}

func (in *instance) putPacket(p []byte) error {
	b, ok := in.mem.Read(in.pkt, uint32(len(p))) //nolint:gosec // at most MaxPacket
	if !ok {
		return errShort
	}
	copy(b, p)
	return nil
}

// Encoder

type wasmEncoder struct {
	in *instance
}

func (c *wasmCodec) NewEncoder(ctx context.Context, o EncoderOptions) (Encoder, error) {
	if err := o.check(); err != nil {
		return nil, err
	}
	in, err := c.instantiate(ctx)
	if err != nil {
		return nil, err
	}
	st, err := in.call(ctx, in.newEncoder, uint64(o.Bitrate), uint64(o.Complexity), flag(o.FEC), flag(o.DTX),
		uint64(o.LossPercent))
	if err != nil || st == 0 {
		in.close(ctx)
		return nil, errors.New("opus: the encoder could not be created")
	}
	in.state = uint32(st)
	return &wasmEncoder{in: in}, nil
}

func flag(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// Encode encodes local samples, which nobody else chose, so a failure here is a bug in this program and
// is returned rather than survived.
func (e *wasmEncoder) Encode(pcm []int16, out []byte) (int, error) {
	if len(pcm) != FrameSamples || len(out) < MaxPacket {
		return 0, errShort
	}
	in := e.in
	if err := in.putSamples(pcm); err != nil {
		return 0, err
	}
	n, err := in.call(context.Background(), in.encode, uint64(in.state), uint64(in.pcm), FrameSamples,
		uint64(in.pkt), MaxPacket)
	if err != nil {
		return 0, fmt.Errorf("opus: encoding: %w", err)
	}
	if n < 0 {
		return 0, codeError("encoding", n)
	}
	b, ok := in.mem.Read(in.pkt, uint32(n)) //nolint:gosec // n is at most MaxPacket, as asked
	if !ok || int(n) > len(out) {
		return 0, errShort
	}
	return copy(out, b), nil
}

func (e *wasmEncoder) SetBitrate(bitrate int) error {
	if err := (EncoderOptions{Bitrate: bitrate}).check(); err != nil {
		return err
	}
	r, err := e.in.call(context.Background(), e.in.setBitrate, uint64(e.in.state), uint64(bitrate))
	if err != nil {
		return fmt.Errorf("opus: setting the bitrate: %w", err)
	}
	if r != 0 {
		return codeError("setting the bitrate", r)
	}
	return nil
}

func (e *wasmEncoder) Close() error {
	e.in.close(context.Background())
	return nil
}

// Decoder

type wasmDecoder struct {
	c  *wasmCodec
	in *instance
}

func (c *wasmCodec) NewDecoder(ctx context.Context) (Decoder, error) {
	d := &wasmDecoder{c: c}
	if err := d.start(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *wasmDecoder) start(ctx context.Context) error {
	in, err := d.c.instantiate(ctx)
	if err != nil {
		return err
	}
	st, err := in.call(ctx, in.newDec)
	if err != nil || st == 0 {
		in.close(ctx)
		return errors.New("opus: the decoder could not be created")
	}
	in.state = uint32(st)
	d.in = in
	return nil
}

// replace discards an instance a call trapped in and starts another, even when ctx is done, since the next
// packet needs it.
func (d *wasmDecoder) replace(ctx context.Context, cause error) error {
	d.in.close(ctx)
	d.in = nil
	if err := d.start(context.WithoutCancel(ctx)); err != nil {
		return errors.Join(ErrReplaced, cause, err)
	}
	return errors.Join(ErrReplaced, cause)
}

// run decodes what is already in the instance's packet buffer: n bytes, or none to conceal.
func (d *wasmDecoder) run(ctx context.Context, n int, frame int, fec bool, pcm []int16) (int, error) {
	if d.in == nil {
		if err := d.start(ctx); err != nil {
			return 0, err
		}
	}
	in := d.in
	data := uint64(in.pkt)
	if n == 0 {
		data = 0
	}
	//nolint:gosec // n and frame are bounded by MaxPacket and MaxDecodeSamples
	r, err := in.call(ctx, in.decode, uint64(in.state), data, uint64(n), uint64(in.pcm), uint64(frame), flag(fec))
	if err != nil {
		return 0, d.replace(ctx, err)
	}
	return in.samples(pcm, r)
}

func (d *wasmDecoder) load(ctx context.Context, packet []byte) error {
	if len(packet) > MaxPacket {
		return ErrTooLong
	}
	if d.in == nil {
		if err := d.start(ctx); err != nil {
			return err
		}
	}
	return d.in.putPacket(packet)
}

func (d *wasmDecoder) Decode(ctx context.Context, packet []byte, pcm []int16) (int, error) {
	if len(pcm) < MaxDecodeSamples {
		return 0, errShort
	}
	if err := d.load(ctx, packet); err != nil {
		return 0, err
	}
	frame := MaxDecodeSamples
	if len(packet) == 0 {
		frame = FrameSamples
	}
	return d.run(ctx, len(packet), frame, false, pcm)
}

func (d *wasmDecoder) Conceal(ctx context.Context, pcm []int16) (int, error) {
	if len(pcm) < FrameSamples {
		return 0, errShort
	}
	return d.run(ctx, 0, FrameSamples, false, pcm)
}

func (d *wasmDecoder) Recover(ctx context.Context, packet []byte, pcm []int16) (int, error) {
	if len(pcm) < FrameSamples {
		return 0, errShort
	}
	if err := d.load(ctx, packet); err != nil {
		return 0, err
	}
	return d.run(ctx, len(packet), FrameSamples, len(packet) > 0, pcm)
}

// Reset starts a fresh instance rather than asking the old one to forget. A reset means the slot has a new
// sender, and an instance a previous sender's packets took over through a libopus bug would ignore the
// request and go on making the audio the new sender is heard to say. Starting one costs about 0.1 ms, once
// per change of sender (the security ledger, M29).
func (d *wasmDecoder) Reset(ctx context.Context) error {
	d.in.close(ctx)
	d.in = nil
	return d.start(ctx)
}

func (d *wasmDecoder) Close() error {
	d.in.close(context.Background())
	d.in = nil
	return nil
}
