// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"io"
	"sync"
	"sync/atomic"
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
// A stream still calling back is opened again too once another device has become the default (M34's manual
// pass): a headset plugged back in mid-call becomes Windows' default and the call stayed on the speakers. A
// stream says so through mover, which each OS answers its own way, and follow opens the new default at once.
// The stream that works is closed only once the new one is open (M34's /code-review): a new default that
// refuses to open, busy or switched off, left the call with no device at all. A stream does nothing with its
// samples until it is the one in use, which it becomes once the old one is closed, so two never feed the
// call at once.
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

// mover is a stream that can say another device has become the default since it opened. Moved is asked
// every followEvery, outside the follower's lock, which the stream's callback takes; it answers false when
// it cannot tell, and the silence rule is left to follow a device that went away.
type mover interface {
	Moved() bool
}

func moved(c io.Closer) bool {
	m, ok := c.(mover)
	return ok && m.Moved()
}

// noteDefault is a stream's poll of the default device, for an OS whose question must be asked on the
// stream's own thread (WASAPI): whether the default, now when ok, is another than opened, the one the stream
// opened on, replacing what the last poll said. A default that changed and changed back is the stream's
// own again, and following it then reopened the device the call was on (M34's second /code-review). An
// opened device not known, or no default to name, is no move.
func noteDefault(moved *atomic.Bool, opened, now string, ok bool) {
	moved.Store(ok && opened != "" && now != opened)
}

// follow opens a stream with open, and opens it again whenever it stops calling back or another device is
// the default. open is handed heard, which the stream's callback calls each time it runs, and whose answer
// is whether the stream is the one in use: a callback whose stream is not leaves the samples alone, playing
// silence. What follow returns closes whichever stream is open, and stops the following; an open that fails
// the first time is follow's error, as an open would be.
func follow(open func(heard func() bool) (io.Closer, error)) (io.Closer, error) {
	f := &follower{open: open, stop: make(chan struct{}), done: make(chan struct{}), active: 1, gen: 1}
	f.last = time.Now()
	c, err := open(f.heardFor(1))
	if err != nil {
		return nil, err
	}
	f.current = c
	go f.watch()
	return f, nil
}

type follower struct {
	open func(heard func() bool) (io.Closer, error)
	stop chan struct{}
	done chan struct{}
	once sync.Once

	mu      sync.Mutex
	last    time.Time // the last callback of the stream in use, or when it was opened
	current io.Closer // nil while none could be opened
	// active is the generation of the stream in use, gen the last one opened, and refused when a new
	// default last failed to open, which is tried again followAfter later.
	active, gen uint64
	refused     time.Time
	reopens     int // for a test
}

// heardFor is the heard of the stream opened as generation gen.
func (f *follower) heardFor(gen uint64) func() bool {
	return func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.active != gen {
			return false
		}
		f.last = time.Now()
		return true
	}
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
		// The watch is the one writer of current while it runs, so old is still current once Moved has
		// answered.
		f.mu.Lock()
		old := f.current
		stale := old == nil || time.Since(f.last) >= followAfter
		retry := time.Since(f.refused) >= followAfter
		f.mu.Unlock()
		switch {
		case stale:
			f.reopen(old)
		case retry && moved(old):
			f.move(old)
		}
	}
}

// reopen replaces a stream that stopped calling back, or opens one where none could be: closed first, since
// its device is gone. Closed outside the lock, which the stream's callback takes, and which Close takes too.
// One opened as the follower is closed is kept as current, and Close, which waits for this loop to return,
// closes it.
func (f *follower) reopen(old io.Closer) {
	f.mu.Lock()
	f.current = nil
	f.gen++
	gen := f.gen
	f.active = gen
	f.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	c, err := f.open(f.heardFor(gen))
	f.mu.Lock()
	f.last = time.Now()
	if err == nil {
		f.current = c
		f.reopens++
	}
	f.mu.Unlock()
}

// move opens the new default beside a stream that still works, and closes the old one only once the new one
// is open: a default that cannot be opened leaves the call where it was, tried again followAfter later. The
// new stream is in use once the old one is closed, Close waiting for its callbacks, so the two never run at
// once on the call's samples.
func (f *follower) move(old io.Closer) {
	f.mu.Lock()
	f.gen++
	gen := f.gen
	f.mu.Unlock()
	c, err := f.open(f.heardFor(gen))
	if err != nil {
		f.mu.Lock()
		f.refused = time.Now()
		f.mu.Unlock()
		return
	}
	f.mu.Lock()
	f.current = c
	f.mu.Unlock()
	_ = old.Close()
	f.mu.Lock()
	f.active, f.last = gen, time.Now()
	f.reopens++
	f.mu.Unlock()
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
