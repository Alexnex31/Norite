// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"testing"

	"github.com/Alexnex31/Norite/voiceworker/internal/audio"
	"github.com/Alexnex31/Norite/voiceworker/internal/dsp"
	"github.com/Alexnex31/Norite/voiceworker/internal/jitter"
	"github.com/Alexnex31/Norite/voiceworker/internal/opus"
)

// The frame, the rate and the packet bound are written in three packages that do not import each other, so
// each stays usable alone; this package uses all three, and is where they are held equal (CLAUDE.md, M15).
// A frame of one size in the jitter buffer and another in the codec would stop FEC recovery matching, with
// every other test still passing.
func TestTheFrameIsOneSizeEverywhere(t *testing.T) {
	if audio.FrameSamples != opus.FrameSamples || jitter.FrameSamples != opus.FrameSamples {
		t.Errorf("a frame is %d samples to the devices, %d to the codec and %d to the jitter buffer",
			audio.FrameSamples, opus.FrameSamples, jitter.FrameSamples)
	}
	if audio.SampleRate != opus.SampleRate {
		t.Errorf("the devices run at %d Hz and the codec at %d", audio.SampleRate, opus.SampleRate)
	}
	// The DSP works in chunks of 10 ms, and process walks a frame in them (M30).
	if dsp.SampleRate != audio.SampleRate || audio.FrameSamples%dsp.ChunkSamples != 0 {
		t.Errorf("the DSP runs at %d Hz in chunks of %d samples, and a frame is %d samples at %d Hz",
			dsp.SampleRate, dsp.ChunkSamples, audio.FrameSamples, audio.SampleRate)
	}
	if jitter.MaxPacket != opus.MaxPacket {
		t.Errorf("the jitter buffer keeps packets of %d bytes and the codec takes %d", jitter.MaxPacket, opus.MaxPacket)
	}
}
