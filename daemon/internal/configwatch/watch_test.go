// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package configwatch

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The config's lock lives in the state directory, so config.Set here would otherwise take one in the real
// state directory of whoever runs the tests.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "norite-configwatch-test-")
	if err != nil {
		panic(err)
	}
	for _, name := range []string{"XDG_STATE_HOME", "HOME", "LOCALAPPDATA", "USERPROFILE"} {
		_ = os.Setenv(name, dir)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func tempConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "norite", "config.toml")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

// watching starts Watch on path. next waits for a notification; silent reports that none came for long
// enough that one would have.
func watching(t *testing.T, path string) (next, silent func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	notified := make(chan struct{}, 64)
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, []string{path}, func() { notified <- struct{}{} }) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
	// Give the watch time to be registered: an event before that is one nobody was listening for.
	time.Sleep(50 * time.Millisecond)
	next = func() bool {
		select {
		case <-notified:
			return true
		case <-time.After(2 * time.Second):
			return false
		}
	}
	silent = func() bool {
		select {
		case <-notified:
			return false
		case <-time.After(4 * watchSettle):
			return true
		}
	}
	return next, silent
}

func TestWatchSeesAFileThatDidNotExistBeingCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "norite", "config.toml")
	next, _ := watching(t, path)
	// As `norite config set` writes one: a temporary file beside it, renamed into place.
	tmp := filepath.Join(filepath.Dir(path), ".config.toml.new")
	require.NoError(t, os.WriteFile(tmp, []byte("[shared]\nclock = \"12h\"\n"), 0o600))
	require.NoError(t, os.Rename(tmp, path))
	require.True(t, next(), "the first `norite config set` on a new install must be noticed")
}

// The two ways an editor saves. Both must be noticed, and a rename is the one a watch on the file itself
// would miss for ever after.
func TestWatchSeesASaveByRenameAndASaveInPlace(t *testing.T) {
	path := tempConfig(t, "[shared]\nclock = \"24h\"\n")
	next, _ := watching(t, path)

	for i := range 3 {
		tmp := path + ".swp"
		require.NoError(t, os.WriteFile(tmp, []byte("[shared]\nclock = \"12h\"\n# save\n"), 0o600))
		require.NoError(t, os.Rename(tmp, path))
		require.True(t, next(), "save by rename %d", i)
	}
	require.NoError(t, os.WriteFile(path, []byte("[shared]\nclock = \"24h\"\n"), 0o600))
	require.True(t, next(), "a save in place")
	require.NoError(t, os.Remove(path))
	require.True(t, next(), "the file being deleted is a change too: the defaults apply again")
}

func TestWatchCoalescesABurstAndIgnoresOtherFiles(t *testing.T) {
	path := tempConfig(t, "[shared]\nclock = \"24h\"\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	count := make(chan struct{}, 64)
	go func() { _ = Watch(ctx, []string{path}, func() { count <- struct{}{} }) }()
	time.Sleep(50 * time.Millisecond)

	// Somebody else's files in the same directory: a theme, an editor's swap file, a lock.
	for _, other := range []string{"themes.toml", ".config.toml.swx", "config.toml.bak", "notes"} {
		require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), other), []byte("x"), 0o600))
	}
	select {
	case <-count:
		t.Fatal("a file that is not the config woke the watch")
	case <-time.After(4 * watchSettle):
	}

	for i := range 10 {
		require.NoError(t, os.WriteFile(path, []byte{byte('#'), byte('0' + i), '\n'}, 0o600))
	}
	time.Sleep(4 * watchSettle)
	require.Len(t, count, 1, "ten writes inside the settle window are one notification")
}

// The config is a link into a dotfiles repository, and a save lands in the repository's directory.
func TestWatchFollowsALinkToWhereTheFileReallyIs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "dotfiles")
	require.NoError(t, os.MkdirAll(repo, 0o700))
	real := filepath.Join(repo, "config.toml")
	require.NoError(t, os.WriteFile(real, []byte("[shared]\nclock = \"24h\"\n"), 0o600))
	link := filepath.Join(dir, "norite", "config.toml")
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o700))
	require.NoError(t, os.Symlink(real, link))
	next, silent := watching(t, link)

	// An editor opened on the repository's copy, saving by rename there.
	tmp := real + ".new"
	require.NoError(t, os.WriteFile(tmp, []byte("[shared]\nclock = \"12h\"\n"), 0o600))
	require.NoError(t, os.Rename(tmp, real))
	require.True(t, next(), "a save in the repository")

	// A neighbor in the repository is not the config.
	require.True(t, silent(), "the first save has settled")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "zshrc"), []byte("x"), 0o600))
	require.True(t, silent(), "another file in the repository is not the config")

	// The link re-pointed at another file, in another directory, which is then saved.
	other := filepath.Join(dir, "other")
	require.NoError(t, os.MkdirAll(other, 0o700))
	second := filepath.Join(other, "config.toml")
	require.NoError(t, os.WriteFile(second, []byte("[shared]\nclock = \"24h\"\n"), 0o600))
	require.NoError(t, os.Remove(link))
	require.NoError(t, os.Symlink(second, link))
	require.True(t, next(), "the link being re-pointed")
	time.Sleep(2 * watchSettle)
	require.NoError(t, os.WriteFile(second, []byte("[shared]\nclock = \"12h\"\n"), 0o600))
	require.True(t, next(), "a save at the link's new target")
}

