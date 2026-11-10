// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/voiceipc"
	"github.com/Alexnex31/Norite/voiceworker/internal/audio"
	"github.com/Alexnex31/Norite/voiceworker/internal/audio/audiotest"
	"github.com/Alexnex31/Norite/voiceworker/internal/dsp"
	"github.com/Alexnex31/Norite/voiceworker/internal/opus"
)

// M29's done-when, in process: sessions with the test device's microphone and speakers, connected through
// M26's SFU and signaled through its harness, built from this checkout as the daemon's end-to-end test
// builds it. What one says, the other hears, and the test reads it off the far end's speakers.

type harness struct {
	url string
	mu  sync.Mutex
	log strings.Builder
}

func freePort(t *testing.T, network string) int {
	t.Helper()
	if network == "udp" {
		c, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		return c.LocalAddr().(*net.UDPAddr).Port
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func startHarness(t *testing.T) *harness {
	t.Helper()
	// The DSP's module is compiled once for the package, and a call does not wait for it, so whichever test
	// ran first compiled it while its callers spoke: 2.2 s under -race on two idle cores, and on a loaded CI
	// runner long enough to put the senders behind, skipping frames, inside the window that test measures.
	// It is compiled here, before any call, so no test pays it.
	if _, err := sharedDSP(context.Background()); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "voice-sfu")
	build := exec.Command("go", "build", "-o", bin, "./internal/voice/voicetest/cmd/voice-sfu") //nolint:gosec // fixed
	build.Dir = filepath.Join("..", "..", "..", "backend")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the harness: %v\n%s", err, out)
	}
	httpAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t, "tcp"))
	cmd := exec.Command(bin, "-loopback", //nolint:gosec // the harness just built
		"-media", fmt.Sprintf("127.0.0.1:%d", freePort(t, "udp")),
		"-turn", fmt.Sprintf("127.0.0.1:%d", freePort(t, "tcp")),
		"-http", httpAddr)
	out, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &harness{url: "http://" + httpAddr}
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			h.mu.Lock()
			h.log.WriteString(sc.Text() + "\n")
			h.mu.Unlock()
		}
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(30 * time.Second)
	for {
		var creds struct{}
		if h.post("/credentials", map[string]string{"user": "1"}, &creds) == nil {
			return h
		}
		if time.Now().After(deadline) {
			t.Fatal("the harness did not come up")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (h *harness) post(path string, in, out any) error {
	body, _ := json.Marshal(in)
	res, err := http.Post(h.url+path, "application/json", bytes.NewReader(body)) //nolint:noctx // a test's harness
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		return fmt.Errorf("%s: %d %s", path, res.StatusCode, b)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// shared compiles the module once for the package's calls; each session still has its own instances.
var shared = struct {
	once  sync.Once
	codec opus.Codec
	err   error
}{}

func sharedCodec(context.Context) (opus.Codec, error) {
	shared.once.Do(func() { shared.codec, shared.err = opus.New(context.Background()) })
	return keepOpen{shared.codec}, shared.err
}

// keepOpen is the shared codec, which one session's end must not close under the others.
type keepOpen struct{ opus.Codec }

func (keepOpen) Close(context.Context) error { return nil }

// sharedMod is the DSP's module, compiled once for the package's calls as the codec is.
var sharedMod = struct {
	once sync.Once
	mod  dsp.Module
	err  error
}{}

func sharedDSP(context.Context) (dsp.Module, error) {
	sharedMod.once.Do(func() { sharedMod.mod, sharedMod.err = dsp.New(context.Background()) })
	return keepModule{sharedMod.mod}, sharedMod.err
}

type keepModule struct{ dsp.Module }

func (keepModule) Close(context.Context) error { return nil }

// join connects a session to room as user, with dev for its microphone and speakers, and its DSP off, which leaves its samples as M29's were: the tests below that feed a tone hold
// the codec's path, and RNNoise takes a steady tone for noise. RNNoise still runs, for the V flag. The
// tests in dsp_e2e_test.go and the latency budget turn the DSP on, through joinWith.
func (h *harness) join(t *testing.T, room, user string, dev audio.Device, opts Options) *Session {
	t.Helper()
	return h.joinWith(t, room, user, dev, dsp.Settings{}, opts)
}

// joinWith is join with the DSP's components on as p says, carried in the join as the daemon carries them.
func (h *harness) joinWith(t *testing.T, room, user string, dev audio.Device, p dsp.Settings, opts Options) *Session {
	t.Helper()
	var creds struct {
		Username string   `json:"username"`
		Password string   `json:"password"`
		URLs     []string `json:"urls"`
	}
	if err := h.post("/credentials", map[string]string{"user": user}, &creds); err != nil {
		t.Fatal(err)
	}
	j := voiceipc.Join{Loopback: true, Processing: ipc.VoiceJoin(p)}
	if len(creds.URLs) > 0 {
		j.ICEServers = []voiceipc.ICEServer{{URLs: creds.URLs, Username: creds.Username, Credential: creds.Password}}
	}
	opts.Version, opts.Device = "dev", dev
	if opts.Codec == nil {
		opts.Codec = sharedCodec
	}
	if opts.DSP == nil {
		opts.DSP = sharedDSP
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	s, err := New(j, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	offer, err := s.Offer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ans struct {
		Answer string `json:"answer"`
	}
	if err := h.post("/provision", map[string]string{"room": room, "user": user, "offer": offer}, &ans); err != nil {
		t.Fatal(err)
	}
	if err := s.Accept(ans.Answer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Ready():
	case reason := <-s.Ended():
		t.Fatalf("%s's call ended: %s", user, reason)
	case <-ctx.Done():
		t.Fatalf("%s's call was not ready", user)
	}
	return s
}

// tonePower is the share of pcm's energy in audiotest.Tone at hz: near 1 for the tone, near 0 for anything
// else. The tone's loudness rises and falls three times a second, which puts a quarter of its energy in
// sidebands 3 Hz either side, so those count too.
func tonePower(pcm []int16, hz float64) float64 {
	var total, at float64
	for _, v := range pcm {
		total += float64(v) * float64(v)
	}
	for _, f := range []float64{hz - 3, hz, hz + 3} {
		var re, im float64
		for i, v := range pcm {
			ph := 2 * math.Pi * f * float64(i) / audio.SampleRate
			re += float64(v) * math.Cos(ph)
			im += float64(v) * math.Sin(ph)
		}
		at += 2 * (re*re + im*im) / float64(len(pcm))
	}
	if total == 0 {
		return 0
	}
	return at / total
}

// envelopeMatch is how well what was heard follows audiotest.Envelope: the best correlation of the two,
// in 10 ms blocks, over the delays a call can add, and the delay it was best at.
func envelopeMatch(heard []int16) (float64, time.Duration) {
	const block = audio.SampleRate / 100
	var got []float64
	for i := 0; i+block <= len(heard); i += block {
		var sum float64
		for _, v := range heard[i : i+block] {
			sum += float64(v) * float64(v)
		}
		got = append(got, math.Sqrt(sum/block))
	}
	best, at := -1.0, 0
	for lag := 0; lag < 100 && lag < len(got)/2; lag++ {
		var want []float64
		for i := range got {
			want = append(want, audiotest.Envelope((i-lag)*block+block/2))
		}
		if c := correlation(got, want); c > best {
			best, at = c, lag
		}
	}
	return best, time.Duration(at) * 10 * time.Millisecond
}

func correlation(a, b []float64) float64 {
	var ma, mb float64
	for i := range a {
		ma, mb = ma+a[i], mb+b[i]
	}
	ma, mb = ma/float64(len(a)), mb/float64(len(b))
	var ab, aa, bb float64
	for i := range a {
		x, y := a[i]-ma, b[i]-mb
		ab, aa, bb = ab+x*y, aa+x*x, bb+y*y
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return ab / math.Sqrt(aa*bb)
}

// steady is what the speakers played from the first frame with sound in it plus a settling second.
func steady(t *testing.T, dev *audiotest.Device, want time.Duration) []int16 {
	t.Helper()
	deadline := time.Now().Add(want + 10*time.Second)
	for {
		heard, _ := dev.Heard()
		first := -1
		for i, v := range heard {
			if v > 500 || v < -500 {
				first = i
				break
			}
		}
		if first >= 0 {
			from := first + audio.SampleRate
			if len(heard) >= from+int(want.Seconds()*audio.SampleRate) {
				return heard[from : from+int(want.Seconds()*audio.SampleRate)]
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the speakers played %d samples, sound from %d", len(heard), first)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func checkHeard(t *testing.T, who string, heard []int16, hz, minMatch float64) {
	t.Helper()
	if p := tonePower(heard, hz); p < 0.9 {
		t.Errorf("%s: %.2f of what was heard is the %v Hz tone", who, p, hz)
	}
	m, delay := envelopeMatch(heard)
	if m < minMatch {
		t.Errorf("%s: what was heard follows the speaker's loudness at %.3f (best at a delay of %v), want %.2f",
			who, m, delay, minMatch)
	}
	t.Logf("%s: tone %.3f, envelope %.3f", who, tonePower(heard, hz), m)
}

func TestTwoCallsHearEachOther(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	h := startHarness(t)
	a := &audiotest.Device{Source: audiotest.Tone(440, 8000)}
	b := &audiotest.Device{Source: audiotest.Tone(660, 8000)}
	h.join(t, "1", "1", a, Options{})
	sb := h.join(t, "1", "2", b, Options{})
	checkHeard(t, "the second caller", steady(t, b, 2*time.Second), 440, 0.9)
	checkHeard(t, "the first caller", steady(t, a, 2*time.Second), 660, 0.9)
	// Every packet the first caller sent carried its audio level, which the SFU re-encodes on forward and
	// reads for who is speaking.
	var levels int64
	for _, sl := range mediaOf(sb).slots {
		levels += sl.levels.Load()
	}
	if levels < 100 {
		t.Errorf("%d packets arrived with an audio level", levels)
	}
}

func mediaOf(s *Session) *media {
	s.mediaMu.Lock()
	defer s.mediaMu.Unlock()
	return s.media
}

// gate is a Source that plays a tone only between two frame numbers.
func gate(from, to int, src func(int, []int16)) func(int, []int16) {
	return func(i int, f []int16) {
		if i < from || i >= to {
			clear(f)
			return
		}
		src(i, f)
	}
}

// Four people speak into three slots. When one stops and the fourth starts, the SFU gives the fourth a
// slot one of the others had: the listener's decoder for that slot is reset before it decodes the
// newcomer, who is heard. Under WebAssembly the reset is a new instance (internal/opus).
func TestASlotThatChangesSenderResetsItsDecoder(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	h := startHarness(t)
	listener := &audiotest.Device{}
	sl := h.join(t, "1", "9", listener, Options{})
	for i, hz := range []float64{300, 500, 700} {
		end := 1 << 30
		if i == 2 {
			end = 150 // three seconds, then silence
		}
		h.join(t, "1", fmt.Sprint(i+1), &audiotest.Device{Source: gate(0, end, audiotest.Tone(hz, 6000))}, Options{})
	}
	h.join(t, "1", "4", &audiotest.Device{Source: gate(200, 1<<30, audiotest.Tone(1100, 6000))}, Options{})
	time.Sleep(8 * time.Second)
	m := mediaOf(sl)
	m.mu.Lock()
	var resets, senders int
	for _, s := range m.slots {
		resets += s.resets
		senders += s.buf.Stats().Senders
	}
	m.mu.Unlock()
	// Three slots' first senders need no reset; the fourth sender, taking a slot over, does.
	if senders < 4 || resets < 1 || resets != senders-len(m.slots) {
		t.Errorf("the slots carried %d senders and reset %d times, want 4 and 1", senders, resets)
	}
	heard, _ := listener.Heard()
	if p := tonePower(heard[len(heard)-2*audio.SampleRate:], 1100); p < 0.15 {
		t.Errorf("the newcomer is %.2f of the last two seconds", p)
	}
}

// The same through TURN, both callers relayed, and with 5% of the first caller's packets lost on the way:
// FEC and concealment carry it.
func TestACallRelayedAndLossyIsStillHeard(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	h := startHarness(t)
	var mu sync.Mutex
	n := 0
	lossy := func() bool {
		mu.Lock()
		defer mu.Unlock()
		n++
		return n%20 == 7 // 5%, spread out: each loss has a next packet to recover it from
	}
	a := &audiotest.Device{Source: audiotest.Tone(440, 8000)}
	b := &audiotest.Device{}
	h.join(t, "1", "1", a, Options{RelayOnly: true, Lossy: lossy})
	sb := h.join(t, "1", "2", b, Options{RelayOnly: true})
	checkHeard(t, "the far end", steady(t, b, 2*time.Second), 440, 0.8)
	sb.mediaMu.Lock()
	m := sb.media
	sb.mediaMu.Unlock()
	var recovered int
	for _, sl := range m.slots {
		recovered += sl.buf.Stats().Recovered
	}
	if recovered == 0 {
		t.Error("no lost frame was recovered from FEC")
	}
}

// clicks is a microphone that is quiet but for a click every period: a 5 ms burst, loud. It notes when
// each click was captured, which is when its frame left the device. Quiet is not digital silence: a real
// microphone, and PipeWire's dither on a virtual one, leave a sample or two of noise, which keeps Opus's
// DTX sending a comfort-noise update every 400 ms or so. Digital silence sends none, and hid a jitter
// buffer that started from one of those updates and played everything 440 ms late (M29's manual pass).
type clicks struct {
	period int // in frames
	// at is the click's first sample's place in its frame. A device hands over a frame once its last
	// sample is captured, so the click was captured (FrameSamples - at) samples before its frame is.
	at    int
	mu    sync.Mutex
	sent  []time.Time
	noise uint32
}

func (c *clicks) source(i int, f []int16) {
	for k := range f {
		c.noise = c.noise*1664525 + 1013904223
		f[k] = int16(c.noise>>30) - 1 // -1, 0, 1 or 2
	}
	if i%c.period != c.period/2 {
		return
	}
	for k := range audio.SampleRate / 200 {
		f[c.at+k] = int16(20000 * math.Sin(2*math.Pi*1000*float64(k)/audio.SampleRate))
	}
	c.mu.Lock()
	c.sent = append(c.sent, time.Now().Add(-time.Duration(audio.FrameSamples-c.at)*time.Second/audio.SampleRate))
	c.mu.Unlock()
}

// onsets are when each sound after a silence was played: the frame's time plus the sample's place in it.
func onsets(heard []int16, at []time.Time) []time.Time {
	var out []time.Time
	quiet := audio.SampleRate // samples since the last loud one
	for i, v := range heard {
		if v > 4000 || v < -4000 {
			if quiet > audio.SampleRate/5 {
				f := i / audio.FrameSamples
				out = append(out, at[f].Add(time.Duration(i%audio.FrameSamples)*time.Second/audio.SampleRate))
			}
			quiet = 0
		} else {
			quiet++
		}
	}
	return out
}

// latencyBudget is mouth to ear on loopback, through the SFU, from the moment a click's sample was captured
// to the moment the far device was handed it. It covers the click's frame filling, the encoder's 6.5 ms
// lookahead, and where the playing clock's tick falls. The jitter buffer waits for one frame, so on a clean
// network there is no second packet to wait for. It leaves out each device's own buffer, about 21 ms each
// way through PipeWire, which M29's manual pass measured separately.
//
// Measured at 38 to 44 ms with the buffer one frame deep, against 59 to 65 at two. With one packet in ten
// lost it measured 56 to 76, since the first loss deepens the buffer to two frames for FEC. The test first
// stamped a click when its whole frame was handed over, with the click at the frame's start. That left out
// the frame's 20 ms of filling, and it measured 38 to 44 ms at two frames deep. The pass found the gap as
// unexplained latency between two VMs: about 112 ms each way, of which 42 ms is the two devices and the
// rest is this. The budget leaves room for a loaded CI runner's timers.
//
// From M30 the speaking caller's DSP is on, as every call's is, and adds 28.9 ms (dsp.Delay): measured at
// 69 to 71 ms, and 83 to 104 with the loss. Under -race on a loaded machine it reached 87.5 ms against
// what was then a 90 ms budget, so the budget rose by the DSP's delay, to keep the room M29 left for a
// loaded runner, and the test runs without -race, which slows the program it times (the author's call at
// M30). Run under -race, a timing test measures the race detector.
const latencyBudget = 120 * time.Millisecond

func TestMouthToEarStaysWithinItsBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("builds M26's harness and makes real calls")
	}
	for _, tc := range []struct {
		name   string
		lossy  bool
		budget time.Duration
	}{{"loopback", false, latencyBudget}, {"with loss", true, latencyBudget + 40*time.Millisecond}} {
		t.Run(tc.name, func(t *testing.T) {
			h := startHarness(t)
			// A click every 1.5 s, each one a new talk-spurt after DTX, falling where a real one would: inside
			// its frame, not at its start.
			c := &clicks{period: 75, at: 706}
			var lossy func() bool
			if tc.lossy {
				n := 0
				var mu sync.Mutex
				lossy = func() bool { mu.Lock(); defer mu.Unlock(); n++; return n%10 == 3 }
			}
			a := &audiotest.Device{Source: c.source}
			b := &audiotest.Device{}
			h.joinWith(t, "1", "1", a, dsp.AllOn(), Options{Lossy: lossy})
			h.join(t, "1", "2", b, Options{})
			time.Sleep(9 * time.Second)
			heard, at := b.Heard()
			got := onsets(heard, at)
			c.mu.Lock()
			sent := append([]time.Time(nil), c.sent...)
			c.mu.Unlock()
			var delays []time.Duration
			for _, g := range got {
				// The click it is: the latest sent before it was heard.
				var from time.Time
				for _, s := range sent {
					if s.Before(g) {
						from = s
					}
				}
				if !from.IsZero() {
					delays = append(delays, g.Sub(from))
				}
			}
			if len(delays) < 5 {
				t.Fatalf("%d clicks heard of %d sent", len(delays), len(sent))
			}
			delays = delays[1:] // the first is the call settling
			worst := time.Duration(0)
			for _, d := range delays {
				worst = max(worst, d)
			}
			t.Logf("%d clicks, mouth to ear %v, worst %v", len(delays), delays, worst)
			if worst > tc.budget {
				t.Errorf("mouth to ear reached %v, past the %v budget", worst, tc.budget)
			}
		})
	}
}
