// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package voiceworker

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// On Windows the same was walked in a VM (M28's pass): the worker went 147 ms after its daemon was
// killed, its pipes being inherited handles.
// A daemon killed outright takes its worker with it: the worker sees its control pipe close, whoever
// closed it.
func TestAWorkerDoesNotOutliveItsDaemon(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	parent := exec.Command(exe) //nolint:gosec // this test binary
	parent.Env = append(os.Environ(), standinVariable+"=parent")
	out, err := parent.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, parent.Start())
	line, err := bufio.NewReader(out).ReadString('\n')
	require.NoError(t, err)
	pidText, ok := strings.CutPrefix(strings.TrimSpace(line), "worker ")
	require.True(t, ok, "%q", line)
	pid, err := strconv.Atoi(pidText)
	require.NoError(t, err)
	require.NoError(t, syscall.Kill(pid, 0), "the worker is running")

	require.NoError(t, parent.Process.Signal(syscall.SIGKILL))
	_ = parent.Wait()
	require.Eventually(t, func() bool { return syscall.Kill(pid, 0) != nil }, 3*time.Second, 10*time.Millisecond,
		"the worker outlived its daemon")
}
