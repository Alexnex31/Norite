// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !unix && !windows

package main

import "errors"

// startAgain cannot be done here. The caller exits with exitRestart, for whatever runs the daemon.
func startAgain(string) error {
	return errors.New("this platform has no way to start the installed version from the running one")
}
