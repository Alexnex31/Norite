// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package dsp

import (
	"context"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// module is RNNoise, WebRTC's audio processing module, abseil and the shim in csrc/, built by
// scripts/build-dsp-wasm.sh, which CI runs again and compares.
//
//go:embed dsp.wasm
var module []byte

// MemoryPages caps each instance's linear memory, in pages of 64 KiB: 8 MiB, what the module declares. A
// chain holds 3.5 MiB once both directions have run, so the cap is a bound on a bug rather than a budget.
const MemoryPages = 128

type wasmModule struct {
	rt wazero.Runtime
	cm wazero.CompiledModule
}

// New compiles the module, which takes about 200 ms, so a worker starts it while ICE connects, as it does
// the codec. It is compiled in memory and never cached on disk, for the reason the codec is not (the
// security ledger, M29).
func New(ctx context.Context) (Module, error) {
	// Not WithCloseOnContextDone, for the codec's reason: a call into an instance cannot be interrupted, and
	// what bounds one that never returns is the session's watchdog (ADR 0036).
	return newWASM(ctx, wazero.NewRuntimeConfig(), module)
}

// newWASM is New on a runtime and a module of the caller's choosing: the tests run the module through
// wazero's interpreter as well, and compare it with the build whose RNNoise has no SIMD.
func newWASM(ctx context.Context, cfg wazero.RuntimeConfig, wasm []byte) (Module, error) {
	rt := wazero.NewRuntimeWithConfig(ctx, cfg.WithMemoryLimitPages(MemoryPages))
	cm, err := rt.CompileModule(ctx, wasm)
	if err == nil {
		err = importsNothing(cm)
	}
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("dsp: %w", err)
	}
	return &wasmModule{rt: rt, cm: cm}, nil
}

// importsNothing holds the module to what ADR 0036 rests on, as internal/opus does: it is given no host
// module, so no clock, file or exit can reach it. csrc/wasi_stubs.c is what lets libc link without one.
func importsNothing(cm wazero.CompiledModule) error {
	if n := len(cm.ImportedFunctions()) + len(cm.ImportedMemories()); n > 0 {
		return fmt.Errorf("the module imports %d functions or memories, and runs with nothing to import", n)
	}
	return nil
}

func (m *wasmModule) Close(ctx context.Context) error { return m.rt.Close(ctx) }

// step is one component of the chain, as the shim exports it.
type step int

const (
	stepAEC step = iota
	stepNS
	stepAGC
)

// order is the chain's: echo cancellation on the linear signal, then RNNoise, then the gain. A test in this
// package swaps it, to show that the order is something its measure can see.
var order = [3]step{stepAEC, stepNS, stepAGC}

type wasmChain struct {
	s      Settings
	order  [3]step
	mod    api.Module
	mem    api.Memory
	state  uint32
	buf    uint32 // the chunk every call works on
	spare  uint32 // RNNoise's copy of it, when its output is discarded
	fns    map[string]api.Function
	stack  [4]uint64
	bytes  [2 * ChunkSamples]byte
	broken bool
}

var exports = []string{"dsp_alloc", "dsp_new", "dsp_render", "dsp_aec", "dsp_ns", "dsp_agc", "dsp_erle"}

func (m *wasmModule) NewChain(ctx context.Context, s Settings) (Chain, error) {
	// Anonymous, so any number can run at once; _initialize runs the C++ constructors.
	mod, err := m.rt.InstantiateModule(ctx, m.cm, wazero.NewModuleConfig().WithName("").
		WithStartFunctions("_initialize"))
	if err != nil {
		return nil, fmt.Errorf("dsp: starting an instance: %w", err)
	}
	c := &wasmChain{s: s, order: order, mod: mod, mem: mod.Memory(), fns: map[string]api.Function{}}
	for _, name := range exports {
		f := mod.ExportedFunction(name)
		if f == nil {
			_ = mod.Close(ctx)
			return nil, fmt.Errorf("dsp: the module exports no %s", name)
		}
		c.fns[name] = f
	}
	if c.mem == nil {
		_ = mod.Close(ctx)
		return nil, errors.New("dsp: the module exports no memory")
	}
	for _, at := range []*uint32{&c.buf, &c.spare} {
		p, err := c.call("dsp_alloc", 2*ChunkSamples)
		if err != nil || p == 0 {
			_ = mod.Close(ctx)
			return nil, errors.New("dsp: an instance could not allocate its buffers")
		}
		*at = uint32(p)
	}
	st, err := c.call("dsp_new")
	if err != nil || st == 0 {
		_ = mod.Close(ctx)
		return nil, errors.New("dsp: the chain could not be created")
	}
	c.state = uint32(st)
	return c, nil
}

