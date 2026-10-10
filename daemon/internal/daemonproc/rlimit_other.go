// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !unix

package daemonproc

// openFileLimit is 0 away from Unix: Windows has no RLIMIT_NOFILE, and its per-process handle table is
// large enough that nothing analogous is worth reporting.
func openFileLimit() uint64 { return 0 }
