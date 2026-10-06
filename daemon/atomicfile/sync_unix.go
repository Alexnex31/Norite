// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package atomicfile

import (
	"errors"
	"os"
	"syscall"
)

// syncDir flushes a directory, which is what makes a rename inside it durable. A variable so a test can
// see that Write calls it, and when.
var syncDir = func(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // the directory of a path the caller chose to write
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	// Some filesystems cannot flush a directory and say so with one of these. There is nothing more to
	// do there, and failing a write that has already happened would report the wrong thing.
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}

// replace renames tmp over target. On these systems a rename replaces an existing file in one step.
func replace(tmp, target string) error { return os.Rename(tmp, target) }
