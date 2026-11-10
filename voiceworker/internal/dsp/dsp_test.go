// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package dsp

import (
	"context"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tetratelabs/wazero"

	"github.com/Alexnex31/Norite/voiceworker/internal/audio/audiotest"
)

// scene is a call as the chain hears it, in chunks: what the speakers played, and what the microphone
// recorded, which is that played audio coming back through a short echo path, a near-end talker in
// stretches, and background noise.
type scene struct {
	far, mic [][]int16
}

// newScene makes seconds of a scene. The near end talks in seconds 3 to 5 of every 8, so there are
// stretches of far end alone, of both at once, and of near end alone.
func newScene(seconds int, seed uint64) scene {
	far := audiotest.SpeechLike(seconds, seed, 120)
	near := audiotest.SpeechLike(seconds, seed+1, 210)
	rng := rand.New(rand.NewPCG(seed, 99)) //nolint:gosec // a deterministic test signal
	// 40 ms of delay, then a few decaying taps, at about -8 dB.
	taps := map[int]float64{0: 0.3, 48: 0.15, 96: -0.1, 240: 0.06, 480: 0.03}
	const delay = SampleRate * 40 / 1000
	var s scene
	for c := 0; (c+1)*ChunkSamples <= len(far); c++ {
		f := make([]int16, ChunkSamples)
		m := make([]int16, ChunkSamples)
		for k := range ChunkSamples {
			i := c*ChunkSamples + k
			f[k] = far[i]
			v := 400 * rng.NormFloat64()
			for d, g := range taps {
				if j := i - delay - d; j >= 0 {
					v += g * float64(far[j])
				}
			}
			if t := i / SampleRate % 8; t >= 3 && t < 5 {
				v += float64(near[i])
			}
			m[k] = int16(max(math.MinInt16, min(math.MaxInt16, math.Round(v))))
		}
		s.far, s.mic = append(s.far, f), append(s.mic, m)
	}
	return s
}

// run feeds a scene to a chain as the session does, the reference first, and returns everything it gave
// back: each chunk's samples, then its voice probability's bits, then any error folded in, so that a
// difference anywhere is a difference in the bytes.
func run(t testing.TB, c Chain, s scene, chunks int) []byte {
	t.Helper()
	var out []byte
	pcm := make([]int16, ChunkSamples)
	for i := range min(chunks, len(s.mic)) {
		if err := c.Render(s.far[i]); err != nil {
			t.Fatalf("chunk %d: render: %v", i, err)
		}
		copy(pcm, s.mic[i])
		p, err := c.Capture(pcm)
		if err != nil {
			t.Fatalf("chunk %d: capture: %v", i, err)
		}
		for _, v := range pcm {
			out = binary.LittleEndian.AppendUint16(out, uint16(v)) //nolint:gosec // a sample's bits
		}
		out = binary.LittleEndian.AppendUint32(out, math.Float32bits(p))
	}
	return out
}

// every is each combination of the three settings.
func every() []Settings {
	var all []Settings
	for b := range 8 {
		all = append(all, Settings{EchoCancellation: b&1 != 0, NoiseSuppression: b&2 != 0, AutoGain: b&4 != 0})
	}
	return all
}

// shared is the module compiled once for the package's tests: a compile costs 0.2 s, and 1.5 s under -race,
// and every chain is an instance of its own, so sharing it costs no test its isolation.
var shared = struct {
	once sync.Once
	mod  Module
	err  error
}{}

func newModule(t testing.TB) Module {
	t.Helper()
	shared.once.Do(func() { shared.mod, shared.err = New(context.Background()) })
	require.NoError(t, shared.err)
	return shared.mod
}

