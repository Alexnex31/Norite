// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package backoff is the daemon's one retry policy: exponential with equal jitter.
//
// One copy, because the session's refresh retries and the gateway's reconnects are the same policy and a
// fix to one — an overflow guard, a jitter change — must not silently miss the other (M19 /code-review).
package backoff

import (
	"math/rand/v2"
	"time"
)

// Backoff doubles from Min to Max. Half of each delay is fixed and half random, so a fleet of daemons
// retrying together after a rollout spreads out without any delay collapsing to nothing. Not safe for
// concurrent use; each retry loop owns one.
type Backoff struct {
	Min, Max time.Duration
	cur      time.Duration
}

// Next returns the next delay.
func (b *Backoff) Next() time.Duration {
	switch {
	case b.cur == 0:
		b.cur = b.Min
	case b.cur < b.Max:
		b.cur = min(b.cur*2, b.Max)
	}
	half := b.cur / 2
	return half + rand.N(half+1) //nolint:gosec // jitter, not a secret
}

// Reset starts again from Min.
func (b *Backoff) Reset() { b.cur = 0 }