// A watch is on a directory, not on its name. When the config's directory is removed and put back, the
// watch that was on it is on nothing, and a daemon that runs for weeks would never notice a save again.
func TestWatchSurvivesItsDirectoryBeingReplaced(t *testing.T) {
	path := tempConfig(t, "[shared]\nclock = \"24h\"\n")
	dir := filepath.Dir(path)
	next, silent := watching(t, path)

	require.NoError(t, os.RemoveAll(dir))
	require.True(t, next(), "the directory going away is a change: the config is now the defaults")
	require.True(t, silent(), "settled")
	_, err := os.Stat(dir)
	require.ErrorIs(t, err, os.ErrNotExist, "the watch does not put the directory back under whoever removed it")

	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.True(t, next(), "the directory coming back")
	require.True(t, silent(), "settled")

	require.NoError(t, os.WriteFile(path, []byte("[shared]\nclock = \"12h\"\n"), 0o600))
	require.True(t, next(), "a save in the directory that replaced the one first watched")
}

// The directory is a link a dotfiles manager owns, and re-points: nothing happens in either directory it
// has pointed at, only to the name.
func TestWatchFollowsItsDirectoryBeingRepointed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links")
	}
	root := t.TempDir()
	first, second := filepath.Join(root, "dots-a"), filepath.Join(root, "dots-b")
	for _, d := range []string{first, second} {
		require.NoError(t, os.MkdirAll(d, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(d, "config.toml"), []byte("[shared]\nclock = \"24h\"\n"), 0o600))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(root, "cfg"), 0o700))
	link := filepath.Join(root, "cfg", "norite")
	require.NoError(t, os.Symlink(first, link))
	next, silent := watching(t, filepath.Join(link, "config.toml"))

	require.NoError(t, os.Remove(link))
	require.NoError(t, os.Symlink(second, link))
	require.True(t, next(), "the directory link being re-pointed")
	require.True(t, silent(), "settled")

	require.NoError(t, os.WriteFile(filepath.Join(second, "config.toml"), []byte("[shared]\nclock = \"12h\"\n"), 0o600))
	require.True(t, next(), "a save where the link now leads")
}

// While the same-machine toggle is on each client reads its own file, and a save to either is a change.
// Their neighbors, the set-aside copies among them, are not.
func TestWatchSeesEveryFileItWasGiven(t *testing.T) {
	path := tempConfig(t, "[shared]\nclock = \"24h\"\n")
	dir := filepath.Dir(path)
	tui, gui := filepath.Join(dir, "config.tui.toml"), filepath.Join(dir, "config.gui.toml")
	ctx, cancel := context.WithCancel(context.Background())
	notified := make(chan struct{}, 64)
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, []string{path, tui, gui}, func() { notified <- struct{}{} }) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
	time.Sleep(50 * time.Millisecond)
	next := func() bool {
		select {
		case <-notified:
			return true
		case <-time.After(2 * time.Second):
			return false
		}
	}
	silent := func() bool {
		select {
		case <-notified:
			return false
		case <-time.After(4 * watchSettle):
			return true
		}
	}

	for _, file := range []string{tui, gui, path} {
		require.NoError(t, os.WriteFile(file, []byte("[shared]\nclock = \"12h\"\n"), 0o600))
		require.True(t, next(), filepath.Base(file))
		require.True(t, silent(), "settled")
	}
	require.NoError(t, os.WriteFile(tui+".before-unsplit", []byte("x"), 0o600))
	require.True(t, silent(), "a set-aside copy is not a config anybody reads")
}

// A config that is a link to a file one directory up has its real file in the directory the config's own
// is in, which is watched already, for another reason: it is what notices the config directory being
// removed and put back. Pointing the link somewhere else must not take that watch away.
func TestRepointingALinkDoesNotCostTheWatchOnTheParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "norite")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	up := filepath.Join(root, "norite.toml")
	require.NoError(t, os.WriteFile(up, []byte("[shared]\nclock = \"24h\"\n"), 0o600))
	link := filepath.Join(dir, "config.toml")
	require.NoError(t, os.Symlink(up, link))
	next, silent := watching(t, link)

	// Re-pointed at a plain file beside it: the real file is no longer in the parent.
	require.NoError(t, os.Remove(link))
	require.NoError(t, os.WriteFile(link, []byte("[shared]\nclock = \"12h\"\n"), 0o600))
	require.True(t, next(), "the link being replaced")
	require.True(t, silent(), "settled")

	// The case the parent's watch is for.
	require.NoError(t, os.RemoveAll(dir))
	require.True(t, next(), "the directory going away")
	require.True(t, silent(), "settled")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.True(t, next(), "the directory coming back")
	require.True(t, silent(), "settled")
	require.NoError(t, os.WriteFile(link, []byte("[shared]\nclock = \"24h\"\n"), 0o600))
	require.True(t, next(), "a save in the directory that replaced it")
}
