// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package statefile

import (
	"context"
	"errors"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/statefile"
)

func split(on bool) func(*statefile.State) error {
	return func(s *statefile.State) error { s.ConfigSplit = on; return nil }
}

// No file is every default, and reading creates nothing.
func TestNoFileIsTheDefaults(t *testing.T) {
	dir := t.TempDir()
	s, err := statefile.ReadIn(dir)
	require.NoError(t, err)
	assert.Equal(t, statefile.State{Version: statefile.Version}, s)
	_, err = os.Stat(statefile.PathIn(dir))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// What is written is what is read back, in a file only its owner can read: it will hold plugin grants.
func TestAnUpdateIsReadBackAndIsPrivate(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Update(context.Background(), dir, split(true), nil))
	s, err := statefile.ReadIn(dir)
	require.NoError(t, err)
	assert.True(t, s.ConfigSplit)
	assert.Equal(t, statefile.Version, s.Version)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(statefile.PathIn(dir))
		require.NoError(t, err)
		assert.EqualValues(t, 0o600, info.Mode().Perm())
	}
}

// An update that changes nothing writes nothing, and one that fails leaves the file as it was.
func TestAnUpdateThatChangesNothingOrFailsLeavesTheFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Update(context.Background(), dir, split(false), nil))
	_, err := os.Stat(statefile.PathIn(dir))
	require.ErrorIs(t, err, os.ErrNotExist, "nothing to record is no file")

	require.NoError(t, Update(context.Background(), dir, split(true), nil))
	before, err := os.ReadFile(statefile.PathIn(dir))
	require.NoError(t, err)
	boom := errors.New("the work the state stands for failed")
	err = Update(context.Background(), dir, func(s *statefile.State) error {
		s.ConfigSplit = false
		return boom
	}, func() { t.Error("then ran after an update that failed") })
	require.ErrorIs(t, err, boom)
	after, err := os.ReadFile(statefile.PathIn(dir))
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}

// A field this build does not know was put there by a later milestone's daemon. An older one that writes
// the file keeps it.
func TestAFieldThisBuildDoesNotKnowSurvivesAWrite(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(statefile.PathIn(dir),
		[]byte(`{"version":1,"config_split":false,"voice_breadcrumb":{"channel_id":"42"}}`), 0o600))
	require.NoError(t, Update(context.Background(), dir, split(true), nil))
	data, err := os.ReadFile(statefile.PathIn(dir))
	require.NoError(t, err)
	assert.JSONEq(t, `{"version":1,"config_split":true,"voice_breadcrumb":{"channel_id":"42"}}`, string(data))
}

// A file from a newer format is refused, by reader and writer alike, rather than read as far as it is
// understood and then written back as this format. So is one that is not a state file at all.
func TestAFileThisBuildCannotReadIsRefusedAndNotRewritten(t *testing.T) {
	for name, body := range map[string]string{
		"newer":      `{"version":2,"config_split":true}`,
		"no version": `{"config_split":true}`,
		"not json":   `config_split = true`,
		"an array":   `[1,2,3]`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(statefile.PathIn(dir), []byte(body), 0o600))
			_, err := statefile.ReadIn(dir)
			require.Error(t, err)
			if name == "newer" {
				require.ErrorIs(t, err, statefile.ErrNewer)
			}
			require.Error(t, Update(context.Background(), dir, split(false), nil))
			data, err := os.ReadFile(statefile.PathIn(dir))
			require.NoError(t, err)
			assert.Equal(t, body, string(data))
		})
	}
}

// A file larger than any state file is not read into memory to find that out.
func TestAnOversizedFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(statefile.PathIn(dir), make([]byte, statefile.MaxSize+1), 0o600))
	_, err := statefile.ReadIn(dir)
	require.ErrorContains(t, err, "larger than")
}

// Two requests at once are two goroutines in one daemon. An update runs with the file to itself from its
// read to its write: fn may do the work the change stands for, and two of those at once on the same files
// is what the lock is for. Counted inside fn, never more than one is running.
func TestUpdatesDoNotInterleave(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	var inside, most, flips atomic.Int32
	for range 12 {
		wg.Go(func() {
			err := Update(context.Background(), dir, func(s *statefile.State) error {
				now := inside.Add(1)
				for {
					seen := most.Load()
					if now <= seen || most.CompareAndSwap(seen, now) {
						break
					}
				}
				time.Sleep(5 * time.Millisecond)
				s.ConfigSplit = !s.ConfigSplit
				flips.Add(1)
				inside.Add(-1)
				return nil
			}, nil)
			assert.NoError(t, err)
		})
	}
	wg.Wait()
	s, err := statefile.ReadIn(dir)
	require.NoError(t, err)
	assert.EqualValues(t, 1, most.Load(), "updates ran one at a time")
	assert.EqualValues(t, 12, flips.Load())
	assert.False(t, s.ConfigSplit, "an even number of flips, each made on the last one's result")
}

// What follows the write runs before the lock is let go, so the next update cannot get between the state
// and the work that had to come after it. A second update started from inside it is told the file is
// locked.
func TestWhatFollowsTheWriteRunsUnderTheLock(t *testing.T) {
	dir := t.TempDir()
	ran := false
	err := Update(context.Background(), dir, split(true), func() {
		ran = true
		written, err := statefile.ReadIn(dir)
		require.NoError(t, err)
		assert.True(t, written.ConfigSplit, "the state is already on disk")

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		assert.ErrorIs(t, Update(ctx, dir, split(false), nil), ErrLocked)
	})
	require.NoError(t, err)
	assert.True(t, ran)
}

// A state file that is a pipe or a directory is refused before it is opened: opening a pipe waits for a
// writer that never comes.
func TestAStateFileThatIsNotAFileIsRefusedWithoutWaiting(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(statefile.PathIn(dir), 0o700))
	_, err := statefile.ReadIn(dir)
	require.ErrorContains(t, err, "not a regular file")
}
