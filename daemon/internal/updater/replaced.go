// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"context"
	"os"
	"time"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// replacedEvery is how often a packaged daemon looks at its own program. A variable so the tests can
// shorten it.
var replacedEvery = time.Minute

// watchReplaced starts the version a package manager has just installed over this program, until ctx ends
// (ADR 0035 §4).
//
// A package manager replaces the file and leaves the process alone: it runs as root and cannot reach a
// user's service, so after `apt upgrade` or `brew upgrade` the running daemon stays the old version until
// the next login, and goes on saying a release is out that is already installed. So a daemon a package
// manager installed watches the path it was started by, and when another program is there, one that runs
// and reports a different version, it ends its run the way an update does and starts that program in its
// place (cmd/daemond: exec, keeping the process id).
//
// Only a packaged daemon watches. One the updater installs is restarted by the updater; anything else, a
// development build above all, is replaced by somebody who did not ask for it to restart. Not on Windows,
// where a running program's file cannot be replaced in place.
//
// A file changed half-way through a package manager's write either is not the same file yet or does not run
// and say a version; both are looked at again a minute later. A reinstall of the same version is noted and
// not restarted for. A voice session puts the restart off, as it puts off an update (architecture.md §6).
func (u *Updater) watchReplaced(ctx context.Context) {
	seen := u.program
	if seen == nil || u.installs || u.opts.GOOS == "windows" || u.current == (gatewayproto.Version{}) ||
		packageManager(u.opts.Executable, u.opts.GOOS) == "" {
		return
	}
	tick := time.NewTicker(replacedEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now, err := os.Stat(u.opts.Program)
		if err != nil || sameProgram(seen, now) {
			continue
		}
		said, err := reportedVersion(ctx, u.opts.Program)
		if err != nil {
			continue
		}
		version, err := gatewayproto.ParseVersion(said)
		if err != nil {
			continue
		}
		if version == u.current {
			seen = now
			continue
		}
		if u.opts.Busy != nil && u.opts.Busy() {
			continue
		}
		u.mu.Lock()
		u.replaced = version.String()
		u.mu.Unlock()
		u.opts.Log.Info().Str("from", u.current.String()).Str("to", termsafe.Text(version.String())).
			Msg("the package manager installed another version over this program; starting it")
		u.opts.Restart()
		return
	}
}

// sameProgram is whether a look at the program's path found the file looked at last time.
//
// The inode number alone cannot say. A package manager renames a new file over the old one, the old inode
// is freed, and ext4 gives its number to the next file created in the directory: two upgrades apart, a new
// program sits under a number already seen. CI found it, a reinstall of the same version followed by an
// upgrade, which the watch then missed until the next start. A new file also has its own change time, which
// the kernel sets and a package cannot carry over, and usually its own modification time and size.
func sameProgram(a, b os.FileInfo) bool {
	if !os.SameFile(a, b) || !a.ModTime().Equal(b.ModTime()) || a.Size() != b.Size() {
		return false
	}
	ac, aok := changedAt(a)
	bc, bok := changedAt(b)
	return aok == bok && ac.Equal(bc)
}
