// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !unix

package main

// startAgain does nothing where a process cannot replace itself. The caller exits with exitRestart, and
// the Windows task's restart-on-failure starts the program now on disk.
func startAgain(string) error { return nil }
