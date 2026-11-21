// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStreams opens streams that call back every few milliseconds until stopped, or until the test pulls
// their device, and refuses to open while it says there is none. Each opens on def, the default device, and
// says it moved once def is another.
type fakeStreams struct {
	mu        sync.Mutex
	none      bool
	def       string
	opened    int
	open      []*fakeStream
	callbacks atomic.Int64
}

type fakeStream struct {
	f            *fakeStreams
	on           string
	stop, pulled chan struct{}
	done         chan struct{}
	closes       atomic.Int32
}

func (s *fakeStream) Moved() bool {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	return s.f.def != s.on
}

func (s *fakeStream) Close() error {
	if s.closes.Add(1) == 1 {
		close(s.stop)
	}
	<-s.done
	return nil
}

func (f *fakeStreams) openWith(heard func()) (io.Closer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.none {
		return nil, errors.New("no device")
	}
	f.opened++
	s := &fakeStream{f: f, on: f.def, stop: make(chan struct{}), pulled: make(chan struct{}), done: make(chan struct{})}
	f.open = append(f.open, s)
	go func() {
		defer close(s.done)
		t := time.NewTicker(5 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-s.pulled:
				<-s.stop // a device pulled out: the stream stays, calling back no more
				return
			case <-t.C:
				f.callbacks.Add(1)
				heard()
			}
		}
	}()
	return s, nil
}

func (f *fakeStreams) latest() *fakeStream {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open[len(f.open)-1]
}

func (f *fakeStreams) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opened
}

func quickFollowing(t *testing.T) {
	t.Helper()
	after, every := followAfter, followEvery
	followAfter, followEvery = 100*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { followAfter, followEvery = after, every })
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A stream whose device goes away is opened again, on whatever the default is then, and calls back again;
// a stream that keeps calling back is never touched.
func TestAStreamThatStopsIsOpenedAgain(t *testing.T) {
	quickFollowing(t)
	f := &fakeStreams{}
	c, err := follow(f.openWith)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	time.Sleep(5 * followAfter)
	if n := f.count(); n != 1 {
		t.Fatalf("a stream calling back was opened %d times", n)
	}
	first := f.latest()
	close(first.pulled)
	eventually(t, "the stream was never opened again", func() bool { return f.count() == 2 })
	if first.closes.Load() != 1 {
		t.Errorf("the stream that stopped was closed %d times", first.closes.Load())
	}
	before := f.callbacks.Load()
	eventually(t, "the stream opened again never called back", func() bool { return f.callbacks.Load() > before+5 })
}

// A stream still calling back is opened again once another device is the default, on that device, and
// once only (M34's manual pass).
func TestAStreamFollowsANewDefault(t *testing.T) {
	quickFollowing(t)
	f := &fakeStreams{def: "speakers"}
	c, err := follow(f.openWith)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	first := f.latest()
	f.mu.Lock()
	f.def = "headset"
	f.mu.Unlock()
	eventually(t, "the stream stayed on the device that stopped being the default", func() bool { return f.count() == 2 })
	if first.closes.Load() != 1 {
		t.Errorf("the stream on the old default was closed %d times", first.closes.Load())
	}
	if on := f.latest().on; on != "headset" {
		t.Errorf("opened again on %q", on)
	}
	time.Sleep(5 * followAfter)
	if n := f.count(); n != 2 {
		t.Errorf("a stream on the default was opened again: %d opens", n)
	}
}

// With no device to open, the stream is tried again until there is one, and the first open's failure is the
// caller's, as an open's would be.
func TestAStreamWithNoDeviceIsTriedUntilThereIsOne(t *testing.T) {
	quickFollowing(t)
	f := &fakeStreams{none: true}
	if _, err := follow(f.openWith); err == nil {
		t.Fatal("a first open that failed was not the caller's error")
	}
	f.none = false
	c, err := follow(f.openWith)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	f.mu.Lock()
	f.none = true
	f.mu.Unlock()
	close(f.latest().pulled)
	time.Sleep(5 * followAfter)
	if n := f.count(); n != 1 {
		t.Fatalf("opened %d times with no device", n)
	}
	f.mu.Lock()
	f.none = false
	f.mu.Unlock()
	eventually(t, "the stream was not opened once a device came back", func() bool { return f.count() == 2 })
}

// Closing stops the following and closes the stream there is, once; nothing is opened after it.
func TestClosingAFollowedStreamClosesItOnce(t *testing.T) {
	quickFollowing(t)
	f := &fakeStreams{}
	c, err := follow(f.openWith)
	if err != nil {
		t.Fatal(err)
	}
	close(f.latest().pulled)
	eventually(t, "the stream was never opened again", func() bool { return f.count() == 2 })
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if n := f.latest().closes.Load(); n != 1 {
		t.Errorf("the stream open at the close was closed %d times", n)
	}
	time.Sleep(5 * followAfter)
	if n := f.count(); n != 2 {
		t.Errorf("a stream was opened after the close: %d opens", n)
	}
}
