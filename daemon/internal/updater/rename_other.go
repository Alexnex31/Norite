// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package updater

import "os"

// renameOver puts tmp where dst is. A rename replaces a running program's name and leaves the running
// copy alone, which is the whole of what is needed here.
func renameOver(tmp, dst string) error { return os.Rename(tmp, dst) }