// call runs an export with args and returns its one result's bits.
func (c *wasmChain) call(name string, args ...uint64) (uint64, error) {
	copy(c.stack[:], args)
	if err := c.fns[name].CallWithStack(context.Background(), c.stack[:]); err != nil {
		c.broken = true
		return 0, fmt.Errorf("dsp: %s: %w", name, err)
	}
	return c.stack[0], nil
}

// code runs an export returning the APM's error code.
func (c *wasmChain) code(name string, at uint32) error {
	r, err := c.call(name, uint64(c.state), uint64(at))
	if err != nil {
		return err
	}
	if code := int32(uint32(r)); code != 0 { //nolint:gosec // an i32 result, carried in a uint64
		c.broken = true
		return apmError(name, code)
	}
	return nil
}

// put writes samples into linear memory, which is little-endian whatever the host is.
func (c *wasmChain) put(at uint32, pcm []int16) error {
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(c.bytes[2*i:], uint16(s)) //nolint:gosec // a sample's bits
	}
	if !c.mem.Write(at, c.bytes[:]) {
		return errShort
	}
	return nil
}

func (c *wasmChain) get(at uint32, pcm []int16) error {
	b, ok := c.mem.Read(at, 2*ChunkSamples)
	if !ok {
		return errShort
	}
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(b[2*i:])) //nolint:gosec // a sample's bits
	}
	return nil
}

func (c *wasmChain) Render(ref []int16) error {
	switch {
	case c.broken:
		return ErrBroken
	case len(ref) != ChunkSamples:
		return errShort
	case !c.s.EchoCancellation:
		return nil
	}
	if err := c.put(c.buf, ref); err != nil {
		return err
	}
	return c.code("dsp_render", c.buf)
}

func (c *wasmChain) Capture(pcm []int16) (float32, error) {
	switch {
	case c.broken:
		return 0, ErrBroken
	case len(pcm) != ChunkSamples:
		return 0, errShort
	}
	if err := c.put(c.buf, pcm); err != nil {
		return 0, err
	}
	var voice float32
	for _, st := range c.order {
		var err error
		switch {
		case st == stepAEC && c.s.EchoCancellation:
			err = c.code("dsp_aec", c.buf)
		case st == stepAGC && c.s.AutoGain:
			err = c.code("dsp_agc", c.buf)
		case st == stepNS:
			voice, err = c.ns()
		}
		if err != nil {
			return 0, err
		}
	}
	return voice, c.get(c.buf, pcm)
}

// ns runs RNNoise on the chunk, or on a copy of it when its output is to be discarded, so that the
// probability is the same whichever the setting.
func (c *wasmChain) ns() (float32, error) {
	at := c.buf
	if !c.s.NoiseSuppression {
		b, ok := c.mem.Read(c.buf, 2*ChunkSamples)
		if !ok || !c.mem.Write(c.spare, b) {
			return 0, errShort
		}
		at = c.spare
	}
	r, err := c.call("dsp_ns", uint64(c.state), uint64(at))
	if err != nil {
		return 0, err
	}
	p := api.DecodeF32(r)
	if p != p || p < 0 || p > 1 { // NaN, or out of range: no correct RNNoise returns either
		c.broken = true
		return 0, fmt.Errorf("dsp: RNNoise returned a voice probability of %v", p)
	}
	return p, nil
}

func (c *wasmChain) Close() error {
	if c.mod != nil {
		_ = c.mod.Close(context.Background())
		c.mod = nil
	}
	c.broken = true
	return nil
}
