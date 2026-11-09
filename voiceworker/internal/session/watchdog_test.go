// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alexnex31/Norite/voiceworker/internal/audio"
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
	s := &Session{ended: make(chan string, 1)}
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
	opus.Encoder
	release chan struct{}
}

func (e stuckEncoder) Encode([]int16, []byte) (int, error) {
	<-e.release
	return 0, errors.New("stuck")
}

// An encode is a call into the same uninterruptible module, and one that never returns ends the call too,
// rather than leaving the caller muted with no reason given.
func TestAnEncodeThatNeverReturnsEndsTheCall(t *testing.T) {
	s := &Session{ended: make(chan string, 1)}
	release := make(chan struct{})
	defer close(release)
	m := &media{s: s, stop: make(chan struct{}), frames: make(chan []int16, 1), base: time.Now(),
		enc: stuckEncoder{release: release}, codec: keepOpen{}}
	m.sent.Add(2)
	go m.watch()
	go m.send()
	m.frames <- make([]int16, audio.FrameSamples)
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
	m := &media{s: &Session{ended: make(chan string, 1)}, stop: make(chan struct{}), base: time.Now(),
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
	m := &media{s: &Session{ended: make(chan string, 1)}, stop: make(chan struct{}), base: time.Now(),
		codec: keepOpen{}}
	m.close()
	d := &devices{}
	m.openDevices(context.Background(), d)
	if d.opened != 0 {
		t.Errorf("%d devices opened after the call closed", d.opened)
	}

	m = &media{s: &Session{ended: make(chan string, 1)}, stop: make(chan struct{}), base: time.Now(),
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
	m := &media{s: &Session{ended: make(chan string, 1)}, stop: make(chan struct{}), base: time.Now(),
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
	m := &media{s: &Session{ended: make(chan string, 1)}, stop: make(chan struct{}), base: time.Now(),
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
	m := &media{s: &Session{ended: make(chan string, 1)}, stop: make(chan struct{}), base: time.Now(),
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
	m := &media{s: &Session{ended: make(chan string, 1), opts: Options{Log: io.Discard}}, stop: make(chan struct{}),
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
	s := &Session{ended: make(chan string, 1), opts: Options{Log: io.Discard}}
	release := make(chan struct{})
	defer close(release)
	m := &media{s: s, stop: make(chan struct{}), frames: make(chan []int16, 1), base: time.Now(),
		chain: stuckChain{release: release}, enc: keeping{got: make(chan []int16, 8)}, codec: keepOpen{}}
	m.sent.Add(2)
	go m.watch()
	go m.send()
	m.frames <- make([]int16, audio.FrameSamples)
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
	opus.Encoder
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
	s := &Session{ended: make(chan string, 1), opts: Options{Log: lockedWriter{&logMu, &log}}}
	closed := false
	enc := keeping{got: make(chan []int16, 4)}
	m := &media{s: s, stop: make(chan struct{}), frames: make(chan []int16, 4), base: time.Now(),
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
		m.frames <- append([]int16(nil), frame...)
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

// A sender whose DSP cannot keep up keeps its queue full, so every frame it takes has others waiting
// behind it. It still sends: the encoder skips a frame, never two in a row, and the caller is choppy where
// skipping every frame would make them mute with nothing to say why.
func TestASenderTooSlowForTheDSPStillSends(t *testing.T) {
	s := &Session{ended: make(chan string, 1), opts: Options{Log: io.Discard}}
	enc := keeping{got: make(chan []int16, 100)}
	m := &media{s: s, stop: make(chan struct{}), frames: make(chan []int16, captureBacklog), base: time.Now(),
		chain: slowChain{}, enc: enc, codec: keepOpen{}}
	m.sent.Add(1)
	go m.send()
	tick := time.NewTicker(audio.FrameSamples * time.Second / audio.SampleRate)
	for range 75 { // a second and a half of microphone
		<-tick.C
		m.captured(make([]int16, audio.FrameSamples))
	}
	tick.Stop()
	close(m.stop)
	m.sent.Wait()
	encoded := len(enc.got)
	t.Logf("%d frames encoded, %d skipped, %d dropped", encoded, m.skipped.Load(), m.dropped.Load())
	if m.skipped.Load() == 0 {
		t.Fatal("the sender never fell behind, so this tested nothing")
	}
	if encoded < 15 {
		t.Errorf("an overloaded sender encoded %d frames in a second and a half", encoded)
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
	s := &Session{ended: make(chan string, 1), opts: Options{Log: io.Discard}, dspDone: make(chan struct{})}
	enc := keeping{got: make(chan []int16, 4)}
	m := &media{s: s, stop: make(chan struct{}), frames: make(chan []int16, captureBacklog), base: time.Now(),
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
		m.frames <- make([]int16, audio.FrameSamples)
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

// With echo cancellation off nothing reads the reference, so send does not walk the ring for it: held by
// counting rather than timing (CLAUDE.md, M21). With it on, every frame played is handed over.
func TestTheReferenceIsReadOnlyForEchoCancellation(t *testing.T) {
	for _, on := range []bool{false, true} {
		renders := 0
		m := &media{s: &Session{opts: Options{Log: io.Discard, Processing: dsp.Settings{EchoCancellation: on}}},
			chain: counting{renders: &renders}}
		for range 10 {
			m.ref.put(make([]int16, audio.FrameSamples))
			m.process(make([]int16, audio.FrameSamples))
		}
		want := 0
		if on {
			want = 10 * audio.FrameSamples / dsp.ChunkSamples
		}
		if renders != want {
			t.Errorf("echo cancellation %v: %d chunks of reference rendered, want %d", on, renders, want)
		}
	}
}
