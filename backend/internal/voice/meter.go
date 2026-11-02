// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"fmt"
	"runtime/debug"
	"time"
)

// meter bounds a rate over one-second windows: what fits in a window is admitted, the rest is not. A
// window that saw more than twice the bound is a strike, a clean one clears them, and enough strikes in a
// row mean the sender is not a client having a bad second.
//
// Not safe for concurrent use; each owner says what guards it.
type meter struct {
	start   time.Time
	count   int
	bytes   int
	strikes int
}

// admit counts one item of size bytes at now, against at most perSecond items and bitsPerSecond bits
// (zero for no bound on bits). It says whether the item fits, and whether the sender has struck out.
func (m *meter) admit(now time.Time, size, perSecond, bitsPerSecond, strikesOut int) (ok, out bool) {
	if gap := now.Sub(m.start); gap >= time.Second || gap < 0 {
		m.close(perSecond, bitsPerSecond)
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

func (m *meter) close(perSecond, bitsPerSecond int) {
	if m.start.IsZero() {
		return
	}
	if m.count > 2*perSecond || (bitsPerSecond > 0 && m.bytes*8 > 2*bitsPerSecond) {
		m.strikes++
	} else {
		m.strikes = 0
	}
}

// contain is deferred at the top of every goroutine and every Pion callback the package runs for a
// connection. The self-hosted SFU shares norite-server's process, so a panic in code that parses a
// stranger's packets would otherwise end REST, the gateway and messaging with it; contained, it ends that
// one connection. Pion's own goroutines are beyond a recover here, which the security ledger records.
func (c *conn) contain(where string) {
	v := recover()
	if v == nil {
		return
	}
	c.log.Error().
		Str("in", where).
		Str("panic", fmt.Sprint(v)).
		Bytes("stack", debug.Stack()).
		Msg("a voice connection panicked; closing it")
	go c.close("it panicked in " + where)
}
