// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package main

// leaveOwnConsole does nothing away from Windows, where starting a program opens no window.
func leaveOwnConsole() bool { return false }