func newChain(t testing.TB, m Module, s Settings) Chain {
	t.Helper()
	c, err := m.NewChain(context.Background(), s)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestTheModuleImportsNothing(t *testing.T) {
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	defer func() { _ = rt.Close(ctx) }()
	cm, err := rt.CompileModule(ctx, module)
	require.NoError(t, err)
	require.Empty(t, cm.ImportedFunctions())
	require.Empty(t, cm.ImportedMemories())
}

// wazero's compiler must run the module exactly as its interpreter does, as internal/opus's must: the
// chain is deterministic, so the two give the same bytes or one of them is wrong. CI runs this on amd64
// and arm64.
func TestTheCompilerAgreesWithTheInterpreter(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the chain through wazero's interpreter, about 35 ms a chunk")
	}
	ctx := context.Background()
	compiled := newModule(t)
	interpreted, err := newWASM(ctx, wazero.NewRuntimeConfigInterpreter(), module)
	require.NoError(t, err)
	defer func() { _ = interpreted.Close(ctx) }()

	// Ten seconds would reach double-talk; the interpreter makes that a minute, so the scene starts two
	// seconds before it. Every component on for three seconds, and each alone for one.
	s := newScene(6, 5)
	s.far, s.mic = s.far[100:], s.mic[100:]
	for _, c := range []struct {
		name   string
		s      Settings
		chunks int
	}{
		{"all", AllOn(), 300},
		{"aec", Settings{EchoCancellation: true}, 100},
		{"ns", Settings{NoiseSuppression: true}, 100},
		{"agc", Settings{AutoGain: true}, 100},
	} {
		t.Run(c.name, func(t *testing.T) {
			ci, err := interpreted.NewChain(ctx, c.s)
			require.NoError(t, err)
			defer func() { _ = ci.Close() }()
			want := run(t, ci, s, c.chunks)
			got := run(t, newChain(t, compiled, c.s), s, c.chunks)
			require.Equal(t, len(want), len(got))
			if i := firstDifference(want, got); i >= 0 {
				t.Fatalf("the chain gives different bytes compiled and interpreted, from chunk %d", i)
			}
		})
	}
}

// RNNoise's SIMD kernels (csrc/rnnoise/vec_wasm.h) compute what its plain C computes, bit for bit, so the
// module and its scalar twin are the same function. That is a property of the module, and WebAssembly is
// the same on every host, so it is checked where the module is built: CI's codegen job builds the twin
// with scripts/build-dsp-wasm.sh --scalar and names it here.
func TestSIMDAgreesWithPlainC(t *testing.T) {
	path := os.Getenv("NORITE_DSP_SCALAR_WASM")
	if path == "" {
		t.Skip("NORITE_DSP_SCALAR_WASM names no scalar build (scripts/build-dsp-wasm.sh --scalar)")
	}
	scalarWASM, err := os.ReadFile(path) //nolint:gosec // a path the test's caller chose
	require.NoError(t, err)
	require.NotEqual(t, module, scalarWASM, "the scalar build is the module itself")
	ctx := context.Background()
	simd := newModule(t)
	scalar, err := newWASM(ctx, wazero.NewRuntimeConfig(), scalarWASM)
	require.NoError(t, err)
	defer func() { _ = scalar.Close(ctx) }()
	s := newScene(20, 11)
	for _, set := range every() {
		cs, err := scalar.NewChain(ctx, set)
		require.NoError(t, err)
		want := run(t, cs, s, len(s.mic))
		_ = cs.Close()
		got := run(t, newChain(t, simd, set), s, len(s.mic))
		if i := firstDifference(want, got); i >= 0 {
			t.Fatalf("%+v: the SIMD build differs from plain C from chunk %d", set, i)
		}
	}
}

// firstDifference is the chunk run's two outputs first differ in, or -1.
func firstDifference(a, b []byte) int {
	const chunk = 2*ChunkSamples + 4
	for i := 0; i < min(len(a), len(b)); i += chunk {
		if string(a[i:min(i+chunk, len(a))]) != string(b[i:min(i+chunk, len(b))]) {
			return i / chunk
		}
	}
	if len(a) != len(b) {
		return min(len(a), len(b)) / chunk
	}
	return -1
}

func TestWithEverythingOffTheChainChangesNothing(t *testing.T) {
	c := newChain(t, newModule(t), Settings{})
	s := newScene(2, 3)
	pcm := make([]int16, ChunkSamples)
	for i := range s.mic {
		require.NoError(t, c.Render(s.far[i]))
		copy(pcm, s.mic[i])
		_, err := c.Capture(pcm)
		require.NoError(t, err)
		require.Equal(t, s.mic[i], pcm, "chunk %d", i)
	}
}

