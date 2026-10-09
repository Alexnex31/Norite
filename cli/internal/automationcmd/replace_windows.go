// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package automationcmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/urfave/cli/v3"
)

// replaceProcess runs the program and waits for it, since Windows cannot replace a process. Its exit code
// is this command's. A signal sent to this process does not reach the program, which is the difference
// from every other platform and is why the help says "takes this command's place" only loosely here.
func replaceProcess(path string, argv, env []string) error {
	cmd := exec.Command(path, argv[1:]...) //nolint:gosec // the program the caller named
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return cli.Exit("", exit.ExitCode())
	}
	if err != nil {
		return fmt.Errorf("cannot start %s: %w", path, err)
	}
	return nil
}
