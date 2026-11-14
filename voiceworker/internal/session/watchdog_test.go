// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"

	"github.com/Alexnex31/Norite/voiceworker/internal/audio"
	"github.com/Alexnex31/Norite/voiceworker/internal/congestion"
	"github.com/Alexnex31/Norite/voiceworker/internal/dsp"
	"github.com/Alexnex31/Norite/voiceworker/internal/jitter"
	"github.com/Alexnex31/Norite/voiceworker/internal/opus"
)

// stuck is a decoder whose Decode never returns until released: a libopus bug that loops, which no call
// into WebAssembly can interrupt (ADR 0036).
type stuck struct {
	opus.Decoder
	release chan struct{}
}

func (d stuck) Decode(context.Context, []byte, []int16) (int, error) { <-d.release; return 0, nil }
func (d stuck) Reset(context.Context) error                          { return nil }
func (d stuck) Conceal(context.Context, []int16) (int, error)        { return 0, errors.New("stuck") }

// A decode that never returns ends the call, with the reason, rather than leaving a core spinning and the
// speakers silent until somebody leaves.
func TestADecodeThatNeverReturnsEndsTheCall(t *testing.T) {
	s := &Session{cc: testController(), ended: make(chan string, 1)}
	release := make(chan struct{})
	defer close(release)
	buf := jitter.New()
	for i := range jitter.MinDelay {
		buf.Push(7, uint16(i), uint32(i*jitter.FrameSamples), []byte{0xf8, 0xff, 0xfe}, time.Now())
	}
	m := &media{s: s, stop: make(chan struct{}), base: time.Now(), slots: []*slot{{buf: buf, dec: stuck{release: release},
		pcm: make([]int16, 0, opus.MaxDecodeSamples+audio.FrameSamples), tmp: make([]int16, opus.MaxDecodeSamples)}}}
	m.sent.Add(1)
	go m.watch()
	go m.fill(make([]int16, audio.FrameSamples))
	select {
	case reason := <-s.ended:
		if !strings.Contains(reason, "decoder stopped responding") {
			t.Errorf("the call ended with %q", reason)
		}
	case <-time.After(3 * stuckDecode):
		t.Fatal("the call did not end")
	}
	close(m.stop)
	m.sent.Wait()
}

// stuckEncoder is an encoder whose Encode never returns until released.
type stuckEncoder struct {
	tunable
	release chan struct{}
}

func (e stuckEncoder) Encode([]int16, []byte) (int, error) {
	<-e.release
	return 0, errors.New("stuck")
}

// An encode is a call into the same uninterruptible module, and one that never returns ends the call too,
// rather than leaving the caller muted with no reason given.
func TestAnEncodeThatNeverReturnsEndsTheCall(t *testing.T) {
	s := &Session{cc: testController(), ended: make(chan string, 1)}
	release := make(chan struct{})
	defer close(release)
	m := &media{s: s, stop: make(chan struct{}), frames: make(chan captureFrame, 1), base: time.Now(),
		enc: stuckEncoder{release: release}, codec: keepOpen{}}
	m.sent.Add(2)
	go m.watch()
	go m.send()
	m.frames <- captureFrame{pcm: make([]int16, audio.FrameSamples)}
	select {
	case reason := <-s.ended:
		if !strings.Contains(reason, "encoder stopped responding") {
			t.Errorf("the call ended with %q", reason)
		}
	case <-time.After(3 * stuckDecode):
		t.Fatal("the call did not end")
	}
	// And leaving does not wait on the encode for ever: the worker must still close its connection.
	closed := make(chan struct{})
	go func() { m.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * stuckDecode):
		t.Fatal("closing waited on the stuck encode")
	}
}

// silent is a decoder that answers every call with no samples and no error, which the interface permits.
type silent struct{ opus.Decoder }

func (silent) Decode(context.Context, []byte, []int16) (int, error) { return 0, errors.New("nothing") }
func (silent) Conceal(context.Context, []int16) (int, error)        { return 0, nil }

