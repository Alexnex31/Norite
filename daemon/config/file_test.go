// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tempConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "norite", "config.toml")
	if contents != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func TestSetCreatesTheFileAndItsDirectory(t *testing.T) {
	path := tempConfig(t, "")
	require.NoError(t, Set(path, Shared, KeyClock, "12h"))
	assert.Equal(t, "[shared]\nclock = \"12h\"\n", readFile(t, path))

	c, err := Load(path, TUI)
	require.NoError(t, err)
	assert.Equal(t, Clock12h, c.Clock())
}

func TestSetThenUnsetThroughTheFile(t *testing.T) {
	path := tempConfig(t, dense)
	require.NoError(t, Set(path, TUI, KeyColorAccent, "#1E90FF"))
	c, err := Load(path, TUI)
	require.NoError(t, err)
	assert.Equal(t, Color("#1e90ff"), c.Color(KeyColorAccent))
	assert.Empty(t, c.Warnings)

	require.NoError(t, Unset(path, TUI, KeyColorAccent))
	c, err = Load(path, TUI)
	require.NoError(t, err)
	assert.Equal(t, Color("6"), c.Color(KeyColorAccent), "unset falls back to the default")
}

func TestASharedKeyCanBeSetForOneClient(t *testing.T) {
	path := tempConfig(t, "[shared]\nclock = \"24h\"\n")
	require.NoError(t, Set(path, TUI, KeyClock, "12h"))
	tui, err := Load(path, TUI)
	require.NoError(t, err)
	gui, err := Load(path, GUI)
	require.NoError(t, err)
	assert.Equal(t, Clock12h, tui.Clock())
	assert.Equal(t, Clock24h, gui.Clock())
}

func TestAKeyInsideATableKeepsItsDots(t *testing.T) {
	path := tempConfig(t, "")
	require.NoError(t, Set(path, TUI, "keys.C-x 4.0", "split"))
	assert.Equal(t, "[tui.keys]\n\"C-x 4.0\" = \"split\"\n", readFile(t, path))
}

func TestSetRefusesWhatItShould(t *testing.T) {
	path := tempConfig(t, dense)
	require.ErrorIs(t, Set(path, TUI, "colors.acent", "3"), ErrUnknownKey)
	require.ErrorIs(t, Set(path, GUI, KeyColorAccent, "3"), ErrUnknownKey, "the terminal's colors are not [gui]'s")
	require.Error(t, Set(path, TUI, KeyColorAccent, "red"))
	require.Error(t, Set(path, TUI, "keys", "x"))
	assert.Equal(t, dense, readFile(t, path), "a refused set writes nothing")
}

// Two Norite programs. Staged, not raced: the second is shown to be waiting while the first holds the
// lock, and both edits are in the file afterwards.
func TestTwoWritersTakeTurns(t *testing.T) {
	path := tempConfig(t, "[shared]\nclock = \"24h\"\n")

	inside, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- Update(path, func(current []byte) ([]byte, error) {
			close(inside)
			<-release
			return setRaw(current, []string{"shared", "clock"}, `"12h"`)
		})
	}()
	<-inside

	entered := make(chan struct{})
	second := make(chan error, 1)
	go func() {
		second <- Update(path, func(current []byte) ([]byte, error) {
			close(entered)
			return setRaw(current, []string{"tui", "colors", "accent"}, "9")
		})
	}()

	select {
	case <-entered:
		t.Fatal("the second writer read the file while the first still held the lock")
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-first)
	require.NoError(t, <-second)

	c, err := Load(path, TUI)
	require.NoError(t, err)
	assert.Equal(t, Clock12h, c.Clock(), "the first writer's edit")
	assert.Equal(t, Color("9"), c.Color(KeyColorAccent), "the second writer's edit, made on top of the first")
}

// A person saving in an editor takes no lock. Their save lands between this program's read and its rename,
// and it is their edit that must not be lost.
func TestAnEditorsSaveBetweenTheReadAndTheRenameIsKept(t *testing.T) {
	path := tempConfig(t, "[shared]\nclock = \"24h\"\n")

	runs := 0
	err := Update(path, func(current []byte) ([]byte, error) {
		runs++
		if runs == 1 {
			// The editor saves, by rename as most do, after Norite has read and before it has written.
			tmp := path + ".editor"
			require.NoError(t, os.WriteFile(tmp, []byte("# written by hand, just now\n[shared]\nclock = \"24h\"\n"), 0o600))
			require.NoError(t, os.Rename(tmp, path))
		}
		return setRaw(current, []string{"tui", "colors", "accent"}, "9")
	})
	require.NoError(t, err)
	assert.Equal(t, 2, runs, "the edit is redone on the person's version")
	assert.Equal(t, "# written by hand, just now\n[shared]\nclock = \"24h\"\n\n[tui.colors]\naccent = 9\n", readFile(t, path))
}

func TestAFileThatNeverStopsChangingIsGivenUpOn(t *testing.T) {
	path := tempConfig(t, "[shared]\nclock = \"24h\"\n")
	n := 0
	err := Update(path, func(current []byte) ([]byte, error) {
		n++
		require.NoError(t, os.WriteFile(path, []byte("# save "+string(rune('a'+n))+"\n"), 0o600))
		return setRaw(current, []string{"shared", "clock"}, `"12h"`)
	})
	require.ErrorIs(t, err, ErrKeepsChanging)
	assert.Equal(t, maxAttempts, n)
	assert.Contains(t, readFile(t, path), "# save", "the person's last save is what is on disk")
}

func TestAHeldLockIsReportedNotWaitedOnForever(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the lock timeout")
	}
	path := tempConfig(t, "[shared]\nclock = \"24h\"\n")
	inside, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Update(path, func(current []byte) ([]byte, error) {
			close(inside)
			<-release
			return current, nil
		})
	}()
	<-inside
	require.ErrorIs(t, Set(path, Shared, KeyClock, "12h"), ErrLocked)
	close(release)
	require.NoError(t, <-done)
}

// The config is somebody's dotfile: a link into a repository, with a mode they chose.
func TestSetWritesThroughASymlinkAndKeepsTheMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links and permission bits")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "dotfiles")
	require.NoError(t, os.MkdirAll(repo, 0o700))
	real := filepath.Join(repo, "config.toml")
	require.NoError(t, os.WriteFile(real, []byte("[shared]\nclock = \"24h\" # mine\n"), 0o644))
	require.NoError(t, os.Chmod(real, 0o644))
	link := filepath.Join(dir, "norite", "config.toml")
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o700))
	require.NoError(t, os.Symlink(real, link))

	require.NoError(t, Set(link, Shared, KeyClock, "12h"))

	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "the link must still be a link")
	assert.Equal(t, "[shared]\nclock = \"12h\" # mine\n", readFile(t, real))
	info, err = os.Stat(real)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())

	entries, err := os.ReadDir(repo)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "nothing is left in the repository: no lock file, no temporary file")
}

func TestSettingTheSameValueDoesNotRewriteTheFile(t *testing.T) {
	path := tempConfig(t, "[shared]\nclock = \"12h\"\n")
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))
	require.NoError(t, Set(path, Shared, KeyClock, "12h"))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.WithinDuration(t, old, info.ModTime(), time.Second, "an unchanged file must not wake every watcher")
}
