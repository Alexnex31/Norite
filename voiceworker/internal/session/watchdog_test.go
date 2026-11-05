// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alexnex31/Norite/voiceworker/internal/audio"
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
	hold           chan struct{}
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
		<-d.hold
	}
	return d.open()
}
func (d *devices) Capture(context.Context, func([]int16)) (io.Closer, error) { return d.open() }

// A leave that lands while the devices open closes them once they have; one that came first leaves them
// unopened. Neither leaves a microphone open after its call.
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
	d = &devices{hold: make(chan struct{})}
	done := make(chan struct{})
	go func() { m.openDevices(context.Background(), d); close(done) }()
	closed := make(chan struct{})
	go func() { time.Sleep(50 * time.Millisecond); m.close(); close(closed) }()
	time.Sleep(100 * time.Millisecond)
	close(d.hold)
	<-done
	<-closed
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.opened != 2 || d.closed != 2 {
		t.Errorf("opened %d devices and closed %d", d.opened, d.closed)
	}
}
