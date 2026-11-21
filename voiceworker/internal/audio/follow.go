// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"io"
	"sync"
	"time"
)

// Following the default device (M34, from M32's entry). A stream opened on the default device stays on that
// device: unplugged, it stops calling back and the call goes silent for good, which the session reports as
// failed by its silence. WASAPI's events stop with AUDCLNT_E_DEVICE_INVALIDATED, and an AudioQueue's
// callbacks stop with its device. Both say it the same way, the callbacks stopping, so one rule serves both:
// a stream that has not called back for followAfter is closed and opened again, on whatever the default is
// now, and again every followEvery until one opens. PulseAudio moves a stream itself (M32), and Linux does
// not follow.
//
// Here, in a file every OS compiles, for the reason outcome.go gives: CI runs Linux alone.

// followAfter is how long a stream may go without calling back before it is opened again: half the
// session's 2 s, after which it reports the device failed, so a device swapped for another is followed
// before anybody is told it failed. followEvery is how often a stream that could not be opened is tried
// again, and how often a stream is looked at.
var (
	followAfter = time.Second
	followEvery = 250 * time.Millisecond
)

// follow opens a stream with open, and opens it again whenever it stops calling back. open is handed heard,
// which the stream's callback calls each time it runs. What follow returns closes whichever stream is open,
// and stops the following; an open that fails the first time is follow's error, as an open would be.
func follow(open func(heard func()) (io.Closer, error)) (io.Closer, error) {
	f := &follower{open: open, stop: make(chan struct{}), done: make(chan struct{})}
	f.last = time.Now()
	c, err := open(f.heard)
	if err != nil {
		return nil, err
	}
	f.current = c
	go f.watch()
	return f, nil
}

type follower struct {
	open func(heard func()) (io.Closer, error)
	stop chan struct{}
	done chan struct{}
	once sync.Once

	mu      sync.Mutex
	last    time.Time // the last callback, or when the stream was opened
	current io.Closer // nil while none could be opened
	reopens int       // for a test
}

func (f *follower) heard() {
	f.mu.Lock()
	f.last = time.Now()
	f.mu.Unlock()
}

func (f *follower) watch() {
	defer close(f.done)
	tick := time.NewTicker(followEvery)
	defer tick.Stop()
	for {
		select {
		case <-f.stop:
			return
		case <-tick.C:
		}
		f.mu.Lock()
		stale := f.current == nil || time.Since(f.last) >= followAfter
		old := f.current
		if stale {
			f.current = nil
		}
		f.mu.Unlock()
		if !stale {
			continue
		}
		// Closed outside the lock, which the stream's callback takes, and which Close takes too.
		if old != nil {
			_ = old.Close()
		}
		// One opened as the follower is closed is kept as current, and Close, which waits for this loop to
		// return, closes it.
		c, err := f.open(f.heard)
		f.mu.Lock()
		f.last = time.Now()
		if err == nil {
			f.current = c
			f.reopens++
		}
		f.mu.Unlock()
	}
}

// Close stops the following, and closes the stream it has open, once the watch has returned: it never
// closes a stream the watch is opening or closing at the same moment.
func (f *follower) Close() error {
	f.once.Do(func() { close(f.stop) })
	<-f.done
	f.mu.Lock()
	c := f.current
	f.current = nil
	f.mu.Unlock()
	if c != nil {
		return c.Close()
	}
	return nil
}
