// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package wasmrt is what every wazero runtime the worker makes goes through first.
//
// wazero v1.12.0 caches its own version in a package variable the first time a compiling runtime is made,
// with no lock (internal/version.GetWazeroVersion), so two runtimes made at once on two goroutines race on
// it. A call makes two from M30, the codec's and the DSP's, compiled together while ICE connects, and
// -race found the race the first time they were. Warm makes one runtime once, before any other, so the
// cache is filled before anything runs concurrently; internal/opus and internal/dsp call it before each
// runtime they make, and the compiles themselves still overlap.
package wasmrt

import (
	"context"
	"sync"

	"github.com/tetratelabs/wazero"
)

// Warm fills wazero's version cache, once per process.
var Warm = sync.OnceFunc(func() {
	ctx := context.Background()
	_ = wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig()).Close(ctx)
})