// A decoder that writes nothing still moves the slot on by a frame of silence, or the mix would ask it
// again for ever on the speakers' thread, holding the lock a leave needs.
func TestADecoderThatWritesNothingCostsAFrame(t *testing.T) {
	buf := jitter.New()
	for i := range jitter.MinDelay {
		buf.Push(7, uint16(i), uint32(i*jitter.FrameSamples), []byte{0xf8}, time.Now())
	}
	m := &media{s: &Session{cc: testController(), ended: make(chan string, 1)}, stop: make(chan struct{}), base: time.Now(),
		slots: []*slot{{buf: buf, dec: silent{}, pcm: make([]int16, 0, opus.MaxDecodeSamples+audio.FrameSamples),
			tmp: make([]int16, opus.MaxDecodeSamples)}}}
	done := make(chan struct{})
	go func() { m.fill(make([]int16, audio.FrameSamples)); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the mix did not return")
	}
}

// The watchdog reads the monotonic clock: a call into the codec noted a moment ago is not stuck whatever the
// wall clock did since, and one noted past stuckDecode ago is.
func TestTheWatchdogRunsOnTheMonotonicClock(t *testing.T) {
	m := &media{base: time.Now().Add(-time.Hour)}
	m.decoding.Store(int64(time.Since(m.base)) + 1)
	if m.stuck(&m.decoding) {
		t.Error("a decode that just started is stuck")
	}
	m.decoding.Store(int64(time.Since(m.base)-2*stuckDecode) + 1)
	if !m.stuck(&m.decoding) {
		t.Error("a decode two bounds old is not stuck")
	}
	m.decoding.Store(0)
	if m.stuck(&m.decoding) {
		t.Error("no decode at all is stuck")
	}
}

// devices counts what was opened and closed, and holds Play until released.
type devices struct {
	mu             sync.Mutex
	opened, closed int
	hold, entered  chan struct{}
}

type closer struct{ d *devices }

func (c closer) Close() error { c.d.mu.Lock(); c.d.closed++; c.d.mu.Unlock(); return nil }

func (d *devices) open() (io.Closer, error) {
	d.mu.Lock()
	d.opened++
	d.mu.Unlock()
	return closer{d}, nil
}

func (d *devices) Play(context.Context, func([]int16)) (io.Closer, error) {
	if d.hold != nil {
		close(d.entered)
		<-d.hold
	}
	return d.open()
}
func (d *devices) Capture(context.Context, func([]int16)) (io.Closer, error) { return d.open() }

// A leave that comes before the devices open leaves them unopened. One that lands while a device opens
// does not wait for it, and the device, once open, sees the leave and closes itself. Staged, not raced:
// Play is held until close has returned.
func TestAMicrophoneIsNeverLeftOpenAfterTheCall(t *testing.T) {
	m := &media{s: &Session{cc: testController(), ended: make(chan string, 1)}, stop: make(chan struct{}), base: time.Now(),
		codec: keepOpen{}}
	m.close()
	d := &devices{}
	m.openDevices(context.Background(), d)
	if d.opened != 0 {
		t.Errorf("%d devices opened after the call closed", d.opened)
	}

	m = &media{s: &Session{cc: testController(), ended: make(chan string, 1)}, stop: make(chan struct{}), base: time.Now(),
		codec: keepOpen{}}
	d = &devices{hold: make(chan struct{}), entered: make(chan struct{})}
	done := make(chan struct{})
	go func() { m.openDevices(context.Background(), d); close(done) }()
	within(t, d.entered, "Play was not called")
	closed := make(chan struct{})
	go func() { m.close(); close(closed) }()
	within(t, closed, "close waited on a device still opening")
	close(d.hold)
	within(t, done, "the devices did not finish opening")
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.opened != 1 || d.closed != 1 || m.playback != nil || m.capture != nil {
		t.Errorf("opened %d, closed %d, kept %v and %v", d.opened, d.closed, m.playback, m.capture)
	}
}

func within(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(2 * time.Second):
		t.Fatal(what)
	}
}

// blocking is a device whose Close waits on a callback in progress, as WASAPI's and AudioQueue's do, and
// the callback is a mix stuck in a decode.
type blocking struct{ release chan struct{} }

func (b blocking) Close() error { <-b.release; return nil }

