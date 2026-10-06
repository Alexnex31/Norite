// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package atomicfile

import (
	"os"
	"time"
)

// syncDir does nothing on Windows, which has no way to flush a directory. The rename there is durable
// when the filesystem's own journal says it is; this package cannot add to that and does not pretend to.
var syncDir = func(string) error { return nil }

// replace renames tmp over target, retrying briefly.
//
// Windows refuses to replace a file another process has open, where other systems allow it. A reader
// that opens the file for a moment (an editor, a virus scanner, this program's own loader in another
// process) would otherwise fail a write that succeeds a few milliseconds later.
func replace(tmp, target string) error {
	var err error
	for attempt := range 5 {
		if err = os.Rename(tmp, target); err == nil {
			return nil
		}
		time.Sleep(time.Duration(10*(attempt+1)) * time.Millisecond)
	}
	return err
}
