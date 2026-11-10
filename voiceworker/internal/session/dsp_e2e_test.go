// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/Alexnex31/Norite/voiceworker/internal/audio"
	"github.com/Alexnex31/Norite/voiceworker/internal/audio/audiotest"
	"github.com/Alexnex31/Norite/voiceworker/internal/audio/audiotest/recordings"
	"github.com/Alexnex31/Norite/voiceworker/internal/dsp"
)

// M30's done-when, end to end: two calls through M26's SFU, the second on a device whose microphone hears
// its own speakers. What the first caller says comes back to them through the second's room, unless the
// second's DSP cancels it. internal/dsp's tests hold the chain to the order and measure it offline; these
// hold the session to feeding it as they assumed.

// loop is a microphone that says pcm over and over, scaled by gain.
func loop(pcm []int16, gain float64) func(int, []int16) {
	return func(i int, f []int16) {
		for k := range f {
			f[k] = int16(gain * float64(pcm[(i*len(f)+k)%len(pcm)]))
		}
	}
}

// speaksAfter is a microphone that says nothing, its noise floor aside, until after has passed, and then
// says pcm at half its level.
func speaksAfter(after time.Duration, pcm []int16) func(int, []int16) {
	at := time.Now().Add(after)
	return func(i int, f []int16) {
		if time.Now().Before(at) {
			clear(f)
			return
		}
		loop(pcm, 0.5)(i, f)
	}
}

// echoRoom is the second caller's room: a 300 ms tail would cost the test device two transforms of 32,768
// points a frame, so the tail here is half that, which internal/dsp's tests do not need to spare.
func echoRoom() audiotest.Room { return audiotest.Room{Delay: 0.030, Gain: 0.3, RT60: 0.15, Seed: 1} }

// heardBack is how loud what the first caller heard is against what the second played them, in dB, over
// the 6 s that end skip seconds before what each has heard so far: the echo returned.
func heardBack(a *audiotest.Device, b *audiotest.Echoing, skip float64) float64 {
	ha, _ := a.Heard()
	hb, _ := b.Heard()
	return 10 * math.Log10(power(ha, 6+skip, skip)/power(hb, 6+skip, skip))
}

// power is the mean power of x from `from` seconds before its end to `to` seconds before it.
func power(x []int16, from, to float64) float64 {
	a, b := max(0, len(x)-int(from*audio.SampleRate)), max(0, len(x)-int(to*audio.SampleRate))
	var e float64
	for _, v := range x[a:b] {
		e += float64(v) * float64(v)
	}
	return e / float64(max(1, b-a))
}

// TestTheFarEndIsNotHeardBackThroughTheRoom is the two-party echo test. Measured: unprocessed, the first
// caller hears themselves 2 dB below what the second's speakers played; with echo cancellation alone, or
// with the whole chain, 62 to 64 dB below, which is the second microphone's noise floor. The second caller
// says nothing for 10 s, so what comes back then is echo and nothing else, and then speaks for 2 s, which
// the first must hear: canceled well, the echo is next to nothing, and so is a call that carries nothing.
func TestTheFarEndIsNotHeardBackThroughTheRoom(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	h := startHarness(t)
	voice := recordings.Far()
	for i, c := range []struct {
		name    string
		s       dsp.Settings
		atLeast float64 // dB the echo must sit below what was played, or above for "off"
	}{
		{"off", dsp.Settings{}, 0},
		{"echo cancellation", dsp.Settings{EchoCancellation: true}, 40},
		{"everything", dsp.AllOn(), 40},
	} {
		t.Run(c.name, func(t *testing.T) {
			room := string(rune('1' + i))
			a := &audiotest.Device{Source: loop(voice, 1)}
			b := &audiotest.Echoing{Room: echoRoom()}
			h.join(t, room, "1", a, Options{})
			b.Source = speaksAfter(10*time.Second, recordings.Near())
			h.joinWith(t, room, "2", b, c.s, Options{})
			time.Sleep(12 * time.Second)
			back := heardBack(a, b, 2.5)
			ha, _ := a.Heard()
			speech := 10 * math.Log10(power(ha, 1.5, 0)/(32768*32768))
			t.Logf("the first caller hears themselves %.1f dB below what the second played, and the second at "+
				"%.1f dBFS", -back, speech)
			if math.IsNaN(back) || math.IsInf(back, 0) {
				t.Fatalf("the echo measured %v: a side heard nothing at all", back)
			}
			if speech < -45 {
				t.Fatalf("the second caller is heard at %.1f dBFS: is their sender still sending?", speech)
			}
			if c.atLeast == 0 {
				// Unprocessed, the echo must be loud, or the cases below prove nothing.
				if back < -10 {
					t.Errorf("unprocessed, the echo is %.1f dB below what was played: the room is not there", -back)
				}
				return
			}
			if back > -c.atLeast {
				t.Errorf("the echo is %.1f dB below what was played, want %.0f", -back, c.atLeast)
			}
		})
	}
}

