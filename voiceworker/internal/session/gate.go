// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

// The gate decides which frames the microphone sends (M35). Before it, every frame was encoded and DTX
// alone left out what Opus took for silence, and AEC3's residue is not that: a silent caller beside
// somebody speaking sent 87% of its frames at about −80 dBFS (M31's manual pass). The gate reads RNNoise's
// probability that a frame holds a voice, which the DSP returns whatever its settings, and in push-to-talk
// the daemon's key instead.
//
// A shut gate encodes nothing, as a server mute encodes nothing: the frame has been through the DSP, which
// stays converged, and the far end hears the gap in the timestamps as DTX. The frames before an opening are
// kept, so the start of a word RNNoise was a frame late on is sent ahead of it.
//
// Measured at M35's step 0, through the real module: speech's onset goes from 0.1 to 0.9 within a frame;
// a chain's first frames read up to 0.9 on dither while RNNoise warms up; and once AEC3 has converged, a
// silent microphone hearing the room's echo of speech never reaches 0.5.
const (
	// gateOpen is the probability at which the gate opens, the V flag's own, and gateClose the one below
	// which a frame does not keep it open: between the two, an open gate stays open and a shut one shut.
	gateOpen  = voiceProbability
	gateClose = 0.3
	// hangFrames is how long the gate stays open after the last frame that kept it open: 300 ms, so the
	// pauses inside a sentence are not cut.
	hangFrames = 15
	// leadFrames is how many frames before an opening are sent ahead of it: 40 ms, the most RNNoise was late
	// on an onset. The far end's jitter buffer starts that talk-spurt two frames deeper, and drains them in
	// the next silence (M31).
	leadFrames = 2
	// warmFrames is how many of a chain's first frames are RNNoise warming up, which neither open the gate
	// nor keep it open: up to 0.9 on dither, where it settles under 0.01 (step 0).
	warmFrames = 4
	// tailFrames is how long the gate stays open after the push-to-talk key's release: 200 ms, so a
	// word's end is not cut by a key let go on it.
	tailFrames = 10
)

// gateInput is what the gate decides one frame from.
type gateInput struct {
	// ptt is whether the input is push-to-talk, held whether the key is down now, and presses how many times
	// it has gone down in the call, so a press too short for a frame to see still opens the gate.
	ptt     bool
	held    bool
	presses uint64
	// voice is RNNoise's probability for the frame, and judged whether there is one to go by: with no chain,
	// not yet taken up or dropped for the rest of the call, nothing judges, and the gate is open, as every
	// frame was sent before M35. warming is a chain's first warmFrames.
	voice   float32
	judged  bool
	warming bool
}

// gate is the sender's, and decides each frame in order. Its zero value is shut.
type gate struct {
	open bool
	// quiet is how many frames have passed since the last that kept the gate open, while it is open.
	quiet int
	// tail is how many frames of push-to-talk are left after the key's release, and presses the count of
	// presses last seen.
	tail    int
	presses uint64
}

// step decides one frame: whether it is sent, and whether it opens the gate, which sends the frames held
// before it first.
func (g *gate) step(in gateInput) (send, opened bool) {
	was := g.open
	pressed := in.presses != g.presses
	g.presses = in.presses
	switch {
	case in.ptt:
		if in.held || pressed {
			g.tail = tailFrames
		}
		g.open = in.held || g.tail > 0
		if !in.held && g.tail > 0 {
			g.tail--
		}
		// What the voice gate takes over with, should the mode change while the key is down: a word under
		// way keeps its hang-over.
		g.quiet = 0
		// No lead-in: the key is down before the word.
		return g.open, false
	case !in.judged:
		g.open, g.quiet = true, 0
		return true, false
	case in.warming:
	case in.voice >= gateOpen:
		g.open, g.quiet = true, 0
		return true, !was
	case in.voice >= gateClose && g.open:
		g.quiet = 0
		return true, false
	}
	if !g.open {
		return false, false
	}
	if g.quiet++; g.quiet > hangFrames {
		g.open, g.quiet = false, 0
		return false, false
	}
	return true, false
}
