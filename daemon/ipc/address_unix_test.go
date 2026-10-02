// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package ipc

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shortDir is a directory whose socket path fits: t.TempDir under a long TMPDIR can exceed the limit,
// which is the case CheckSocketPath exists for and not the one these tests are about.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "nipc")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestNoSocketMeansNoDaemon(t *testing.T) {
	_, err := DialAt(context.Background(), SocketPath(shortDir(t)))
	assert.ErrorIs(t, err, ErrNotRunning)
}

// TestASocketNobodyAcceptsOnMeansNoDaemon is what a daemon that crashed leaves behind until the next one
// removes it.
func TestASocketNobodyAcceptsOnMeansNoDaemon(t *testing.T) {
	path := SocketPath(shortDir(t))
	l, err := net.Listen("unix", path)
	require.NoError(t, err)
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, l.Close())
	_, statErr := os.Stat(path)
	require.NoError(t, statErr, "the socket file is still there")

	_, err = DialAt(context.Background(), path)
	assert.ErrorIs(t, err, ErrNotRunning)
}

func TestASocketPathTooLongIsRefusedNamingTheLimit(t *testing.T) {
	long := "/" + strings.Repeat("d", maxSocketPath())
	err := CheckSocketPath(long)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "XDG_STATE_HOME")

	fits := filepath.Join("/", strings.Repeat("d", maxSocketPath()-1))
	assert.NoError(t, CheckSocketPath(fits))
}

// TestTheLimitIsThePlatformsOwn binds at the limit and one byte past it, so the constant is checked against
// the kernel rather than against a comment.
func TestTheLimitIsThePlatformsOwn(t *testing.T) {
	dir := shortDir(t)
	at := func(n int) string {
		pad := n - len(dir) - 1
		require.Positive(t, pad, "the temporary directory is too long to build the path")
		return filepath.Join(dir, strings.Repeat("s", pad))
	}

	fits := at(maxSocketPath())
	l, err := net.Listen("unix", fits)
	require.NoError(t, err, "a path of exactly the limit must bind")
	_ = l.Close()

	_, err = net.Listen("unix", at(maxSocketPath()+1))
	assert.Error(t, err, "one byte past the limit must not")
}
