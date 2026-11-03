// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package voiceworker

import (
	"os"
	"os/exec"

	"github.com/Alexnex31/Norite/daemon/voiceipc"
)

// inherit hands the worker its two pipes as descriptors 3 and 4, after stdin, stdout and stderr, and
// returns the -pipes value naming them. Nothing else the daemon holds is inherited: Go opens every
// descriptor close-on-exec, and ExtraFiles are the exception it makes on purpose.
func inherit(cmd *exec.Cmd, read, write *os.File) (string, error) {
	cmd.ExtraFiles = []*os.File{read, write}
	return voiceipc.FormatPipes(3, 4), nil
}
