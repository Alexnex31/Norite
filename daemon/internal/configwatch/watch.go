// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package configwatch notices that the user's config.toml changed.
//
// It is the daemon's and nobody else's, which is why it is internal and not part of daemon/config: the
// CLI reads and edits that file and never watches it, and keeping the watcher here keeps fsnotify out of
// the CLI's binary.
package configwatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/Alexnex31/Norite/daemon/atomicfile"
)

// watchSettle coalesces a burst of events into one notification. A save is rarely one event: an atomic
// write is a create and a rename, and vim writes a backup, renames, writes and changes the mode.
const watchSettle = 100 * time.Millisecond

// settleAfter starts the wait that ends a burst. A variable so that a test can end the burst itself: how
// many writes fit in a tenth of a second is a fact about the machine, and a test that assumed ten did failed
// on a CI runner, where they took three windows.
var settleAfter = func() <-chan time.Time { return time.After(watchSettle) }

// dirMode is what the config directory is created with when it does not exist yet, as daemon/config
// creates it.
const dirMode = 0o700

// Watch calls changed whenever one of the config files at paths may have changed, until ctx is done. They
// are config.toml and, for the same-machine toggle, the file each client reads while it is on; all of
// them are in one directory, the first one's, which is the one created when it is missing.
//
// Directories are watched, never the files. Nearly every editor and this package's own writer save by
// renaming a new file into place, and a watch on the old file follows it into oblivion. More directories
// when a config is a link into a dotfiles repository: the one the link is in, where it can be replaced
// or re-pointed, and the one the real file is in, which is where a save actually lands.
//
// changed is told that something happened, not what. It runs on Watch's goroutine and must not block.
// A notification for a file that turns out not to have changed is harmless and expected: reading a config
// costs microseconds, and guessing wrong in the other direction is a setting that silently did not apply.
//
// It returns an error only when no watch could be set up at all; the caller logs that and carries on, and
// a client then picks a change up when it next starts.
func Watch(ctx context.Context, paths []string, changed func()) error {
	if len(paths) == 0 {
		return errors.New("configwatch: no file to watch")
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()

	// The directory has to exist to be watched, and a config that does not exist yet is the ordinary
	// state of a new install. Creating it is the one thing a daemon does to this directory, and it is done
	// once: a directory that is later removed is waited for, not put back under whoever is replacing it.
	dir := filepath.Dir(paths[0])
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	if err := w.Add(dir); err != nil {
		return err
	}
	// And the directory the config's directory is in. A watch is on an inode, not a name: when
	// ~/.config/norite is removed and restored, or is a link a dotfiles manager re-points, the watch above
	// is on a directory nothing uses any more, and every later save is silence until the daemon restarts
	// (M21 /code-review). The parent sees the name change hands. Without it the watch is as good as it was.
	parent := filepath.Dir(dir)
	if parent != dir {
		_ = w.Add(parent)
	}

	// The names themselves, which is where a link is replaced or re-pointed.
	links := make(map[string]bool, len(paths))
	for _, path := range paths {
		links[filepath.Clean(path)] = true
	}
	// The real files, where they are somewhere else, and their directories. Looked up again after every
	// change, since the change may have been a link being pointed at another file.
	reals, targets := map[string]bool{}, map[string]bool{}
	follow := func() {
		nextReals, nextTargets := map[string]bool{}, map[string]bool{}
		for _, path := range paths {
			resolved, err := atomicfile.Resolve(path)
			if err != nil {
				continue
			}
			real := filepath.Clean(resolved)
			nextReals[real] = true
			// Not the two watched already. A config that is a link to a file one directory up has its real
			// file in the parent, and counting that as a target meant taking the watch off the parent the
			// day the link was pointed elsewhere, which is the watch that notices the directory itself
			// being replaced (M21 /code-review).
			if d := filepath.Dir(real); d != dir && d != parent {
				nextTargets[d] = true
			}
		}
		for d := range targets {
			if !nextTargets[d] {
				_ = w.Remove(d)
			}
		}
		for d := range nextTargets {
			if !targets[d] && w.Add(d) != nil {
				delete(nextTargets, d)
			}
		}
		reals, targets = nextReals, nextTargets
	}
	follow()

	var settle <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			switch name := filepath.Clean(ev.Name); {
			case name == dir:
				// The directory itself was created, removed, renamed or replaced. Watch whatever the name
				// leads to now; if that is nothing, the parent says when it is something again.
				_ = w.Remove(dir)
				_ = w.Add(dir)
			case links[name], reals[name]:
				// A link itself, or the file one resolves to. Everything else in these directories is
				// somebody else's file.
			default:
				continue
			}
			if settle == nil {
				settle = settleAfter()
			}
		case _, ok := <-w.Errors:
			if !ok {
				return nil
			}
			// An overflowed queue means events were dropped, and one may have been the config. Say it
			// changed rather than guess that it did not.
			if settle == nil {
				settle = settleAfter()
			}
		case <-settle:
			settle = nil
			follow()
			changed()
		}
	}
}
