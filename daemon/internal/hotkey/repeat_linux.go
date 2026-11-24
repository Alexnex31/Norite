// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package hotkey

import "time"

// repeatWithin is how long a release waits for the press an autorepeat sends after it: X sends a held key
// as a release and a press with one timestamp, at the keyboard's repeat rate, and a release with no press
// behind it within this is the key let go (M25: 30 ms filtered every repeat under Xvfb).
const repeatWithin = 30 * time.Millisecond

// repeats tells a key held from a key pressed again, where X reports a held key as releases and presses. It
// is fed what X says, and says when the key went down and when it went up, once each.
type repeats struct {
	down bool
	// pending is a release not yet taken as one: the time it carried, and when it came.
	pending     bool
	pendingTime uint32
}

// press is the OS saying the key went down at t, its own clock. It reports whether that is the key going
// down, rather than a repeat of one already down.
func (r *repeats) press(t uint32) bool {
	if r.pending && r.pendingTime == t {
		// The release before it was an autorepeat's.
		r.pending = false
		return false
	}
	r.pending = false
	if r.down {
		return false
	}
	r.down = true
	return true
}

// release is the OS saying the key went up at t. It is taken as one once repeatWithin passes with no
// press behind it: the caller asks settle then.
func (r *repeats) release(t uint32) {
	if r.down {
		r.pending, r.pendingTime = true, t
	}
}

// settle reports whether a release is waiting, now taken as the key going up.
func (r *repeats) settle() bool {
	if !r.pending {
		return false
	}
	r.pending, r.down = false, false
	return true
}
