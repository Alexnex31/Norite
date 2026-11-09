// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package dsp

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/voiceworker/internal/audio/audiotest"
	"github.com/Alexnex31/Norite/voiceworker/internal/audio/audiotest/recordings"
)

// What the chain does to real audio, in the roadmap's terms: echo canceled, on the linear signal before
// RNNoise touches it, and noise suppressed while speech is kept. The chain is deterministic, so each
// number is the same on every run and every host; the bounds sit below what the first run measured, as
// M29 set its latency budget, and are there to catch the module changing.

// process runs mic through a chain whose speakers play far, the reference first as the session does, and
// returns the output moved back by the chain's delay for its settings, so that it lines up with its input,
// and each chunk's voice probability.
func process(t *testing.T, c Chain, far, mic []int16, delay int) ([]float64, []float32) {
	t.Helper()
	out := make([]float64, 0, len(mic))
	var voice []float32
	pcm, quiet := make([]int16, ChunkSamples), make([]int16, ChunkSamples)
	for i := 0; i+ChunkSamples <= len(mic); i += ChunkSamples {
		ref := quiet
		if far != nil {
			ref = far[i : i+ChunkSamples]
		}
		require.NoError(t, c.Render(ref))
		copy(pcm, mic[i:])
		p, err := c.Capture(pcm)
		require.NoError(t, err)
		voice = append(voice, p)
		for _, v := range pcm {
			out = append(out, float64(v))
		}
	}
	return out[delay:], voice
}

// delayOf is the delay a chain with s adds, as the components measured alone: RNNoise's two chunks and the
// AEC object's 430 samples.
func delayOf(s Settings) int {
	d := 0
	if s.NoiseSuppression {
		d += 2 * ChunkSamples
	}
	if s.EchoCancellation {
		d += 430
	}
	return d
}

func sample(v float64) int16 { return int16(max(math.MinInt16, min(math.MaxInt16, math.Round(v)))) }

func energy(x []float64, from, to int) float64 {
	var e float64
	for _, v := range x[from:min(to, len(x))] {
		e += v * v
	}
	return e
}

func db(r float64) float64 { return 10 * math.Log10(r) }

// TestEchoIsCanceledBeforeRNNoise is the roadmap's two clauses about echo: AEC cancels the far end, and it
// runs on the linear signal, before RNNoise's non-linear processing. Swapping the order must make the
// result measurably worse, or the test could not tell the orders apart.
//
// The far end, a woman reading, plays through a room with a 300 ms tail and a clock 50 parts per million
// off, and the near end, a man reading, joins at 6 s. Measured, in three rooms: in the far end's stretch
// after 3 s, the chain in order removes 42 to 45 dB of the echo, and with RNNoise ahead of AEC 21 to 29 dB,
// no better than AEC alone (23 to 26 dB). Echo canceled after RNNoise has run is canceled on a signal no
// linear filter models, and RNNoise after AEC removes the residue AEC leaves.
//
// The plan expected the far end alone not to tell the orders apart, and the double-talk stretch to. It was
// the other way round: in double-talk, echo is 33 to 45 dB below the near end in either order, and which
// order is better changes with the room. So double-talk is held only to being well canceled.
func TestEchoIsCanceledBeforeRNNoise(t *testing.T) {
	far, near, noise := recordings.Far(), recordings.Near(), recordings.Noise()
	s := Settings{EchoCancellation: true, NoiseSuppression: true}
	m := newModule(t)
	for _, seed := range []uint64{1, 2, 3} {
		room := audiotest.LivingRoom(seed)
		echo := room.Echo(far)
		mic := make([]int16, len(far))
		nearPart := make([]float64, len(far))
		for i := range mic {
			if i >= 6*SampleRate {
				nearPart[i] = 0.5 * float64(near[i])
			}
			mic[i] = sample(echo[i] + nearPart[i] + 0.03*float64(noise[i]))
		}
		micF := make([]float64, len(mic))
		for i, v := range mic {
			micF[i] = float64(v)
		}
		removed := func(order [3]step) (farOnly float64, out []float64) {
			c := newChain(t, m, s)
			c.(*wasmChain).order = order
			out, _ = process(t, c, far, mic, delayOf(s))
			from, to := 3*SampleRate, 6*SampleRate-delayOf(s)
			return db(energy(micF, from, to) / energy(out, from, to)), out
		}
		inOrder, out := removed(order)
		swapped, _ := removed([3]step{stepNS, stepAEC, stepAGC})
		t.Logf("room %d: far end alone, %.1f dB of echo removed in order and %.1f swapped", seed, inOrder, swapped)
		require.GreaterOrEqual(t, inOrder, 35.0, "room %d: the chain in order cancels too little echo", seed)
		require.GreaterOrEqual(t, inOrder-swapped, 10.0,
			"room %d: with RNNoise ahead of AEC the echo should be at least 10 dB worse canceled", seed)

		// Double-talk: the output's echo and near-end components, by least squares on the two known parts.
		var ee, nn, en, ye, yn float64
		for i := 7 * SampleRate; i < len(out); i++ {
			e, n, y := echo[i], nearPart[i], out[i]
			ee, nn, en, ye, yn = ee+e*e, nn+n*n, en+e*n, ye+y*e, yn+y*n
		}
		det := ee*nn - en*en
		ge, gn := (ye*nn-yn*en)/det, (yn*ee-ye*en)/det
		ratio := db(ge * ge * ee / (gn * gn * nn))
		t.Logf("room %d: in double-talk the echo left is %.1f dB below the near end, which keeps %.2f of its level",
			seed, ratio, gn)
		require.LessOrEqual(t, ratio, -25.0, "room %d: too much echo is left under double-talk", seed)
		require.GreaterOrEqual(t, gn, 0.2, "room %d: the near end is suppressed under double-talk", seed)
	}
}

