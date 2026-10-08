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

// watched is a Watch running for a test.
type watched struct{ notified chan struct{} }

// watching starts Watch on the paths.
func watching(t *testing.T, paths ...string) *watched {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := &watched{notified: make(chan struct{}, 64)}
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, paths, func() { w.notified <- struct{}{} }) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
	// Give the watch time to be registered: an event before that is one nobody was listening for.
	time.Sleep(50 * time.Millisecond)
	return w
}

// next waits for a notification. Long, because a miss is the failure and a slow machine is not one.
func (w *watched) next() bool {
	select {
	case <-w.notified:
		return true
	case <-time.After(10 * time.Second):
		return false
	}
}

// silent reports that no notification came for long enough that one would have. It is for asserting that
// something is not the config, and is only meaningful after settle.
func (w *watched) silent() bool {
	select {
	case <-w.notified:
		return false
	case <-time.After(4 * watchSettle):
		return true
	}
}

// settle waits until the watch has gone quiet, taking whatever notifications come first. One save is
// several events, and on a slow machine they can fall either side of a window and be told twice, which is
// the watch being right twice and not a fault: a test that means "let that finish" must not count them.
func (w *watched) settle() {
	for {
		select {
		case <-w.notified:
		case <-time.After(4 * watchSettle):
			return
		}
	}
}

func TestWatchSeesAFileThatDidNotExistBeingCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "norite", "config.toml")
	w := watching(t, path)
	// As `norite config set` writes one: a temporary file beside it, renamed into place.
	tmp := filepath.Join(filepath.Dir(path), ".config.toml.new")
	require.NoError(t, os.WriteFile(tmp, []byte("[shared]\nclock = \"12h\"\n"), 0o600))
	require.NoError(t, os.Rename(tmp, path))
	require.True(t, w.next(), "the first `norite config set` on a new install must be noticed")
}

// The two ways an editor saves. Both must be noticed, and a rename is the one a watch on the file itself
// would miss for ever after.
func TestWatchSeesASaveByRenameAndASaveInPlace(t *testing.T) {
	path := tempConfig(t, "[shared]\nclock = \"24h\"\n")
	w := watching(t, path)

	for i := range 3 {
		tmp := path + ".swp"
		require.NoError(t, os.WriteFile(tmp, []byte("[shared]\nclock = \"12h\"\n# save\n"), 0o600))
		require.NoError(t, os.Rename(tmp, path))
		require.True(t, w.next(), "save by rename %d", i)
	}
	require.NoError(t, os.WriteFile(path, []byte("[shared]\nclock = \"24h\"\n"), 0o600))
	require.True(t, w.next(), "a save in place")
	require.NoError(t, os.Remove(path))
	require.True(t, w.next(), "the file being deleted is a change too: the defaults apply again")
}

func TestWatchCoalescesABurstAndIgnoresOtherFiles(t *testing.T) {
	// The burst ends when the test says so, not after a tenth of a second: how many writes fit in that is
	// a fact about the machine. Every wait the watch starts is counted, and none ends until released.
	armed := make(chan chan time.Time, 64)
	real := settleAfter
	settleAfter = func() <-chan time.Time {
		c := make(chan time.Time, 1)
		armed <- c
		return c
	}
	t.Cleanup(func() { settleAfter = real })

	path := tempConfig(t, "[shared]\nclock = \"24h\"\n")
	ctx, cancel := context.WithCancel(context.Background())
	count := make(chan struct{}, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Watch(ctx, []string{path}, func() { count <- struct{}{} })
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	time.Sleep(50 * time.Millisecond)

	// Somebody else's files in the same directory: a theme, an editor's swap file, a lock. None of them
	// starts a wait at all.
	for _, other := range []string{"themes.toml", ".config.toml.swx", "config.toml.bak", "notes"} {
		require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), other), []byte("x"), 0o600))
	}
	select {
	case <-armed:
		t.Fatal("a file that is not the config woke the watch")
	case <-time.After(300 * time.Millisecond):
	}

	// Ten saves, however long they take. The first starts one wait and the rest join it.
	for i := range 10 {
		require.NoError(t, os.WriteFile(path, []byte{byte('#'), byte('0' + i), '\n'}, 0o600))
	}
	var first chan time.Time
	select {
	case first = <-armed:
	case <-time.After(5 * time.Second):
		t.Fatal("ten saves of the config started no wait")
	}
	// Long enough for every event of the ten to have been read while that wait is open.
	time.Sleep(300 * time.Millisecond)
	require.Empty(t, armed, "one burst is one wait: the saves after the first joined it")
	require.Empty(t, count, "nothing is said until the burst ends")

	first <- time.Now()
	select {
	case <-count:
	case <-time.After(5 * time.Second):
		t.Fatal("the burst ended and nobody was told")
	}
	select {
	case <-count:
		t.Fatal("ten saves in one burst are one notification")
	case <-armed:
		t.Fatal("nothing happened after the burst, and a wait was started")
	case <-time.After(300 * time.Millisecond):
	}
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
	w := watching(t, link)

	// An editor opened on the repository's copy, saving by rename there.
	tmp := real + ".new"
	require.NoError(t, os.WriteFile(tmp, []byte("[shared]\nclock = \"12h\"\n"), 0o600))
	require.NoError(t, os.Rename(tmp, real))
	require.True(t, w.next(), "a save in the repository")

	// A neighbor in the repository is not the config.
	w.settle()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "zshrc"), []byte("x"), 0o600))
	require.True(t, w.silent(), "another file in the repository is not the config")

	// The link re-pointed at another file, in another directory, which is then saved.
	other := filepath.Join(dir, "other")
	require.NoError(t, os.MkdirAll(other, 0o700))
	second := filepath.Join(other, "config.toml")
	require.NoError(t, os.WriteFile(second, []byte("[shared]\nclock = \"24h\"\n"), 0o600))
	require.NoError(t, os.Remove(link))
	require.NoError(t, os.Symlink(second, link))
	require.True(t, w.next(), "the link being re-pointed")
	time.Sleep(2 * watchSettle)
	require.NoError(t, os.WriteFile(second, []byte("[shared]\nclock = \"12h\"\n"), 0o600))
	require.True(t, w.next(), "a save at the link's new target")
}

