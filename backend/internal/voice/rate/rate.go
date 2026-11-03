// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package rate bounds what a stranger sends, over one-second windows, for the SFU and for TURN alike.
package rate

import "time"

// Meter bounds a rate over one-second windows: what fits in a window is admitted, the rest is not. A
// window that saw more than twice the bound is a strike, a clean one clears them, and enough strikes in a
// row mean the sender is not a client having a bad second.
//
// Not safe for concurrent use; each owner says what guards it.
type Meter struct {
	start   time.Time
	count   int
	bytes   int
	strikes int
}

// Admit counts one item of size bytes at now, against at most perSecond items and bitsPerSecond bits
// (zero for no bound on bits). It says whether the item fits, and whether the sender has struck out.
func (m *Meter) Admit(now time.Time, size, perSecond, bitsPerSecond, strikesOut int) (ok, out bool) {
	if gap := now.Sub(m.start); gap >= time.Second || gap < 0 {
		m.closeWindow(perSecond, bitsPerSecond)
		if gap >= 2*time.Second {
			// A second or more with nothing in it: whatever came before is not a run.
			m.strikes = 0
		}
		m.start, m.count, m.bytes = now, 0, 0
	}
	m.count++
	m.bytes += size
	ok = m.count <= perSecond && (bitsPerSecond == 0 || m.bytes*8 <= bitsPerSecond)
	return ok, m.strikes >= strikesOut
}

func (m *Meter) closeWindow(perSecond, bitsPerSecond int) {
	if m.start.IsZero() {
		return
	}
	if m.count > 2*perSecond || (bitsPerSecond > 0 && m.bytes*8 > 2*bitsPerSecond) {
		m.strikes++
	} else {
		m.strikes = 0
	}
}
