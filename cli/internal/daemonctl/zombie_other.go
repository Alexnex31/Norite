// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix && !linux

package daemonctl

// isZombie cannot be asked cheaply away from Linux, so an exited process nobody has waited for is waited
// on for the full limit there, and the command then says it did not see the daemon exit.
func isZombie(int) bool { return false }
