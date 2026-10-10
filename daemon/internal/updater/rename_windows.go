// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package updater

import (
	"errors"
	"io/fs"
	"os"
)

// renameOver puts tmp where dst is.
//
// Windows refuses to replace or delete a program that is running, and allows it to be renamed. So the
// program in the way is moved to a name of its own first, and removed when it can be: at once if nothing
// is running it, at the next installation otherwise. Not run on Windows before M24's own pass there.
func renameOver(tmp, dst string) error {
	old := dst + ".old"
	_ = os.Remove(old)
	if err := os.Rename(dst, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		// Put the program back under its name, so a failed installation leaves one that runs.
		_ = os.Rename(old, dst)
		return err
	}
	_ = os.Remove(old)
	return nil
}
