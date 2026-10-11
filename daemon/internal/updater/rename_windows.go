// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package updater

import (
	"errors"
	"io/fs"
	"os"
	"strconv"
	"time"
)

// renameOver puts tmp where dst is.
//
// Windows refuses to replace or delete a program that is running, and allows it to be renamed. So the
// program in the way is moved to a name of its own first, and removed when it can be: at once if nothing
// is running it, at a later installation otherwise.
//
// The name is new each time. With one fixed name, a `norite.exe` left open across two updates still ran
// from that name at the second, which could then neither remove it nor rename over it, and every
// installation and every restore failed until that window was closed (M24 /code-review). Not run on
// Windows before M24's own pass there.
func renameOver(tmp, dst string) error {
	sweepOld(dst)
	old := dst + oldSuffix + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := os.Rename(dst, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		// Put the program back under its name, so a failed installation leaves one that runs.
		_ = os.Rename(old, dst)
		return err
	}
	sweepOld(dst)
	return nil
}
