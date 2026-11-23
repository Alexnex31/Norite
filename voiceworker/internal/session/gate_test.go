// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"

	"github.com/Alexnex31/Norite/daemon/voiceipc"
	"github.com/Alexnex31/Norite/voiceworker/internal/audio"
	"github.com/Alexnex31/Norite/voiceworker/internal/dsp"
)

// run steps the gate through each input and returns, frame by frame, whether it sent and whether it opened.
func run(g *gate, ins []gateInput) (sent, opened []bool) {
	for _, in := range ins {
		s, o := g.step(in)
		sent, opened = append(sent, s), append(opened, o)
	}
	return sent, opened
}

// voices are judged frames by voice, each probability for n frames, past the chain's warm-up.
func voices(spans ...any) []gateInput {
	var out []gateInput
	for i := 0; i < len(spans); i += 2 {
		for range spans[i+1].(int) {
			out = append(out, gateInput{voice: float32(spans[i].(float64)), judged: true})
		}
	}
	return out
}

func count(b []bool) int {
	n := 0
	for _, v := range b {
		if v {
			n++
		}
	}
	return n
}

// By voice, the gate opens on a frame RNNoise calls a voice, stays open on frames between the two
// thresholds, and shuts hangFrames after the last that kept it open: a sentence's pauses go, the silence
// after it does not.
func TestTheGateFollowsTheVoiceWithHysteresisAndAHangOver(t *testing.T) {
	var g gate
	sent, opened := run(&g, voices(0.1, 10, 0.9, 5, 0.4, 20, 0.1, 40))
	if count(sent[:10]) != 0 {
		t.Errorf("the gate sent %d frames before anybody spoke", count(sent[:10]))
	}
	if !opened[10] || count(opened) != 1 {
		t.Errorf("the gate opened at %v, want frame 10 alone", slices.Index(opened, true))
	}
	// Open from the voice through the frames between the thresholds, and hangFrames past them.
	if want := 5 + 20 + hangFrames; count(sent) != want || !sent[34+hangFrames] || sent[35+hangFrames] {
		t.Errorf("the gate sent %d frames, want %d, ending at frame %d", count(sent), want, 34+hangFrames)
	}
	// Between the thresholds a shut gate stays shut.
	g = gate{}
	if sent, _ := run(&g, voices(0.4, 30)); count(sent) != 0 {
		t.Errorf("frames at 0.4 opened a shut gate for %d frames", count(sent))
	}
}

// A chain's first frames are RNNoise warming up, up to 0.9 on dither (M35's step 0), and neither open the
// gate nor keep it open; with no chain to judge, every frame goes, as before M35.
func TestTheGateIgnoresRNNoiseWarmingUpAndOpensWithNothingToJudge(t *testing.T) {
	var g gate
	warm := make([]gateInput, warmFrames)
	for i := range warm {
		warm[i] = gateInput{voice: 0.9, judged: true, warming: true}
	}
	if sent, _ := run(&g, append(warm, voices(0.01, 30)...)); count(sent) != 0 {
		t.Errorf("a chain warming up opened the gate for %d frames", count(sent))
	}
	g = gate{}
	unjudged := make([]gateInput, 20)
	sent, opened := run(&g, append(unjudged, append(warm, voices(0.01, 30)...)...))
	if count(sent[:20]) != 20 || count(opened) != 0 {
		t.Errorf("with no chain, %d of 20 frames went, and %d opened the gate", count(sent[:20]), count(opened))
	}
	// Once a chain judges, the gate shuts after its warm-up and the hang-over, which the warm-up counts in.
	if want := 20 + hangFrames; count(sent) != want {
		t.Errorf("%d frames went, want %d", count(sent), want)
	}
}

