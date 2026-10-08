// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package atomicfile replaces a file's contents in one step, durably, or leaves them untouched.
//
// It is the one implementation of "temp file plus rename" in the repository. There were two, the instance
// wizard's and the credential store's, and both stopped at the rename: neither flushed the directory, so
// after a crash the old name could still be the one on disk. Every writer of client or instance state goes
// through Write, which is why it lives in the daemon module, outside internal/, where the CLI can reach it.
package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrNotDurable reports a write that happened but whose directory could not be flushed: the file holds
// the new contents, and whether the rename survives a crash is up to the filesystem. It is an error
// because "written" and "written durably" are different facts, and it is distinguishable because a caller
// that treats it as "nothing was written" will then act on a file that is not what it believes: delete a
// config that is complete, or keep presenting a token the store no longer holds.
var ErrNotDurable = errors.New("the file was replaced, but its directory could not be flushed, " +
	"so the change may not survive a crash")

// ErrReadOnly reports a destination its owner has made read-only, under KeepMode.
var ErrReadOnly = errors.New("is read-only; make it writable to let Norite change it")

// maxLinks bounds how many symbolic links Write follows, so a loop is an error rather than a hang.
const maxLinks = 40

// Options says how the destination is treated.
type Options struct {
	// Mode is the permission a file created by this write gets. Required.
	Mode fs.FileMode

	// KeepMode keeps the permission of a file that already exists, and uses Mode only for a new one. It is
	// for files a person owns and may have chosen a mode for, and it honors the mode as well as copying
	// it: a file with no owner-write bit is refused with ErrReadOnly, since a rename would otherwise
	// replace what its owner locked (and on Windows fails, so the platforms would disagree). A file
	// holding a secret leaves KeepMode false, so that every write puts the file back to Mode whatever it
	// had drifted to.
	KeepMode bool

	// FollowSymlink writes the file a symbolic link points at, and leaves the link in place. Without it
	// the rename replaces the link itself with a regular file, which is right for a file nobody is meant
	// to link and silently detaches a config kept in a dotfiles repository.
	FollowSymlink bool

	// Before, when set, runs after the new contents are safely on disk and immediately ahead of the
	// rename. An error from it abandons the write with the destination untouched. It is where a caller
	// checks that the file is still the one it read.
	Before func() error
}

// Write replaces path's contents with data.
//
// Readers see the old file or the complete new one, never a partial write: the data goes to a temporary
// file in the destination's own directory, is flushed, and is then renamed over the destination. The
// directory is flushed afterwards, which is what makes the rename itself survive a crash.
func Write(path string, data []byte, opt Options) error {
	if opt.Mode == 0 {
		return errors.New("atomicfile: Options.Mode is required")
	}

	target := path
	if opt.FollowSymlink {
		resolved, err := Resolve(path)
		if err != nil {
			return err
		}
		target = resolved
	}

	mode := opt.Mode
	if opt.KeepMode {
		if info, err := os.Stat(target); err == nil {
			mode = info.Mode().Perm()
			if mode&0o200 == 0 {
				return fmt.Errorf("%s %w", target, ErrReadOnly)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("reading the permissions of %s: %w", target, err)
		}
	}

	// The same directory, so the rename stays inside one filesystem. Across a mount boundary it would
	// become a copy, which is not atomic.
	dir, name := filepath.Split(target)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return fmt.Errorf("writing %s: creating a temporary file: %w", target, err)
	}
	tmpName := tmp.Name()
	// On every path out. Once the rename has succeeded there is nothing left under this name.
	defer func() { _ = os.Remove(tmpName) }()

	// Before the first byte, so the contents are never readable more widely than the destination will be.
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: setting permissions %#o: %w", target, mode, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", target, err)
	}
	// Flushed before the rename publishes the name. Without it a crash just after the rename can leave
	// the new name pointing at an empty file.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: flushing: %w", target, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", target, err)
	}

	if opt.Before != nil {
		if err := opt.Before(); err != nil {
			return err
		}
	}

	if err := replace(tmpName, target); err != nil {
		return fmt.Errorf("writing %s: %w", target, err)
	}
	// The rename changed the directory, and until the directory is flushed the change is only in memory.
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("%s: %w: %w", target, ErrNotDurable, err)
	}
	return nil
}

// Resolve follows path through any symbolic links to the file a write with FollowSymlink replaces. A
// watcher needs it too: the directory to watch is the real file's, not the link's.
//
// It differs from filepath.EvalSymlinks in one way that matters: a link whose target does not exist yet
// resolves to that target, so the first write through a dangling link creates the file it names.
func Resolve(path string) (string, error) {
	current := path
	for range maxLinks {
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return current, nil
		}
		if err != nil {
			return "", fmt.Errorf("resolving %s: %w", path, err)
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			return current, nil
		}
		next, err := os.Readlink(current)
		if err != nil {
			return "", fmt.Errorf("resolving %s: %w", path, err)
		}
		if !filepath.IsAbs(next) {
			// Relative to the directory the link is really in. Joining onto the path as written would
			// collapse a ".." before the directory's own link had been followed, and land beside the
			// link instead of beside its target, in a file the operating system never reads.
			dir := filepath.Dir(current)
			if real, err := filepath.EvalSymlinks(dir); err == nil {
				dir = real
			}
			next = filepath.Join(dir, next)
		}
		current = next
	}
	return "", fmt.Errorf("resolving %s: more than %d symbolic links, which is probably a loop", path, maxLinks)
}
