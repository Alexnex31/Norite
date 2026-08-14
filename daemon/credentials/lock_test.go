package credentials

import (
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two writers is a new situation as of M7: the CLI writes at `norite login`, the daemon writes back the
// rotated refresh token at every start, and starting the daemon and logging in are things people do in
// either order seconds apart.

// Concurrent saves must each leave the store consistent — a record and a token that belong to the same
// save, never one half of each.
//
// The window is forced open rather than waited for. A Save is two small writes, so the interleaving is real
// but narrow, and a test that hopes to hit it by chance passes just as happily with no lock at all — which
// is exactly what an earlier version of this test did.
func TestConcurrentSavesDoNotInterleave(t *testing.T) {
	dir := t.TempDir()

	slow, err := OpenIn(dir)
	require.NoError(t, err)

	inWindow := make(chan struct{})
	release := make(chan struct{})
	slow.betweenWrites = func() {
		close(inWindow)
		<-release
	}

	slowRecord := sampleRecord()
	slowRecord.Username = "ada"

	done := make(chan error, 1)
	go func() { done <- slow.Save(slowRecord, "nrt_ada") }()

	// The slow writer now holds the lock, with its token written and its record not yet.
	<-inWindow

	// A second writer, as a separate process would be. It must not be able to slip its record in between.
	second := make(chan error, 1)
	go func() {
		store, err := OpenIn(dir)
		if err != nil {
			second <- err
			return
		}
		fast := sampleRecord()
		fast.Username = "grace"
		second <- store.Save(fast, "nrt_grace")
	}()

	// Give the second writer every chance to interleave before letting the first finish.
	time.Sleep(200 * time.Millisecond)
	close(release)

	require.NoError(t, <-done)
	require.NoError(t, <-second)

	store, err := OpenIn(dir)
	require.NoError(t, err)
	record, token, err := store.Load()
	require.NoError(t, err)

	assert.Equal(t, "nrt_"+record.Username, token,
		"the record and the token must come from the same save, not from two different ones")
}

// A read taken while a write is in flight must not see half of it.
func TestLoadWaitsForAWriteInFlight(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenIn(dir)
	require.NoError(t, err)
	require.NoError(t, store.Save(sampleRecord(), "nrt_initial"))

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := OpenIn(dir)
			if !assert.NoError(t, err) {
				return
			}
			second := sampleRecord()
			second.Username = "grace"
			assert.NoError(t, s.Save(second, "nrt_second"))
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := OpenIn(dir)
			if !assert.NoError(t, err) {
				return
			}
			record, token, err := s.Load()
			if !assert.NoError(t, err) {
				return
			}
			// Either state is fine; a mixture is not.
			assert.Equal(t, "nrt_"+map[string]string{"ada": "initial", "grace": "second"}[record.Username],
				token)
		}()
	}
	wg.Wait()
}

// The lock is its own file and never holds content. A lock taken on a file that is then replaced by rename
// protects nothing — the new file is a different inode, and the next process locks that one instead.
func TestTheLockIsSeparateFromTheFilesItGuards(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenIn(dir)
	require.NoError(t, err)
	require.NoError(t, store.Save(sampleRecord(), "nrt_secret"))

	assert.NotEqual(t, filepath.Join(dir, lockFileName), store.recordPath())

	// ...and it does not become a place a secret lands.
	data, err := exec.Command("cat", filepath.Join(dir, lockFileName)).Output()
	require.NoError(t, err)
	assert.NotContains(t, string(data), "nrt_secret")
	assert.Empty(t, strings.TrimSpace(string(data)))
}