// A watch is on a directory, not on its name. When the config's directory is removed and put back, the
// watch that was on it is on nothing, and a daemon that runs for weeks would never notice a save again.
func TestWatchSurvivesItsDirectoryBeingReplaced(t *testing.T) {
	path := tempConfig(t, "[shared]\nclock = \"24h\"\n")
	dir := filepath.Dir(path)
	w := watching(t, path)

	require.NoError(t, os.RemoveAll(dir))
	require.True(t, w.next(), "the directory going away is a change: the config is now the defaults")
	w.settle()
	_, err := os.Stat(dir)
	require.ErrorIs(t, err, os.ErrNotExist, "the watch does not put the directory back under whoever removed it")

	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.True(t, w.next(), "the directory coming back")
	w.settle()

	require.NoError(t, os.WriteFile(path, []byte("[shared]\nclock = \"12h\"\n"), 0o600))
	require.True(t, w.next(), "a save in the directory that replaced the one first watched")
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
	w := watching(t, filepath.Join(link, "config.toml"))

	require.NoError(t, os.Remove(link))
	require.NoError(t, os.Symlink(second, link))
	require.True(t, w.next(), "the directory link being re-pointed")
	w.settle()

	require.NoError(t, os.WriteFile(filepath.Join(second, "config.toml"), []byte("[shared]\nclock = \"12h\"\n"), 0o600))
	require.True(t, w.next(), "a save where the link now leads")
}

// While the same-machine toggle is on each client reads its own file, and a save to either is a change.
// Their neighbors, the set-aside copies among them, are not.
func TestWatchSeesEveryFileItWasGiven(t *testing.T) {
	path := tempConfig(t, "[shared]\nclock = \"24h\"\n")
	dir := filepath.Dir(path)
	tui, gui := filepath.Join(dir, "config.tui.toml"), filepath.Join(dir, "config.gui.toml")
	w := watching(t, path, tui, gui)

	for _, file := range []string{tui, gui, path} {
		require.NoError(t, os.WriteFile(file, []byte("[shared]\nclock = \"12h\"\n"), 0o600))
		require.True(t, w.next(), filepath.Base(file))
		w.settle()
	}
	require.NoError(t, os.WriteFile(tui+".before-unsplit", []byte("x"), 0o600))
	require.True(t, w.silent(), "a set-aside copy is not a config anybody reads")
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
	w := watching(t, link)

	// Re-pointed at a plain file beside it: the real file is no longer in the parent.
	require.NoError(t, os.Remove(link))
	require.NoError(t, os.WriteFile(link, []byte("[shared]\nclock = \"12h\"\n"), 0o600))
	require.True(t, w.next(), "the link being replaced")
	w.settle()

	// The case the parent's watch is for.
	require.NoError(t, os.RemoveAll(dir))
	require.True(t, w.next(), "the directory going away")
	w.settle()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.True(t, w.next(), "the directory coming back")
	w.settle()
	require.NoError(t, os.WriteFile(link, []byte("[shared]\nclock = \"24h\"\n"), 0o600))
	require.True(t, w.next(), "a save in the directory that replaced it")
}
