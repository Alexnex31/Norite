// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"math"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alexnex31/Norite/voiceworker/internal/audio"
	"github.com/Alexnex31/Norite/voiceworker/internal/audio/audiotest"
	"github.com/Alexnex31/Norite/voiceworker/internal/audio/audiotest/recordings"
	"github.com/Alexnex31/Norite/voiceworker/internal/congestion"
	"github.com/Alexnex31/Norite/voiceworker/internal/dsp"
)

// M31's done-when: a caller's uplink is cut to 64 kbps for twenty seconds and then lifted, between the
// worker and M26's SFU, while the caller reads aloud with the pauses of reading and a silence between
// readings. With the controller, the encoder's bitrate comes down to what the link carries, the queue on it
// stays short, and once it is lifted the bitrate is back at 64 kbps and the SFU receives what it did before.
// Without it, the same link holds a full queue. The far end hears the reading better with it than without.
//
// The link counts what IPv4 and UDP add, as a real one does: 64 kbps carries 39.2 kbps of Opus in 20 ms
// frames, the rest being the 62 bytes of headers, tag and IP each packet carries.
//
// Measured at M31 on loopback, with the controller: Opus at 35 to 37 kbps in the limit's last twelve
// seconds, 31 to 45 ms of queue and nothing dropped; without it, 279 ms of queue and 13% dropped. The spike
// before it measured pion's GCC at 190 ms.
func TestTheBitrateFollowsAConstrainedLink(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls through a shaped link")
	}
	var with, without linkRun
	t.Run("group", func(t *testing.T) {
		t.Run("with the controller", func(t *testing.T) { t.Parallel(); with = runShaped(t, false) })
		t.Run("without", func(t *testing.T) { t.Parallel(); without = runShaped(t, true) })
	})
	if t.Failed() {
		return
	}
	t.Logf("with: %+v", with)
	t.Logf("without: %+v", without)

	fits := float64((64000/8/50 - 62) * 8 * 50)
	if with.limitedRate > fits || with.limitedRate < congestion.Floor {
		t.Errorf("on the shaped link the encoder was set to %.0f bps at most, want what fits, %.0f, and no less "+
			"than the floor", with.limitedRate, fits)
	}
	if with.limitedQueue > 100*time.Millisecond || with.limitedDropped > 5 {
		t.Errorf("with the controller the shaped link queued %v on average and dropped %d", with.limitedQueue,
			with.limitedDropped)
	}
	if with.liftedRate != 64000 {
		t.Errorf("after the link was lifted the encoder was set to %.0f bps, not back at 64 kbps", with.liftedRate)
	}
	if with.liftedKbps < 0.9*with.cleanKbps {
		t.Errorf("after the link was lifted the SFU received %.1f kbps of audio, against %.1f before it was "+
			"shaped", with.liftedKbps, with.cleanKbps)
	}
	// What the test can see: the same link, the controller off, holds a queue.
	if without.limitedQueue < 150*time.Millisecond {
		t.Errorf("without the controller the shaped link queued only %v on average", without.limitedQueue)
	}
	// At the far end: what the shaped link added to the delay, and what the far end's jitter buffer had to
	// conceal or recover. The loudness alone does not tell the two runs apart: concealment and FEC carry a
	// 13% loss well enough to follow the reading at 0.75 to 0.93, late. Measured at M31: with the
	// controller, 180 to 200 ms added and 36 to 38 frames; without, 280 ms and 280 to 370 frames. What the controller leaves is the jitter
	// buffer's: the queue's first seconds deliver packets late, each deepens the buffer a frame, and it sheds
	// one every ten seconds (internal/jitter's shrinkAfter).
	if with.heard < 0.9 {
		t.Errorf("the far end's loudness followed the reading at %.3f on the shaped link", with.heard)
	}
	addedWith, addedWithout := with.shapedLag-with.cleanLag, without.shapedLag-without.cleanLag
	if addedWith+60*time.Millisecond > addedWithout || with.concealed*3 > without.concealed {
		t.Errorf("the far end heard the reading %v later and concealed %d frames with the controller, against "+
			"%v and %d without", addedWith, with.concealed, addedWithout, without.concealed)
	}
}

type linkRun struct {
	cleanKbps, liftedKbps   float64 // what the SFU received of the caller's audio, the median second
	limitedRate, liftedRate float64 // the encoder's bitrate, the highest in the limit's last 12 s, the last
	limitedQueue            time.Duration
	limitedDropped          int
	// At the far end: how well its loudness follows the reading while shaped, at which delay, against the
	// delay before the link was shaped, and what its jitter buffer concealed or recovered.
	heard               float64
	shapedLag, cleanLag time.Duration
	concealed           int
}

