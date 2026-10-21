// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"os"
	"syscall"
	"time"
)

// changedAt is when the file's inode last changed, which the kernel sets at its creation and at a rename,
// and which nothing writing a file can set.
func changedAt(fi os.FileInfo) (time.Time, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(st.Ctimespec.Sec, st.Ctimespec.Nsec), true
}
