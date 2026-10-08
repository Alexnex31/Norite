// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package automation

import (
	"sync"
	"time"
)

// bucket is a token bucket: burst tokens at most, refilled at rate a second. It refuses rather than waits,
// so a script that asks too fast is told at once and a connection is never held to pace it.
type bucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

func newBucket(rate float64, burst int, now func() time.Time) *bucket {
	return &bucket{rate: rate, burst: float64(burst), tokens: float64(burst), last: now(), now: now}
}

// take spends one token, or reports that there is none.
func (b *bucket) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	// A clock that went backwards refills nothing, and is taken as the new start.
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = min(b.burst, b.tokens+elapsed*b.rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
