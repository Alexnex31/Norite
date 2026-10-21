// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux && !darwin

package updater

import (
	"os"
	"time"
)

// changedAt is not read on the systems no release is built for, and on Windows, where nothing watches the
// program (watchReplaced).
func changedAt(os.FileInfo) (time.Time, bool) { return time.Time{}, false }