// By push-to-talk, the key decides whatever RNNoise says: open while held and tailFrames after, shut
// otherwise, and a press released before any frame saw it still opens the gate for its tail.
func TestTheGateFollowsThePushToTalkKey(t *testing.T) {
	var g gate
	var ins []gateInput
	for i := range 80 {
		in := gateInput{ptt: true, voice: 0.95, judged: true, held: i >= 10 && i < 20, presses: 0}
		if i >= 10 {
			in.presses = 1
		}
		if i >= 50 {
			in.presses = 2 // pressed and released between frames 49 and 50
		}
		ins = append(ins, in)
	}
	sent, opened := run(&g, ins)
	if count(opened) != 0 {
		t.Error("push-to-talk reported an opening, which would send a lead-in")
	}
	if count(sent[:10]) != 0 {
		t.Errorf("%d frames went before the key", count(sent[:10]))
	}
	if count(sent[10:20+tailFrames]) != 10+tailFrames || sent[20+tailFrames] {
		t.Errorf("with the key held for 10 frames, frames 10 to %d went: %v", 20+tailFrames, sent[10:40])
	}
	if count(sent[50:50+tailFrames]) != tailFrames || count(sent[50+tailFrames:]) != 0 {
		t.Errorf("a press no frame saw sent %d frames, want %d", count(sent[50:]), tailFrames)
	}
}

// Changing to voice while the key is down does not cut the word under way: the voice gate takes over open,
// with its hang-over.
func TestAModeChangeMidWordKeepsTheGateOpen(t *testing.T) {
	var g gate
	run(&g, []gateInput{{ptt: true, held: true, presses: 1}, {ptt: true, held: true, presses: 1}})
	sent, _ := run(&g, voices(0.01, 30))
	if count(sent) != hangFrames {
		t.Errorf("after the change, %d frames went, want the hang-over's %d", count(sent), hangFrames)
	}
}

// scriptedChain is a chain whose voice probability for each frame the test sets, as RNNoise's would be, and
// which leaves the audio as it was. It counts its frames, so a test can pace them.
type scriptedChain struct {
	dsp.Chain
	voice  func(frame int) float32
	chunks atomic.Int64
}

func (*scriptedChain) Render([]int16) error { return nil }
func (*scriptedChain) Close() error         { return nil }
func (c *scriptedChain) Capture([]int16) (float32, error) {
	n := c.chunks.Add(1) - 1
	return c.voice(int(n / 2)), nil
}

// tagging encodes each frame as three bytes carrying its first sample, which the test sets to the frame's
// number plus one, so what was sent says which frames it was.
type tagging struct{ tunable }

func (tagging) Encode(pcm []int16, out []byte) (int, error) {
	return copy(out, []byte{byte(pcm[0] >> 8), byte(pcm[0]), 1}), nil
}

// packets keeps every packet written to it.
type packets struct {
	mu   sync.Mutex
	sent []rtp.Packet
}

func (p *packets) WriteRTP(pkt *rtp.Packet) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := *pkt
	c.Payload = slices.Clone(pkt.Payload)
	p.sent = append(p.sent, c)
	return nil
}

// frames returns which frames were sent, by their tags, and which of those started a talk-spurt.
func (p *packets) frames() (tags []int, markers []int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pkt := range p.sent {
		tag := int(pkt.Payload[0])<<8 | int(pkt.Payload[1])
		tags = append(tags, tag-1)
		if pkt.Marker {
			markers = append(markers, tag-1)
		}
		if pkt.Timestamp != uint32((tag-1)*audio.FrameSamples) {
			panic("a frame sent with another frame's timestamp")
		}
	}
	return tags, markers
}

// gated is a sender with the scriptedChain chain, fed n frames one at a time, each the moment the last is
// through the chain, with before(i) called ahead of frame i.
func gated(t *testing.T, s *Session, n int, voice func(int) float32, before func(i int)) *packets {
	t.Helper()
	if s.opts.Log == nil {
		s.opts.Log = io.Discard
	}
	s.cc, s.ended = testController(), make(chan string, 1)
	chain := &scriptedChain{voice: voice}
	track := &packets{}
	m := &media{s: s, stop: make(chan struct{}), frames: make(chan captureFrame, captureBacklog), base: time.Now(),
		chain: chain, enc: tagging{}, codec: keepOpen{}, track: track}
	m.sent.Add(1)
	go m.send()
	defer func() {
		close(m.stop)
		m.sent.Wait()
	}()
	for i := range n {
		if before != nil {
			before(i)
		}
		pcm := make([]int16, audio.FrameSamples)
		pcm[0] = int16(i + 1)
		m.captured(pcm)
		deadline := time.Now().Add(time.Second)
		for chain.chunks.Load() < int64(2*(i+1)) {
			if time.Now().After(deadline) {
				t.Fatalf("frame %d was not processed within a second", i)
			}
			time.Sleep(100 * time.Microsecond)
		}
	}
	// The last frame's encode follows its processing.
	time.Sleep(20 * time.Millisecond)
	return track
}