// runShaped is one call through a shaper: 6 s open, 20 s at 64 kbps, 16 s open again.
func runShaped(t *testing.T, fixed bool) linkRun {
	h := startHarness(t)
	reading := recordings.Near()
	frames := len(reading) / audio.FrameSamples
	cycle := frames + 3*50 // the reading, then three seconds of a quiet room
	loud := make([]float64, cycle)
	for k := range frames {
		loud[k] = frameRMS(reading[k*audio.FrameSamples : (k+1)*audio.FrameSamples])
	}
	var started atomic.Pointer[time.Time]
	var noise uint32 = 1
	speaker := &audiotest.Device{Source: func(i int, f []int16) {
		if i == 0 {
			now := time.Now()
			started.Store(&now)
		}
		if k := i % cycle; k < frames {
			copy(f, reading[k*audio.FrameSamples:(k+1)*audio.FrameSamples])
			return
		}
		for j := range f {
			noise = noise*1664525 + 1013904223
			f[j] = int16(noise>>30) - 1
		}
	}}
	listener := &audiotest.Device{}
	far := h.join(t, "1", "2", listener, Options{})
	var sh *shaper
	s := h.joinVia(t, "1", "1", speaker, dsp.AllOn(), Options{fixedBitrate: fixed}, through(t, &sh))
	start := time.Now()

	var r linkRun
	var clean, lifted []float64
	var queued time.Duration
	var queuedN int
	for sec := 1; sec <= 42; sec++ {
		time.Sleep(time.Until(start.Add(time.Duration(sec) * time.Second)))
		switch sec {
		case 6:
			sh.limit(64000)
		case 26:
			sh.limit(0)
		}
		d := sh.take(time.Second)
		b, _ := s.cc.Target()
		rate := float64(b)
		if fixed {
			rate = 64000
		}
		talking := d.packets >= 25 || d.dropped > 0
		switch {
		case sec <= 6 && sec > 1 && talking:
			clean = append(clean, d.kbps)
		case sec > 14 && sec <= 26:
			// The limit's last twelve seconds: the controller has had eight to come down.
			r.limitedRate = max(r.limitedRate, rate)
			r.limitedDropped += d.dropped
			if talking {
				queued += d.queue * time.Duration(d.packets)
				queuedN += d.packets
			}
		case sec > 34 && talking:
			lifted = append(lifted, d.kbps)
		}
		if sec == 42 {
			r.liftedRate = rate
		}
	}
	if queuedN > 0 {
		r.limitedQueue = queued / time.Duration(queuedN)
	}
	r.cleanKbps, r.liftedKbps = median(clean), median(lifted)
	heard, at := listener.Heard()
	r.heard, r.shapedLag = followsLoudness(heard, at, *started.Load(), start.Add(14*time.Second),
		start.Add(26*time.Second), loud)
	_, r.cleanLag = followsLoudness(heard, at, *started.Load(), start.Add(time.Second), start.Add(6*time.Second),
		loud)
	for _, sl := range mediaOf(far).slots {
		st := sl.buf.Stats()
		r.concealed += st.Concealed + st.Recovered
	}
	return r
}

func frameRMS(f []int16) float64 {
	var sum float64
	for _, v := range f {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(f)))
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	v = slices.Clone(v)
	slices.Sort(v)
	return v[len(v)/2]
}

// followsLoudness is how well what was heard between from and to follows the source's loudness frame by frame, at
// the delay it follows best: the speaker's frames are numbered from started, and loud is their loudness,
// one cycle of it. The delays tried reach 800 ms, past what a full 300 ms queue and the jitter buffer add.
func followsLoudness(heard []int16, at []time.Time, started, from, to time.Time, loud []float64) (float64,
	time.Duration) {
	best, bestLag := -1.0, 0
	for lag := range 40 {
		var got, want []float64
		for f := range at {
			k := int(at[f].Sub(started)/(20*time.Millisecond)) - lag
			if k < 0 || at[f].Before(from) || at[f].After(to) || (f+1)*audio.FrameSamples > len(heard) {
				continue
			}
			got = append(got, frameRMS(heard[f*audio.FrameSamples:(f+1)*audio.FrameSamples]))
			want = append(want, loud[k%len(loud)])
		}
		if c := correlation(got, want); c > best {
			best, bestLag = c, lag
		}
	}
	return best, time.Duration(bestLag) * 20 * time.Millisecond
}
