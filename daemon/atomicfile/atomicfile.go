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

// maxLinks bounds how many symbolic links Write follows, so a loop is an error rather than a hang.
const maxLinks = 40

// Options says how the destination is treated.
type Options struct {
	// Mode is the permission a file created by this write gets. Required.
	Mode fs.FileMode

	// KeepMode keeps the permission of a file that already exists, and uses Mode only for a new one. It is
	// for files a person owns and may have chosen a mode for. A file holding a secret leaves it false, so
	// that every write puts the file back to Mode whatever it had drifted to.
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
		resolved, err := resolve(path)
		if err != nil {
			return err
		}
		target = resolved
	}

	mode := opt.Mode
	if opt.KeepMode {
		if info, err := os.Stat(target); err == nil {
			mode = info.Mode().Perm()
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
		return fmt.Errorf("%s was replaced, but its directory could not be flushed, so the change may not "+
			"survive a crash: %w", target, err)
	}
	return nil
}

// resolve follows path through any symbolic links to the file a write should replace.
//
// It differs from filepath.EvalSymlinks in one way that matters: a link whose target does not exist yet
// resolves to that target, so the first write through a dangling link creates the file it names.
func resolve(path string) (string, error) {
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
			next = filepath.Join(filepath.Dir(current), next)
		}
		current = next
	}
	return "", fmt.Errorf("resolving %s: more than %d symbolic links, which is probably a loop", path, maxLinks)
}

// Resolve reports the file a write to path with FollowSymlink would replace. A watcher needs it: the
// directory to watch is the real file's, not the link's.
func Resolve(path string) (string, error) { return resolve(path) }
