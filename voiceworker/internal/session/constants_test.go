// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"testing"

	"github.com/Alexnex31/Norite/voiceworker/internal/audio"
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
	if jitter.MaxPacket != opus.MaxPacket {
		t.Errorf("the jitter buffer keeps packets of %d bytes and the codec takes %d", jitter.MaxPacket, opus.MaxPacket)
	}
}