// Leaving a call whose decoder is stuck still returns, in time for the connection's goodbye.
func TestClosingIsBoundedWhateverIsStuck(t *testing.T) {
	release := make(chan struct{})
	m := &media{s: &Session{cc: testController(), ended: make(chan string, 1)}, stop: make(chan struct{}), base: time.Now(),
		codec: keepOpen{}, playback: blocking{release}, capture: blocking{release}}
	m.sent.Add(1) // a sender stuck in an encode
	m.mu.Lock()   // and a mix stuck in a decode
	closed := make(chan struct{})
	go func() { m.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(closeWithin + 200*time.Millisecond):
		t.Error("closing waited past its bound") // bounded, so a removed bound fails rather than hangs
	}
	// Unstick everything, and wait for close's own goroutine to finish rather than outlive the test: it
	// closes the codec last, which keepOpen makes visible as closed.
	close(release)
	m.sent.Done()
	m.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for !m.closedFlag() {
		if time.Now().After(deadline) {
			t.Fatal("close's goroutine did not finish once everything was released")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A slot that changes sender plays nothing more of the previous one's, even what their last packet decoded
// to and had not played yet.
func TestANewSenderHearsNothingOfThePrevious(t *testing.T) {
	buf := jitter.New()
	sl := &slot{buf: buf, dec: long{}, pcm: make([]int16, 0, opus.MaxDecodeSamples+audio.FrameSamples),
		tmp: make([]int16, opus.MaxDecodeSamples)}
	m := &media{s: &Session{cc: testController(), ended: make(chan string, 1)}, stop: make(chan struct{}), base: time.Now(),
		slots: []*slot{sl}}
	now := time.Now()
	for i := range jitter.MinDelay {
		buf.Push(7, uint16(i), uint32(i*opus.MaxDecodeSamples), []byte{1}, now)
	}
	out := make([]int16, audio.FrameSamples)
	m.fill(out) // the previous sender's 120 ms packet: 20 ms played, 100 ms left over
	if out[0] != 1 {
		t.Fatalf("the first sender was not heard: %d", out[0])
	}
	for i := range jitter.MinDelay {
		buf.Push(9, uint16(10+i), uint32(10_000+i*jitter.FrameSamples), []byte{2}, now)
	}
	m.fill(out)
	for i, v := range out {
		if v == 1 {
			t.Fatalf("sample %d after the change is the previous sender's", i)
		}
	}
}

// The same with a leftover shorter than a frame, which the mix tops up from the next step: the step that
// carries the reset must drop it, not play it ahead of the new sender's first samples.
func TestANewSenderHearsNothingOfThePreviousShortTail(t *testing.T) {
	buf := jitter.New()
	sl := &slot{buf: buf, dec: thirty{}, pcm: make([]int16, 0, opus.MaxDecodeSamples+audio.FrameSamples),
		tmp: make([]int16, opus.MaxDecodeSamples)}
	m := &media{s: &Session{cc: testController(), ended: make(chan string, 1)}, stop: make(chan struct{}), base: time.Now(),
		slots: []*slot{sl}}
	now := time.Now()
	for i := range jitter.MinDelay {
		buf.Push(7, uint16(i), uint32(i*1440), []byte{1}, now)
	}
	out := make([]int16, audio.FrameSamples)
	m.fill(out) // 30 ms decoded, 20 played, 10 left over
	for i := range jitter.MinDelay {
		buf.Push(9, uint16(10+i), uint32(10_000+i*1440), []byte{2}, now)
	}
	m.fill(out)
	for i, v := range out {
		if v == 1 {
			t.Fatalf("sample %d after the change is the previous sender's", i)
		}
	}
}

// thirty decodes every packet to 30 ms of its first byte.
type thirty struct{ long }

func (thirty) Decode(_ context.Context, p []byte, pcm []int16) (int, error) {
	for i := range 1440 {
		pcm[i] = int16(p[0])
	}
	return 1440, nil
}

// long decodes every packet to 120 ms of its first byte.
type long struct{ opus.Decoder }

func (long) Decode(_ context.Context, p []byte, pcm []int16) (int, error) {
	for i := range opus.MaxDecodeSamples {
		pcm[i] = int16(p[0])
	}
	return opus.MaxDecodeSamples, nil
}
func (long) Reset(context.Context) error                         { return nil }
func (long) Conceal(_ context.Context, pcm []int16) (int, error) { clear(pcm[:960]); return 960, nil }

func (m *media) closedFlag() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// slowClose is a device whose Close waits until released, as a late device's would on a stuck callback,
// and says when it has started.
type slowClose struct {
	started, release chan struct{}
}

func (s slowClose) Close() error { close(s.started); <-s.release; return nil }

type lateDevice struct {
	entered, hold chan struct{}
	dev           slowClose
}

func (d *lateDevice) Play(context.Context, func([]int16)) (io.Closer, error) {
	close(d.entered)
	<-d.hold
	return d.dev, nil
}
func (d *lateDevice) Capture(context.Context, func([]int16)) (io.Closer, error) {
	return nil, errors.New("no microphone")
}

// A device that finishes opening after the leave closes itself without holding the lock close takes first:
// its Close may wait on a stuck callback, and close must not wait with it.
func TestALateDeviceClosesItselfOutsideTheLock(t *testing.T) {
	m := &media{s: &Session{cc: testController(), ended: make(chan string, 1), opts: Options{Log: io.Discard}}, stop: make(chan struct{}),
		base: time.Now(), codec: keepOpen{}}
	d := &lateDevice{entered: make(chan struct{}), hold: make(chan struct{}),
		dev: slowClose{started: make(chan struct{}), release: make(chan struct{})}}
	defer close(d.dev.release)
	done := make(chan struct{})
	go func() { m.openDevices(context.Background(), d); close(done) }()
	within(t, d.entered, "Play was not called")
	m.close()
	close(d.hold)
	within(t, d.dev.started, "the late device did not close itself")
	// Its Close has started and is waiting: devMu must be free for close to take.
	if !m.devMu.TryLock() {
		t.Fatal("the late device closes while holding devMu")
	}
	m.devMu.Unlock()
}

// stuckChain is a DSP whose Capture never returns until released.
type stuckChain struct {
	dsp.Chain
	release chan struct{}
}

func (stuckChain) Render([]int16) error { return nil }
func (stuckChain) Close() error         { return nil }
func (c stuckChain) Capture([]int16) (float32, error) {
	<-c.release
	return 0, errors.New("stuck")
}

// A call into the DSP cannot be interrupted either, and its reference is a stranger's audio, so one that
// never returns ends the call as a stuck codec does: nothing else frees the core (ADR 0036).
func TestAProcessingThatNeverReturnsEndsTheCall(t *testing.T) {
	// A log, because the release at the test's end lets the stuck call fail, and the sender logs it.
	s := &Session{cc: testController(), ended: make(chan string, 1), opts: Options{Log: io.Discard}}
	release := make(chan struct{})
	defer close(release)
	m := &media{s: s, stop: make(chan struct{}), frames: make(chan captureFrame, 1), base: time.Now(),
		chain: stuckChain{release: release}, enc: keeping{got: make(chan []int16, 8)}, codec: keepOpen{}}
	m.sent.Add(2)
	go m.watch()
	go m.send()
	m.frames <- captureFrame{pcm: make([]int16, audio.FrameSamples)}
	select {
	case reason := <-s.ended:
		if reason != "the audio processing stopped responding, so the call was ended" {
			t.Errorf("the call ended with %q", reason)
		}
	case <-time.After(3 * stuckDecode):
		t.Fatal("the call did not end")
	}
	closed := make(chan struct{})
	go func() { m.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * stuckDecode):
		t.Fatal("closing waited on the stuck processing")
	}
}

// stuckModule never returns from making a chain until released, as a loop in a constructor would not.
type stuckModule struct {
	dsp.Module
	release chan struct{}
}

func (m stuckModule) NewChain(context.Context, dsp.Settings) (dsp.Chain, error) {
	<-m.release
	return nil, errors.New("stuck")
}

func (stuckModule) Close(context.Context) error { return nil }

// Making the chain runs WebAssembly too, the module's constructors and dsp_new, so one that never returns
// ends the call like a stuck frame: it would otherwise hold the microphone silent for the rest of the call,
// with nothing in the log to say why (M30 /code-review).
func TestAChainThatIsNeverMadeEndsTheCall(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	s := &Session{cc: testController(), ended: make(chan string, 1), opts: Options{Log: io.Discard}, dspDone: make(chan struct{}),
		dsp: stuckModule{release: release}}
	close(s.dspDone)
	m := &media{s: s, stop: make(chan struct{}), frames: make(chan captureFrame, 1), base: time.Now(),
		enc: keeping{got: make(chan []int16, 8)}, codec: keepOpen{}}
	m.sent.Add(2)
	go m.watch()
	go m.send()
	m.frames <- captureFrame{pcm: make([]int16, audio.FrameSamples)}
	select {
	case reason := <-s.ended:
		if reason != "the audio processing stopped responding, so the call was ended" {
			t.Errorf("the call ended with %q", reason)
		}
	case <-time.After(3 * stuckDecode):
		t.Fatal("the call did not end")
	}
}

// failingChain fails every call, as a chain a trap broke does.
type failingChain struct {
	dsp.Chain
	closed *bool
}

func (failingChain) Render([]int16) error { return dsp.ErrBroken }
func (failingChain) Capture(pcm []int16) (float32, error) {
	clear(pcm) // what a half-finished call could leave behind
	return 0, dsp.ErrBroken
}
func (c failingChain) Close() error { *c.closed = true; return nil }

// keeping is an encoder that keeps what it is given, and sends nothing.
type keeping struct {
	tunable
	got chan []int16
}

func (e keeping) Encode(pcm []int16, _ []byte) (int, error) {
	e.got <- append([]int16(nil), pcm...)
	return 0, nil
}

// A DSP that fails is dropped for the rest of the call, and the call goes on with its audio unprocessed:
// the reference is a stranger's audio, and a failure it can cause must not be a way to end other people's
// calls (M30's plan, D1). One line in the log says so.
func TestAFailedDSPLeavesTheCallUnprocessed(t *testing.T) {
	var log strings.Builder
	var logMu sync.Mutex
	s := &Session{cc: testController(), ended: make(chan string, 1), opts: Options{Log: lockedWriter{&logMu, &log}}}
	closed := false
	enc := keeping{got: make(chan []int16, 4)}
	m := &media{s: s, stop: make(chan struct{}), frames: make(chan captureFrame, 4), base: time.Now(),
		chain: failingChain{closed: &closed}, enc: enc, codec: keepOpen{}}
	m.sent.Add(1)
	go m.send()
	frame := make([]int16, audio.FrameSamples)
	for i := range frame {
		frame[i] = int16(i)
	}
	// The first frame is the one the chain fails on, having cleared it as a half-finished call could: it
	// goes as it was captured. The others go with no chain at all.
	for i := range 3 {
		m.frames <- captureFrame{pcm: append([]int16(nil), frame...)}
		select {
		case got := <-enc.got:
			if !slices.Equal(got, frame) {
				t.Errorf("frame %d was changed on its way to the encoder by a DSP that failed", i)
			}
		case <-time.After(time.Second):
			t.Fatal("the frame did not reach the encoder")
		}
	}
	close(m.stop)
	m.sent.Wait()
	select {
	case reason := <-s.ended:
		t.Fatalf("the call ended: %s", reason)
	default:
	}
	if !closed || m.chain != nil {
		t.Error("the failed chain was kept")
	}
	logMu.Lock()
	defer logMu.Unlock()
	if strings.Count(log.String(), "the audio processing failed, so the rest of the call goes unprocessed") != 1 {
		t.Errorf("the log says %q", log.String())
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// slowChain takes longer than real time over every chunk: a machine too slow for the DSP.
type slowChain struct{ dsp.Chain }

func (slowChain) Render([]int16) error { return nil }
func (slowChain) Close() error         { return nil }
func (slowChain) Capture([]int16) (float32, error) {
	time.Sleep(13 * time.Millisecond) // two chunks a frame: 26 ms of work every 20 ms
	return 1, nil
}

// A sender whose DSP cannot keep up keeps its queue full, and skipping the encoder cannot drain it: it
// filled to captureBacklog, 320 ms behind the microphone, and the far end, sent half the frames, heard
// nothing (M30 /code-review). The DSP goes once it has kept the sender behind for slowDSP frames, and the
// queue drains: what is sent is back within captureQueue of the microphone, every frame of it after that.
func TestADSPTooSlowForRealTimeIsDropped(t *testing.T) {
	var mu sync.Mutex
	var logged bytes.Buffer
	s := &Session{cc: testController(), ended: make(chan string, 1), opts: Options{Log: lockedWriter{&mu, &logged}}}
	enc := keeping{got: make(chan []int16, 100)}
	m := &media{s: s, stop: make(chan struct{}), frames: make(chan captureFrame, captureBacklog), base: time.Now(),
		chain: slowChain{}, enc: enc, codec: keepOpen{}}
	m.sent.Add(1)
	go m.send()
	tick := time.NewTicker(audio.FrameSamples * time.Second / audio.SampleRate)
	deepest := 0
	for range 75 { // a second and a half of microphone
		<-tick.C
		m.captured(make([]int16, audio.FrameSamples))
		deepest = max(deepest, len(m.frames))
	}
	tick.Stop()
	time.Sleep(50 * time.Millisecond) // the last frame's turn
	left := len(m.frames)
	close(m.stop)
	m.sent.Wait()
	encoded := len(enc.got)
	t.Logf("%d frames encoded, %d skipped, %d dropped; %d deepest, %d left", encoded, m.skipped.Load(),
		m.dropped.Load(), deepest, left)
	if m.skipped.Load() == 0 {
		t.Fatal("the sender never fell behind, so this tested nothing")
	}
	mu.Lock()
	said := logged.String()
	mu.Unlock()
	if !strings.Contains(said, "the audio processing is slower than real time on this machine, so the rest of "+
		"the call goes unprocessed\n") {
		t.Errorf("the log does not say the DSP was dropped:\n%s", said)
	}
	if m.dropped.Load() != 0 || deepest >= captureBacklog {
		t.Errorf("the queue filled: %d deep, %d frames dropped", deepest, m.dropped.Load())
	}
	if left >= captureQueue {
		t.Errorf("%d frames still waited at the end: the sender never caught up", left)
	}
	if encoded+int(m.skipped.Load()) != 75 {
		t.Errorf("%d encoded and %d skipped of 75", encoded, m.skipped.Load())
	}
}

// marking is a chain that marks every frame it processes, and markingModule makes it.
type marking struct{ dsp.Chain }

func (marking) Render([]int16) error { return nil }
func (marking) Close() error         { return nil }
func (marking) Capture(pcm []int16) (float32, error) {
	pcm[0] = 12345
	return 1, nil
}

type markingModule struct{ dsp.Module }

func (markingModule) NewChain(context.Context, dsp.Settings) (dsp.Chain, error) {
	return marking{}, nil
}
func (markingModule) Close(context.Context) error { return nil }

// The DSP compiles in about 200 ms, twice the codec, and a call does not wait for it: frames go out
// unprocessed until it is ready, and processed from then on.
func TestTheCallDoesNotWaitForTheDSP(t *testing.T) {
	s := &Session{cc: testController(), ended: make(chan string, 1), opts: Options{Log: io.Discard}, dspDone: make(chan struct{})}
	enc := keeping{got: make(chan []int16, 4)}
	m := &media{s: s, stop: make(chan struct{}), frames: make(chan captureFrame, captureBacklog), base: time.Now(),
		enc: enc, codec: keepOpen{}}
	m.sent.Add(1)
	go m.send()
	ready := false
	defer func() {
		// Bounded: a sender that waits for the DSP would otherwise hold this test for ever, rather than
		// fail it (CLAUDE.md, M28).
		close(m.stop)
		if !ready {
			close(s.dspDone)
		}
		m.sent.Wait()
	}()
	next := func() []int16 {
		m.frames <- captureFrame{pcm: make([]int16, audio.FrameSamples)}
		select {
		case got := <-enc.got:
			return got
		case <-time.After(time.Second):
			t.Fatal("the frame did not reach the encoder")
			return nil
		}
	}
	if next()[0] != 0 {
		t.Fatal("a frame was processed before the DSP had compiled")
	}
	s.dsp = markingModule{}
	close(s.dspDone)
	ready = true
	if next()[0] != 12345 {
		t.Error("the DSP, once compiled, was not taken up")
	}
}

// counting is a chain that counts the reference frames it is handed.
type counting struct {
	dsp.Chain
	renders *int
}

func (c counting) Render([]int16) error           { *c.renders++; return nil }
func (counting) Capture([]int16) (float32, error) { return 1, nil }
func (counting) Close() error                     { return nil }

// With echo cancellation off nothing reads the reference, so the speakers do not write it and send does
// not walk it: held by counting rather than timing (CLAUDE.md, M21). With it on, every frame played is
// handed over.
func TestTheReferenceIsReadOnlyForEchoCancellation(t *testing.T) {
	for _, on := range []bool{false, true} {
		renders := 0
		m := &media{s: &Session{opts: Options{Log: io.Discard}, processing: dsp.Settings{EchoCancellation: on}},
			chain: counting{renders: &renders}}
		for range 10 {
			m.fill(make([]int16, audio.FrameSamples))
			m.process(make([]int16, audio.FrameSamples), m.ref.count())
		}
		want := 0
		if on {
			want = 10 * audio.FrameSamples / dsp.ChunkSamples
		}
		if renders != want {
			t.Errorf("echo cancellation %v: %d chunks of reference rendered, want %d", on, renders, want)
		}
		if written := m.ref.count(); !on && written != 0 {
			t.Errorf("echo cancellation off: the speakers wrote %d frames of reference nobody reads", written)
		}
	}
}

// pairing is a chain that keeps, at every captured chunk, the most reference it had been handed beyond
// the microphone: what AEC3's render buffer would hold in excess.
type pairing struct {
	dsp.Chain
	mu                      *sync.Mutex
	renders, captures, most *int
}

func (p pairing) Render([]int16) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	*p.renders++
	return nil
}

func (p pairing) Capture([]int16) (float32, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	*p.captures++
	*p.most = max(*p.most, *p.renders-*p.captures)
	return 1, nil
}

func (pairing) Close() error { return nil }

// A sender running behind hands the echo canceller what had been played when each frame was recorded, and
// no more. Handing it everything played so far, as M30 first did, put what the sender was behind by in
// AEC3's render buffer, which resets its delay estimate when that stays above two frames for a second
// (M30 /code-review). The speakers and the microphone tick here in turn, and send is held three frames
// behind them throughout.
func TestASenderBehindRendersOnlyUpToTheFrame(t *testing.T) {
	var mu sync.Mutex
	renders, captures, most := 0, 0, 0
	gate := make(chan struct{}, 100)
	s := &Session{cc: testController(), ended: make(chan string, 1), processing: dsp.Settings{EchoCancellation: true},
		opts: Options{Log: io.Discard, stall: func() { <-gate }}}
	enc := keeping{got: make(chan []int16, 100)}
	m := &media{s: s, stop: make(chan struct{}), frames: make(chan captureFrame, captureBacklog), base: time.Now(),
		chain: pairing{mu: &mu, renders: &renders, captures: &captures, most: &most}, taken: true, enc: enc,
		codec: keepOpen{}}
	m.sent.Add(1)
	go m.send()
	frame := make([]int16, audio.FrameSamples)
	const behind, frames = 3, 60
	for i := range frames {
		m.fill(frame)
		m.captured(frame)
		if i >= behind {
			gate <- struct{}{}
			// send has taken frame i-3, and waits for it to be through before the next tick.
			deadline := time.Now().Add(time.Second)
			for len(enc.got) != i-behind+1 {
				if time.Now().After(deadline) {
					t.Fatalf("frame %d was not through within a second", i-behind)
				}
				time.Sleep(time.Millisecond)
			}
		}
	}
	close(gate) // what is left goes through, and send waits on nothing while it stops
	close(m.stop)
	m.sent.Wait()
	mu.Lock()
	defer mu.Unlock()
	if captures < 2*(frames-behind-1) {
		t.Fatalf("%d chunks captured, so this tested little", captures)
	}
	// One frame of two chunks: the speakers' tick before the microphone's.
	if most > 2 {
		t.Errorf("the echo canceller was handed %d chunks of reference beyond the microphone, want at most 2", most)
	}
}

// threeBytes encodes every frame as a packet worth sending.
type threeBytes struct{ tunable }

func (threeBytes) Encode(_ []int16, out []byte) (int, error) { return copy(out, []byte{1, 2, 3}), nil }

// recordingTrack keeps the timestamp of every packet written to it.
type recordingTrack struct {
	mu  sync.Mutex
	ts  []uint32
	seq []uint16
}

func (r *recordingTrack) WriteRTP(p *rtp.Packet) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ts, r.seq = append(r.ts, p.Timestamp), append(r.seq, p.SequenceNumber)
	return nil
}

// A frame skipped for being behind leaves a gap in the timestamps, as a frame DTX did not send does, and
// the sequence numbers run on: the far end hears a moment of silence. Timestamps that counted only the
// frames sent ran slower than the clock under a sender behind, and the far end's buffer, finding each
// packet late, deepened at every skip, or played nothing at all (M30 /code-review).
func TestASkippedFrameLeavesAGapInTheTimestamps(t *testing.T) {
	s := &Session{cc: testController(), ended: make(chan string, 1), opts: Options{Log: io.Discard}}
	track := &recordingTrack{}
	m := &media{s: s, stop: make(chan struct{}), frames: make(chan captureFrame, captureBacklog), base: time.Now(),
		enc: threeBytes{}, codec: keepOpen{}, track: track}
	// Six frames waiting before send starts: the first has five behind it and is skipped, and the queue
	// then drains without another skip.
	for range 6 {
		m.captured(make([]int16, audio.FrameSamples))
	}
	m.sent.Add(1)
	go m.send()
	deadline := time.Now().Add(time.Second)
	for {
		track.mu.Lock()
		n := len(track.ts)
		track.mu.Unlock()
		if n == 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d packets written in a second", n)
		}
		time.Sleep(time.Millisecond)
	}
	close(m.stop)
	m.sent.Wait()
	track.mu.Lock()
	defer track.mu.Unlock()
	if m.skipped.Load() != 1 {
		t.Fatalf("%d frames skipped, want the first alone", m.skipped.Load())
	}
	want := []uint32{1 * audio.FrameSamples, 2 * audio.FrameSamples, 3 * audio.FrameSamples,
		4 * audio.FrameSamples, 5 * audio.FrameSamples}
	if !slices.Equal(track.ts, want) {
		t.Errorf("timestamps %v, want %v", track.ts, want)
	}
	if !slices.Equal(track.seq, []uint16{0, 1, 2, 3, 4}) {
		t.Errorf("sequence numbers %v, want them to run on", track.seq)
	}
}

// A backlog from before the DSP was taken up is not the DSP's: a sender that fell behind while the module
// compiled takes it up and keeps it, rather than dropping it at its first frame for 25 it never processed
// (M30 /code-review).
func TestADSPTakenUpBehindIsNotBlamedForTheBacklog(t *testing.T) {
	var mu sync.Mutex
	var logged bytes.Buffer
	s := &Session{cc: testController(), ended: make(chan string, 1), dspDone: make(chan struct{}),
		opts: Options{Log: lockedWriter{&mu, &logged}}}
	n := 0
	// send is slow for its first 40 frames, with no DSP yet, and then the module is ready and send keeps up.
	s.opts.stall = func() {
		n++
		switch {
		case n < 40:
			time.Sleep(30 * time.Millisecond)
		case n == 40:
			s.dsp = markingModule{}
			close(s.dspDone)
		}
	}
	enc := keeping{got: make(chan []int16, 100)}
	m := &media{s: s, stop: make(chan struct{}), frames: make(chan captureFrame, captureBacklog), base: time.Now(),
		enc: enc, codec: keepOpen{}}
	m.sent.Add(1)
	go m.send()
	tick := time.NewTicker(audio.FrameSamples * time.Second / audio.SampleRate)
	for range 75 {
		<-tick.C
		m.captured(make([]int16, audio.FrameSamples))
	}
	tick.Stop()
	time.Sleep(50 * time.Millisecond)
	close(m.stop)
	m.sent.Wait()
	mu.Lock()
	said := logged.String()
	mu.Unlock()
	if strings.Contains(said, "slower than real time") {
		t.Errorf("the DSP was blamed for a backlog from before it was taken up:\n%s", said)
	}
	var last []int16
	for len(enc.got) > 0 {
		last = <-enc.got
	}
	if m.chain == nil || last == nil || last[0] != 12345 {
		t.Error("the call's last frame went unprocessed")
	}
}

// Once the call has no chain for good, the speakers stop writing a reference nobody will read: after a
// chain that failed, and when the module never started.
func TestNoChainForGoodStopsTheReference(t *testing.T) {
	closed := false
	m := &media{s: &Session{opts: Options{Log: io.Discard}, processing: dsp.Settings{EchoCancellation: true}},
		chain: failingChain{closed: &closed}, taken: true}
	frame := make([]int16, audio.FrameSamples)
	m.fill(frame)
	m.process(frame, m.ref.count())
	m.fill(frame)
	if !closed || m.ref.count() != 1 {
		t.Errorf("after the chain failed: closed %v, %d frames of reference written, want 1", closed, m.ref.count())
	}

	s := &Session{opts: Options{Log: io.Discard}, processing: dsp.Settings{EchoCancellation: true},
		dspDone: make(chan struct{}), dspErr: errors.New("no module")}
	close(s.dspDone)
	m = &media{s: s}
	m.process(frame, m.ref.count())
	m.fill(frame)
	if m.ref.count() != 0 {
		t.Errorf("with no module: %d frames of reference written, want none", m.ref.count())
	}
}

// testController is the controller New gives a session, for the sessions these tests build by hand.
func testController() *congestion.Controller {
	return newController(time.Now())
}

// tunable is a fake encoder's answer to the controller's adjustments, which the fakes take no notice of.
type tunable struct{ opus.Encoder }

func (tunable) SetBitrate(int) error { return nil }
func (tunable) SetLoss(int) error    { return nil }
