// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceworker

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// On Linux the worker holds descriptors 0 to 4 and nothing else the daemon had open.
func TestTheWorkerInheritsOnlyItsPipes(t *testing.T) {
	fast(t)
	held, err := os.Open(os.Args[0]) // something the daemon holds, which must not cross
	require.NoError(t, err)
	defer func() { _ = held.Close() }()
	// And something it inherited, as a daemon started from a shell or by CI's runner does: dup gives a
	// descriptor without close-on-exec, which Go never opens by itself.
	stray, err := syscall.Dup(int(held.Fd()))
	require.NoError(t, err)
	defer func() { _ = syscall.Close(stray) }()
	log := &logBuffer{}
	w := joinStandin(t, "fds", log)
	w.Leave()
	var fds []string
	require.Eventually(t, func() bool {
		for _, l := range log.workerLines() {
			if list, ok := strings.CutPrefix(l, "fds "); ok {
				fds = strings.Fields(list)
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond)
	var numbers []string
	for _, fd := range fds {
		n, target, _ := strings.Cut(fd, "=")
		numbers = append(numbers, n)
		require.NotEqual(t, held.Name(), target, "a file the daemon holds crossed to the worker")
	}
	require.Equal(t, []string{"0", "1", "2", "3", "4"}, numbers, "%v", fds)
}
