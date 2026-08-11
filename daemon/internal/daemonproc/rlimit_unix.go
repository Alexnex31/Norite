//go:build unix

package daemonproc

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// wantedFileLimit is the soft RLIMIT_NOFILE the daemon asks for at startup.
//
// The daemon is effectively a local server: it simultaneously holds the gateway WebSocket, one socket per
// attached CLI/GUI client, the bot-automation TCP listener, the voice-worker pipes, and SQLite plus log
// files (docs/architecture.md §3). macOS still defaults the soft limit to 256, which normal multi-client,
// active-voice use passes without anything being wrong. 4096 is the figure the architecture names.
const wantedFileLimit uint64 = 4096

// raiseFileLimit raises the soft RLIMIT_NOFILE toward wantedFileLimit, up to whatever the hard limit allows.
//
// Done before any handle is opened, so the process never hits the low default in the middle of accepting a
// client. Raising the *soft* limit is unprivileged as long as it stays under the hard limit, so this needs
// no elevation and no capability.
//
// Returns the resulting soft limit. A hard limit below what we asked for is not an error: the daemon runs
// fine at a lower ceiling until it genuinely runs out, and refusing to start over a file-descriptor budget
// would be a self-inflicted outage. The caller logs the outcome so a later "too many open files" has an
// obvious first thing to look at.
func raiseFileLimit() (uint64, error) {
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
		return 0, fmt.Errorf("reading RLIMIT_NOFILE: %w", err)
	}

	if lim.Cur >= wantedFileLimit {
		return lim.Cur, nil
	}

	target := wantedFileLimit
	if lim.Max < target {
		target = lim.Max
	}
	if target == lim.Cur {
		return lim.Cur, nil
	}

	raised := lim
	raised.Cur = target
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &raised); err != nil {
		return lim.Cur, fmt.Errorf("raising RLIMIT_NOFILE to %d: %w", target, err)
	}
	return target, nil
}
