// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"testing"
	"time"

	"github.com/Alexnex31/Norite/voiceworker/internal/audio"
	"github.com/Alexnex31/Norite/voiceworker/internal/congestion"
	"github.com/Alexnex31/Norite/voiceworker/internal/dsp"
	"github.com/Alexnex31/Norite/voiceworker/internal/jitter"
	"github.com/Alexnex31/Norite/voiceworker/internal/opus"
)

// The frame, the rate and the packet bound are written in packages that do not import each other, so each
// stays usable alone: the devices, the codec, the jitter buffer, the DSP and, since M31, the controller.
// This package uses them all, and is where they are held equal (CLAUDE.md, M15).
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
	// The controller credits its climb per packet, a frame each (M31).
	if congestion.Frame != time.Duration(opus.FrameSamples)*time.Second/time.Duration(opus.SampleRate) {
		t.Errorf("the controller credits %v a packet, and a frame is %d samples at %d Hz", congestion.Frame,
			opus.FrameSamples, opus.SampleRate)
	}
}

// The encoder starts with its FEC sized for the loss M29 chose, and the controller never sizes it for less:
// the two must be one number, or the call's first frame moves it (M31's /code-review found it moved to 0%,
// where libopus turns FEC off). The ceiling is the encoder's starting bitrate for the same reason.
func TestTheEncoderStartsWhereTheControllerDoes(t *testing.T) {
	o := opus.DefaultEncoderOptions()
	if o.LossPercent != congestion.MinLossPercent {
		t.Errorf("the encoder starts at %d%% loss and the controller's floor is %d%%", o.LossPercent,
			congestion.MinLossPercent)
	}
	if o.LossPercent < 1 {
		t.Errorf("the encoder starts at %d%% loss, which turns libopus's FEC off", o.LossPercent)
	}
	if b, l := newController(time.Now()).Target(); b != o.Bitrate || l != o.LossPercent {
		t.Errorf("the call's controller starts at %d bps and %d%%, the encoder at %d bps and %d%%", b, l,
			o.Bitrate, o.LossPercent)
	}
}