// RNNoise runs whatever the noise setting, for the probability (M35's voice detection reads it), and with
// the setting off it reads exactly what it would have read with it on.
func TestTheVoiceProbabilityDoesNotDependOnTheNoiseSetting(t *testing.T) {
	m := newModule(t)
	on := newChain(t, m, Settings{NoiseSuppression: true})
	off := newChain(t, m, Settings{})
	s := newScene(4, 8)
	a, b := make([]int16, ChunkSamples), make([]int16, ChunkSamples)
	var voiced int
	for i := range s.mic {
		copy(a, s.mic[i])
		copy(b, s.mic[i])
		pa, err := on.Capture(a)
		require.NoError(t, err)
		pb, err := off.Capture(b)
		require.NoError(t, err)
		require.Equal(t, pa, pb, "chunk %d", i)
		require.Equal(t, s.mic[i], b, "chunk %d: suppression off changed the samples", i)
		if pa >= 0.5 {
			voiced++
		}
	}
	require.Positive(t, voiced, "RNNoise heard no voice in four seconds of speech")
}

// A chain a call failed in is not trusted again: the instance's state is whatever the failure left.
func TestAChainThatFailedStaysFailed(t *testing.T) {
	c := newChain(t, newModule(t), AllOn())
	pcm := make([]int16, ChunkSamples)
	_, err := c.Capture(pcm)
	require.NoError(t, err)
	wc := c.(*wasmChain)
	wc.state = 0xfffffff0 // past the end of memory: the next call traps
	_, err = c.Capture(pcm)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrBroken)
	wc.state = 0 // even pointed somewhere harmless again
	_, err = c.Capture(pcm)
	require.ErrorIs(t, err, ErrBroken)
	require.ErrorIs(t, c.Render(pcm), ErrBroken)
}

func TestACallTakesExactlyOneChunk(t *testing.T) {
	c := newChain(t, newModule(t), AllOn())
	_, err := c.Capture(make([]int16, ChunkSamples-1))
	require.Error(t, err)
	require.Error(t, c.Render(make([]int16, 2*ChunkSamples)))
	_, err = c.Capture(make([]int16, ChunkSamples))
	require.NoError(t, err, "a refused size must not break the chain")
}

// The memory cap is a bound on a bug: a chain that has run both directions for a while stays well under it.
func TestAChainStaysWellUnderItsMemoryCap(t *testing.T) {
	c := newChain(t, newModule(t), AllOn())
	run(t, c, newScene(10, 21), 1000)
	pages := c.(*wasmChain).mem.Size() / 65536
	t.Logf("a chain after 10 s holds %d pages of %d", pages, MemoryPages)
	require.LessOrEqual(t, pages, uint32(MemoryPages/2))
}

// TestTheChainDelaysByDelay measures the chain's delay as the lag at which its output best matches its
// input, on speech with nothing played, so the latency budget the session holds can add it.
func TestTheChainDelaysByDelay(t *testing.T) {
	c := newChain(t, newModule(t), AllOn())
	in := audiotest.SpeechLike(6, 4, 150)
	out := make([]int16, 0, len(in))
	pcm, quiet := make([]int16, ChunkSamples), make([]int16, ChunkSamples)
	for i := 0; i+ChunkSamples <= len(in); i += ChunkSamples {
		require.NoError(t, c.Render(quiet))
		copy(pcm, in[i:])
		_, err := c.Capture(pcm)
		require.NoError(t, err)
		out = append(out, pcm...)
	}
	best, lag := math.Inf(-1), 0
	for l := range 2 * DelaySamples {
		var s float64
		for i := SampleRate; i+l < len(out); i++ { // past the first second, which AGC2 spends adapting
			s += float64(in[i]) * float64(out[i+l])
		}
		if s > best {
			best, lag = s, l
		}
	}
	t.Logf("the chain's output trails its input by %d samples", lag)
	require.InDelta(t, DelaySamples, lag, SampleRate/2000, "half a millisecond either way")
}
