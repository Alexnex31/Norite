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
// their device, and refuses to open while it says there is none, or refuses refuse, a device that cannot be
// opened. Each opens on def, the default device, and says it moved once def is another. used is which
// stream each callback that was in use came from, in order.
type fakeStreams struct {
	mu        sync.Mutex
	none      bool
	def       string
	refuse    string
	opened    int
	open      []*fakeStream
	used      []int
	callbacks atomic.Int64
	// slowClose keeps a stream calling back that long into its Close, as a device's thread runs on until
	// it is stopped.
	slowClose time.Duration
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
		time.Sleep(s.f.slowClose)
		close(s.stop)
	}
	<-s.done
	return nil
}

func (f *fakeStreams) openWith(heard func() bool) (io.Closer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.none || (f.refuse != "" && f.def == f.refuse) {
		return nil, errors.New("no device")
	}
	f.opened++
	id := f.opened
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
				if heard() {
					f.mu.Lock()
					f.used = append(f.used, id)
					f.mu.Unlock()
				}
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
	f := &fakeStreams{def: "speakers", slowClose: 30 * time.Millisecond}
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
	// The new stream fed the call only once the old one had stopped: never the two interleaved.
	f.mu.Lock()
	used := append([]int(nil), f.used...)
	f.mu.Unlock()
	for i := 1; i < len(used); i++ {
		if used[i] < used[i-1] {
			t.Fatalf("the old stream fed the call after the new one: %v", used)
		}
	}
	if len(used) == 0 || used[len(used)-1] != 2 {
		t.Errorf("the new stream never fed the call: %v", used)
	}
}

// A new default that cannot be opened leaves the stream that works open and in use, and is tried again
// later, not at every look (M34's /code-review): closed first, the call had no device at all.
func TestANewDefaultThatWillNotOpenLeavesTheStreamThatWorks(t *testing.T) {
	quickFollowing(t)
	f := &fakeStreams{def: "speakers", refuse: "headset"}
	c, err := follow(f.openWith)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	first := f.latest()
	f.mu.Lock()
	f.def = "headset"
	f.mu.Unlock()
	time.Sleep(5 * followAfter)
	if n := first.closes.Load(); n != 0 {
		t.Fatalf("the stream that works was closed for a default that would not open")
	}
	f.mu.Lock()
	used := len(f.used)
	f.mu.Unlock()
	eventually(t, "the stream that works stopped feeding the call", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.used) > used+5
	})
	f.mu.Lock()
	f.refuse = ""
	f.mu.Unlock()
	eventually(t, "the new default was not tried again", func() bool { return f.count() == 2 })
	if on := f.latest().on; on != "headset" {
		t.Errorf("opened again on %q", on)
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
