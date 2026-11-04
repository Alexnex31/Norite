// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package voiceworker

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"github.com/Alexnex31/Norite/daemon/voiceipc"
)

// inherit hands the worker its two pipes as descriptors 3 and 4, after stdin, stdout and stderr, and
// returns the -pipes value naming them. ExtraFiles are the one exception to close-on-exec it makes.
func inherit(cmd *exec.Cmd, read, write *os.File) (string, error) {
	closeInherited()
	cmd.ExtraFiles = []*os.File{read, write}
	return voiceipc.FormatPipes(3, 4), nil
}

// closeInherited marks every descriptor from 3 up close-on-exec, so nothing but its pipes reaches the
// worker. Go opens each of its own that way, and leaves alone what the daemon inherited from whatever
// started it: a shell's, a terminal's, a CI runner's. Those stay open in the daemon and crossed to every
// child it started. GitHub's runner hands two such pipes down, which is how they were found. Marking one
// changes nothing for the daemon, since only exec reads the flag, and ExtraFiles are put in place in the
// child whatever the flag on the original. Run before each start rather than once, since a library can
// open a descriptor without the flag at any time.
//
// /dev/fd lists them on Linux and macOS. Where it cannot be read, nothing is marked.
func closeInherited() {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return
	}
	for _, e := range entries {
		fd, err := strconv.Atoi(e.Name())
		if err != nil || fd < 3 {
			continue
		}
		syscall.CloseOnExec(fd)
	}
}