// TestEchoStaysCanceledWhenTheSenderFallsBehind starves the second caller's sender: every 25th frame it
// stops for 30 to 110 ms, so frames queue past captureQueue and the encoder skips some. Measured over the
// 6 s before the second caller speaks: the echo 57 to 60 dB below what was played. With M29's queue, which
// dropped the oldest frame before the DSP, it was 13 dB: AEC3 pairs reference and microphone by order,
// and every lost frame shifted the pairing. Echo cancellation alone, since RNNoise after it would remove a
// residue the test means to see.
//
// Canceled well, the echo can be nothing at all, which a sender that stopped sending would also give. So
// the second caller speaks for the last two seconds, and the first must hear them.
func TestEchoStaysCanceledWhenTheSenderFallsBehind(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	h := startHarness(t)
	var mu sync.Mutex
	n, stalls := 0, 0
	stall := func() {
		mu.Lock()
		n++
		k := n
		if k%25 == 0 {
			stalls++
		}
		mu.Unlock()
		if k%25 == 0 {
			time.Sleep(time.Duration(30+(k/25)%5*20) * time.Millisecond)
		}
	}
	a := &audiotest.Device{Source: loop(recordings.Far(), 1)}
	b := &audiotest.Echoing{Room: echoRoom()}
	h.join(t, "1", "1", a, Options{})
	b.Source = speaksAfter(10*time.Second, recordings.Near())
	sb := h.joinWith(t, "1", "2", b, dsp.Settings{EchoCancellation: true}, Options{stall: stall})
	time.Sleep(12 * time.Second)
	back := heardBack(a, b, 2.5)
	m := mediaOf(sb)
	ha, _ := a.Heard()
	speech := 10 * math.Log10(power(ha, 1.5, 0)/(32768*32768))
	t.Logf("starved: the echo %.1f dB below what was played, the second caller heard at %.1f dBFS; %d frames "+
		"processed and not sent, %d dropped", -back, speech, m.skipped.Load(), m.dropped.Load())
	mu.Lock()
	stalled := stalls
	mu.Unlock()
	if stalled < 10 {
		t.Fatalf("the sender stalled %d times, so this tested little", stalled)
	}
	if speech < -45 {
		t.Fatalf("the second caller is heard at %.1f dBFS: is their sender still sending?", speech)
	}
	// What bounds the latency of what is sent: a sender behind skips frames rather than send them late.
	if m.skipped.Load() == 0 {
		t.Error("the sender fell behind and skipped no frame")
	}
	if back > -40 {
		t.Errorf("starved, the echo is only %.1f dB below what was played", -back)
	}
}

// TestTheAudioLevelFollowsTheProcessedSignal reads RFC 6464's audio level and V flag where the SFU
// delivers them, on the far end's slot. The level is computed on the frame as sent, so a quiet speaker
// AGC2 raises reads louder; V follows RNNoise's voice probability, so speech sets it and rain does not.
func TestTheAudioLevelFollowsTheProcessedSignal(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	h := startHarness(t)
	type reading struct{ level, voiced float64 }
	// listen is the mean level and the voiced share of what the listener received in seconds 6 to 10 of a
	// speaker saying src.
	listen := func(room string, src func(int, []int16), s dsp.Settings) reading {
		h.joinWith(t, room, "1", &audiotest.Device{Source: src}, s, Options{})
		sl := h.join(t, room, "2", &audiotest.Device{}, Options{})
		slot := func() (n, voiced, loud int64) {
			m := mediaOf(sl)
			for _, x := range m.slots {
				n, voiced, loud = n+x.levels.Load(), voiced+x.voiced.Load(), loud+x.loudness.Load()
			}
			return
		}
		time.Sleep(6 * time.Second) // AGC2 adapts at a few dB a second
		n0, v0, l0 := slot()
		time.Sleep(4 * time.Second)
		n1, v1, l1 := slot()
		if n1-n0 < 50 {
			t.Fatalf("room %s: %d packets with a level arrived in 4 s", room, n1-n0)
		}
		return reading{float64(l1-l0) / float64(n1-n0), float64(v1-v0) / float64(n1-n0)}
	}
	quiet := loop(recordings.Near(), 0.05)
	raw := listen("1", quiet, dsp.Settings{})
	gained := listen("2", quiet, dsp.Settings{AutoGain: true})
	t.Logf("a quiet speaker: level %.1f -dBov unprocessed, %.1f with AGC2; voiced %.2f and %.2f",
		raw.level, gained.level, raw.voiced, gained.voiced)
	if gained.level > raw.level-6 {
		t.Errorf("with AGC2 the level is %.1f -dBov, want at least 6 dB louder than %.1f", gained.level, raw.level)
	}
	if gained.voiced < 0.7 {
		t.Errorf("only %.2f of a speaker's packets were sent as a voice", gained.voiced)
	}
	rain := listen("3", loop(recordings.Noise(), 0.4), dsp.AllOn())
	t.Logf("rain: voiced %.2f", rain.voiced)
	if rain.voiced > 0.1 {
		t.Errorf("%.2f of rain's packets were sent as a voice", rain.voiced)
	}
}
