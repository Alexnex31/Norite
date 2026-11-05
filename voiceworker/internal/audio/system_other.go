// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux && !windows && !darwin

package audio

import (
	"context"
	"io"
)

// System has no backend on any OS but Linux, Windows and macOS.
func System() Device { return unsupported{} }

type unsupported struct{}

func (unsupported) Capture(context.Context, func([]int16)) (io.Closer, error) {
	return nil, ErrUnsupported
}
func (unsupported) Play(context.Context, func([]int16)) (io.Closer, error) {
	return nil, ErrUnsupported
}
