// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteReplacesTheContentsAndLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))

	require.NoError(t, Write(path, []byte("new"), Options{Mode: 0o600}))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new", string(got))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "the temporary file must not outlive the write")
}

// The directory flush is the step both earlier writers lacked. It must happen, and after the rename:
// flushing first would flush a directory that does not yet hold the change.
func TestWriteFlushesTheDirectoryAfterTheRename(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot flush a directory")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "file")

	real := syncDir
	t.Cleanup(func() { syncDir = real })
	var flushed []string
	var sawNewContents bool
	syncDir = func(d string) error {
		flushed = append(flushed, d)
		got, _ := os.ReadFile(path)
		sawNewContents = string(got) == "new"
		return real(d)
	}

	require.NoError(t, Write(path, []byte("new"), Options{Mode: 0o600}))
	require.Len(t, flushed, 1, "the directory must be flushed exactly once")
	assert.Equal(t, filepath.Clean(dir), filepath.Clean(flushed[0]))
	assert.True(t, sawNewContents, "the directory was flushed before the rename had happened")
}

func TestAFailedDirectoryFlushSaysTheFileWasReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	real := syncDir
	t.Cleanup(func() { syncDir = real })
	syncDir = func(string) error { return errors.New("no") }

	err := Write(path, []byte("new"), Options{Mode: 0o600})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "was replaced")
	got, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, "new", string(got))
}

func TestModeIsForcedUnlessKept(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()

	secret := filepath.Join(dir, "secret")
	require.NoError(t, os.WriteFile(secret, []byte("x"), 0o644))
	require.NoError(t, os.Chmod(secret, 0o644))
	require.NoError(t, Write(secret, []byte("y"), Options{Mode: 0o600}))
	assertMode(t, secret, 0o600, "a secret-bearing file goes back to its mode on every write")

	owned := filepath.Join(dir, "owned")
	require.NoError(t, os.WriteFile(owned, []byte("x"), 0o644))
	require.NoError(t, os.Chmod(owned, 0o644))
	require.NoError(t, Write(owned, []byte("y"), Options{Mode: 0o600, KeepMode: true}))
	assertMode(t, owned, 0o644, "a file its owner chose a mode for keeps it")

	fresh := filepath.Join(dir, "fresh")
	require.NoError(t, Write(fresh, []byte("y"), Options{Mode: 0o600, KeepMode: true}))
	assertMode(t, fresh, 0o600, "a new file gets Mode even when KeepMode is set")
}

// A config kept in a dotfiles repository is a link. Renaming over the link would leave the repository's
// copy stale for ever, with nothing to say so.
func TestFollowSymlinkWritesTheTargetAndKeepsTheLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symbolic link needs a privilege on Windows")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "dotfiles")
	require.NoError(t, os.Mkdir(repo, 0o700))
	target := filepath.Join(repo, "config.toml")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o600))
	link := filepath.Join(dir, "config.toml")
	require.NoError(t, os.Symlink(filepath.Join("dotfiles", "config.toml"), link)) // relative, as stow makes

	require.NoError(t, Write(link, []byte("new"), Options{Mode: 0o600, FollowSymlink: true}))

	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "the link was replaced by a regular file")
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "new", string(got), "the file the link points at must be the one written")
}

func TestWithoutFollowSymlinkTheLinkIsReplaced(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symbolic link needs a privilege on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o600))
	link := filepath.Join(dir, "token")
	require.NoError(t, os.Symlink(target, link))

	require.NoError(t, Write(link, []byte("new"), Options{Mode: 0o600}))

	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.Zero(t, info.Mode()&os.ModeSymlink, "a secret must never be written through a link somebody planted")
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got))
}

func TestADanglingLinkIsWrittenThrough(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symbolic link needs a privilege on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "not-yet")
	link := filepath.Join(dir, "config.toml")
	require.NoError(t, os.Symlink(target, link))

	require.NoError(t, Write(link, []byte("new"), Options{Mode: 0o600, FollowSymlink: true}))
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "new", string(got))
}

func TestALinkLoopIsAnErrorNotAHang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symbolic link needs a privilege on Windows")
	}
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	require.NoError(t, os.Symlink(b, a))
	require.NoError(t, os.Symlink(a, b))

	err := Write(a, []byte("x"), Options{Mode: 0o600, FollowSymlink: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "loop")
}

func TestBeforeCanAbandonTheWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
	changed := errors.New("changed underneath")

	err := Write(path, []byte("new"), Options{Mode: 0o600, Before: func() error { return changed }})
	require.ErrorIs(t, err, changed)

	got, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, "old", string(got), "an abandoned write must leave the destination alone")
	entries, readErr := os.ReadDir(dir)
	require.NoError(t, readErr)
	assert.Len(t, entries, 1, "an abandoned write must not leave its temporary file")
}

func TestModeIsRequired(t *testing.T) {
	require.Error(t, Write(filepath.Join(t.TempDir(), "f"), nil, Options{}))
}

func assertMode(t *testing.T, path string, want os.FileMode, msg string) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, want, info.Mode().Perm(), msg)
}
