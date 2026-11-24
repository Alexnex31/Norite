// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"runtime"

	"github.com/Alexnex31/Norite/daemon/internal/hotkey"
)

// The main goroutine stays on the main thread from the start, which is the one Carbon and AppKit dispatch
// on: locked in init, before main runs.
func init() { runtime.LockOSThread() }

// serve runs the daemon on a goroutine, and gives the main thread to the application loop push-to-talk's key
// needs, which starts only once a key is first wanted. The process exits with run's code.
func serve(run func() int) { hotkey.ServeMainThread(run) }
