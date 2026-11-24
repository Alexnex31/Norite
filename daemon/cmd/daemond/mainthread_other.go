// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !darwin

package main

import "os"

// serve runs the daemon on the main goroutine and exits with its code: only macOS needs the main thread for
// anything else.
func serve(run func() int) { os.Exit(run()) }
