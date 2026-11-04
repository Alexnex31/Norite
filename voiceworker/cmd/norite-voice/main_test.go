// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func build(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "norite-voice")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput() //nolint:gosec // fixed arguments
	require.NoError(t, err, "%s", out)
	return bin
}

// The updater smoke-runs what it stages with -version and an empty environment, as it does the daemon,
// and installs nothing that answers otherwise.
func TestVersionAnswersWithNothingInItsEnvironment(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the worker")
	}
	cmd := exec.Command(build(t), "-version") //nolint:gosec // the binary just built
	cmd.Env = []string{}
	out, err := cmd.Output()
	require.NoError(t, err)
	require.Equal(t, "dev\n", string(out))
}

// Started by hand, it says what it is for and exits as a usage error, rather than waiting on pipes nobody
// gave it.
func TestStartedByHandItSaysWhoStartsIt(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the worker")
	}
	bin := build(t)
	for _, args := range [][]string{nil, {"-pipes", "3"}, {"-pipes", "3,3"}, {"-pipes", "3,4", "extra"}} {
		out, err := exec.Command(bin, args...).CombinedOutput() //nolint:gosec // the binary just built
		var exit *exec.ExitError
		require.True(t, errors.As(err, &exit), "%v: %v", args, err)
		require.Equal(t, exitUsage, exit.ExitCode(), "%v", args)
		require.NotEmpty(t, strings.TrimSpace(string(out)))
	}
	out, _ := exec.Command(bin).CombinedOutput() //nolint:gosec // the binary just built
	require.Contains(t, string(out), "started by norite-daemon")
}
