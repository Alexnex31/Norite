// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"sync"
	"time"
)

// Clock is this machine's clock: what time it is, and a way to wait.
//
// An interface so the tests can skew it by hours and advance it by minutes without waiting for either.
// Production uses the real one, whose Now carries Go's monotonic reading — which is what serverClock relies
// on, since a difference between two monotonic readings is immune to the wall clock being stepped.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// serverClock estimates the instance's clock from the last time it said what time it was.
//
// The access token's expiry is the instance's time, and comparing it with this machine's wall clock is
// wrong by however far that clock is off — hours, on a machine with a dead CMOS battery or a hand-set
// clock. ADR 0010 has the daemon apply an offset instead, and this is that offset, kept as a sample rather
// than a number: the server's time when it was observed, plus how much local time has passed since, measured
// on the monotonic clock. So a wall clock stepped while the daemon runs (NTP, a person, a VM resuming)
// changes nothing.
//
// Samples come from two places: the Date header on every refresh response, which is how the first decision
// is made before any HELLO, and HELLO's server_time on every connection. A refresh's sample is dated from the
// request's start, so the round trip it took makes the instance look later rather than earlier.
//
// **Suspend is the case the monotonic clock gets wrong**, in the safe direction. On Linux it stops while the
// machine sleeps, so after a laptop wakes this estimate is behind by however long it slept, and a token can
// look valid that is not. That is survivable because of what a waking daemon does next: its connection is
// gone, it reconnects, and every connection begins with a HELLO that is sampled before the token is checked
// for IDENTIFY. A 4004 after all of that costs one refresh.
type serverClock struct {
	local Clock

	mu      sync.Mutex
	server  time.Time // the instance's time at the last sample
	sampled time.Time // this machine's clock at that moment, monotonic reading included
	ok      bool
}

// observe records the instance's clock as of now. A zero time is no sample and is ignored.
func (c *serverClock) observe(server time.Time) { c.observeAt(server, c.local.Now()) }

// observeAt records that the instance's clock read server when this machine's read local. A caller that
// knows only that the instance stamped its time somewhere after local passes local, so the estimate errs
// toward the instance's clock being later than it is — which is the safe direction for expiry.
func (c *serverClock) observeAt(server, local time.Time) {
	if server.IsZero() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.server, c.sampled, c.ok = server, local, true
}

// now estimates the instance's current time. Before any sample it is this machine's own clock, which is all
// there is — and the first refresh, which needs no estimate to be correct, supplies a sample immediately.
func (c *serverClock) now() time.Time {
	local := c.local.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ok {
		return local
	}
	return c.server.Add(local.Sub(c.sampled))
}
