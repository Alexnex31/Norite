// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux

package audio

import (
	"context"
	"io"
)

// System has no backend here yet: WASAPI on Windows and AudioQueue on macOS are M29's, and every other OS
// stays without one.
func System() Device { return unsupported{} }

type unsupported struct{}

func (unsupported) Capture(context.Context, func([]int16)) (io.Closer, error) {
	return nil, ErrUnsupported
}
func (unsupported) Play(context.Context, func([]int16)) (io.Closer, error) {
	return nil, ErrUnsupported
}
