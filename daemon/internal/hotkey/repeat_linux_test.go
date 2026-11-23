// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package hotkey

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A held key comes as a press, then releases and presses sharing one time, at the keyboard's repeat rate:
// one press and one release reach the call. A release with no press behind it is the key let go.
func TestAHeldKeyIsOnePressAndOneRelease(t *testing.T) {
	var r repeats
	assert.True(t, r.press(100))
	for t0 := uint32(600); t0 < 2000; t0 += 33 {
		r.release(t0)
		assert.False(t, r.press(t0), "an autorepeat at %d read as a press", t0)
	}
	assert.False(t, r.settle(), "nothing was waiting to settle")
	r.release(2100)
	assert.True(t, r.settle(), "the release was not the key let go")
	assert.False(t, r.settle())
	// Pressed again later, it goes down again.
	assert.True(t, r.press(2500))
	// A release before any press is nothing.
	var fresh repeats
	fresh.release(10)
	assert.False(t, fresh.settle())
}