// TestNoiseIsSuppressedAndSpeechKept is the roadmap's "measurable suppression" for noise: a man reading
// over traffic in the rain, about 9 dB above it, with two 2 s pauses. Measured: RNNoise takes the noise
// down 51.5 dB in the pauses once a quarter of a second has passed, keeps loud speech within 1.6 dB of its
// level, and brings the output's loudness, chunk by chunk, from a correlation of 0.79 with the clean
// speech's to 0.89. The voice probability passes 0.5 in 98% of loud speech, and in none of 1,100 chunks of
// traffic and rain alone.
func TestNoiseIsSuppressedAndSpeechKept(t *testing.T) {
	near, noise := recordings.Near(), recordings.Noise()
	speaking := func(i int) bool { s := i / SampleRate; return (s < 3 || s >= 5) && (s < 8 || s >= 10) }
	clean := make([]float64, len(near))
	mic := make([]int16, len(near))
	micF := make([]float64, len(near))
	for i := range mic {
		if speaking(i) {
			clean[i] = 0.5 * float64(near[i])
		}
		mic[i] = sample(clean[i] + 0.2*float64(noise[i]))
		micF[i] = float64(mic[i])
	}
	m := newModule(t)
	type outcome struct {
		floor, kept, envelope, voiced float64
	}
	measure := func(s Settings) outcome {
		out, voice := process(t, newChain(t, m, s), nil, mic, delayOf(s))
		var o outcome
		var floorIn, floorOut, kept float64
		var loud, voiced int
		var x, y []float64
		for c := 100; (c+1)*ChunkSamples <= len(out); c++ { // past the first second
			a, b := c*ChunkSamples, (c+1)*ChunkSamples
			ce, ie, oe := energy(clean, a, b), energy(micF, a, b), energy(out, a, b)
			// A pause, a quarter of a second in, so that what RNNoise heard last is noise.
			if !speaking(a-SampleRate/4) && !speaking(b) {
				floorIn, floorOut = floorIn+ie, floorOut+oe
			}
			cdb, odb := db(ce/ChunkSamples+1), db(oe/ChunkSamples+1)
			if cdb > 55 {
				kept += odb - db(ie/ChunkSamples+1)
				loud++
				if voice[c+delayOf(s)/ChunkSamples] >= 0.5 {
					voiced++
				}
			}
			x, y = append(x, cdb), append(y, odb)
		}
		o.floor, o.kept, o.envelope = db(floorIn/floorOut), kept/float64(loud), correlation(x, y)
		o.voiced = float64(voiced) / float64(loud)
		return o
	}
	on, off := measure(Settings{NoiseSuppression: true}), measure(Settings{})
	t.Logf("suppression on: noise down %.1f dB, loud speech %+.2f dB, envelope %.3f, voiced %.2f",
		on.floor, on.kept, on.envelope, on.voiced)
	t.Logf("suppression off: envelope %.3f", off.envelope)
	require.GreaterOrEqual(t, on.floor, 40.0, "the noise floor in the pauses")
	require.GreaterOrEqual(t, on.kept, -3.0, "loud speech is cut")
	require.GreaterOrEqual(t, on.envelope, 0.85)
	require.GreaterOrEqual(t, on.envelope-off.envelope, 0.05, "suppression should bring the output closer to the speech")
	require.GreaterOrEqual(t, on.voiced, 0.9, "the voice probability misses speech")

	// Traffic and rain alone, a little louder than under the speech.
	c := newChain(t, m, AllOn())
	alone := make([]int16, len(noise))
	for i, v := range noise {
		alone[i] = sample(0.4 * float64(v))
	}
	_, voice := process(t, c, nil, alone, 0)
	var falseVoice int
	for _, p := range voice[100:] {
		if p >= 0.5 {
			falseVoice++
		}
	}
	t.Logf("noise alone: voice in %d of %d chunks", falseVoice, len(voice)-100)
	require.LessOrEqual(t, float64(falseVoice)/float64(len(voice)-100), 0.02, "noise alone reads as a voice")
}

func correlation(x, y []float64) float64 {
	var sx, sy, sxx, syy, sxy float64
	for i := range x {
		sx, sy = sx+x[i], sy+y[i]
		sxx, syy, sxy = sxx+x[i]*x[i], syy+y[i]*y[i], sxy+x[i]*y[i]
	}
	n := float64(len(x))
	return (n*sxy - sx*sy) / math.Sqrt((n*sxx-sx*sx)*(n*syy-sy*sy))
}
