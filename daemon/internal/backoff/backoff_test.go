// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package backoff

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestItGrowsToItsCapAndKeepsAFloor(t *testing.T) {
	b := &Backoff{Min: time.Second, Max: 8 * time.Second}
	for _, ceiling := range []time.Duration{1, 2, 4, 8, 8} {
		d := b.Next()
		assert.GreaterOrEqual(t, d, ceiling*time.Second/2)
		assert.LessOrEqual(t, d, ceiling*time.Second)
	}
	b.Reset()
	assert.LessOrEqual(t, b.Next(), time.Second)
}