func span(from, to int) []int {
	var out []int
	for i := from; i < to; i++ {
		out = append(out, i)
	}
	return out
}

// The two frames before an opening go first, oldest first, in their own timestamps, the first of them
// starting the talk-spurt: the start of a word RNNoise is a frame late on is heard (M35's step 0). After
// the voice, the hang-over, and then nothing: a silent caller sends nothing at all.
func TestAnOpeningSendsTheFramesBeforeIt(t *testing.T) {
	voice := func(i int) float32 {
		if i >= 10 && i < 30 {
			return 0.9
		}
		return 0.01
	}
	s := &Session{}
	tags, markers := gated(t, s, 80, voice, nil).frames()
	if want := span(10-leadFrames, 30+hangFrames); !slices.Equal(tags, want) {
		t.Errorf("sent frames %v, want %v", tags, want)
	}
	if !slices.Equal(markers, []int{10 - leadFrames}) {
		t.Errorf("talk-spurts started at %v, want at %d alone", markers, 10-leadFrames)
	}
}

// Nothing captured while sending is not allowed is kept for a lead-in: a mute lifted mid-word would
// otherwise send what was said under it.
func TestALeadInSendsNothingCapturedUnderAMute(t *testing.T) {
	voice := func(i int) float32 {
		if i >= 10 && i < 20 {
			return 0.9
		}
		return 0.01
	}
	s := &Session{}
	s.Rights(voiceipc.Rights{Send: true, Hear: true})
	tags, _ := gated(t, s, 50, voice, func(i int) {
		s.mayNotSend.Store(i >= 4 && i < 10)
	}).frames()
	if want := span(10, 20+hangFrames); !slices.Equal(tags, want) {
		t.Errorf("sent frames %v, want %v", tags, want)
	}
}

// By push-to-talk, the key is what sends, whatever RNNoise says, and a press never lifts a mute.
func TestPushToTalkSendsWhileTheKeyIsDown(t *testing.T) {
	s := &Session{}
	s.Input(voiceipc.Input{Mode: voiceipc.InputPushToTalk})
	tags, markers := gated(t, s, 90, func(int) float32 { return 0.9 }, func(i int) {
		switch i {
		case 10:
			s.Input(voiceipc.Input{Mode: voiceipc.InputPushToTalk, Held: true})
		case 20:
			s.Input(voiceipc.Input{Mode: voiceipc.InputPushToTalk})
		case 50:
			s.mayNotSend.Store(true)
			s.Input(voiceipc.Input{Mode: voiceipc.InputPushToTalk, Held: true})
		case 60:
			s.Input(voiceipc.Input{Mode: voiceipc.InputPushToTalk})
		}
	}).frames()
	if want := span(10, 20+tailFrames); !slices.Equal(tags, want) {
		t.Errorf("sent frames %v, want %v", tags, want)
	}
	if !slices.Equal(markers, []int{10}) {
		t.Errorf("talk-spurts started at %v, want at 10 alone", markers)
	}
}

// A press and its release between two frames still send the tail: the session counts the press as it
// arrives, and the sender's next frame sees the count go up, whatever the key is by then.
func TestAPressBetweenTwoFramesStillSends(t *testing.T) {
	s := &Session{}
	s.Input(voiceipc.Input{Mode: voiceipc.InputPushToTalk})
	tags, _ := gated(t, s, 40, func(int) float32 { return 0.9 }, func(i int) {
		if i == 10 {
			s.Input(voiceipc.Input{Mode: voiceipc.InputPushToTalk, Held: true})
			s.Input(voiceipc.Input{Mode: voiceipc.InputPushToTalk})
		}
	}).frames()
	if want := span(10, 10+tailFrames); !slices.Equal(tags, want) {
		t.Errorf("sent frames %v, want %v", tags, want)
	}
}
