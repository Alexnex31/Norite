// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package logfile

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// A pipe where the log goes is refused at once. Opened the ordinary way it would wait for a writer that
// never comes, and `norite logs tail` would hang on a machine where something replaced the log.
func TestAPipeWhereTheLogGoesIsRefusedWithoutWaiting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot make a pipe here: %v", err)
	}

	for name, read := range map[string]func() error{
		"Tail": func() error { _, _, err := Tail(path, Options{Lines: 5}); return err },
		"Next": func() error { _, err := Follow(path, nil).Next(); return err },
	} {
		done := make(chan error, 1)
		go func() { done <- read() }()
		select {
		case err := <-done:
			if !errors.Is(err, ErrNotAFile) {
				t.Errorf("%s on a pipe: %v, want ErrNotAFile", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s on a pipe did not return", name)
		}
	}
}

// Nothing holds the log between reads: the daemon rotates by renaming it, and on Windows a held file is
// in the way of that. Checked where open files can be listed.
func TestNeitherTailNorFollowLeavesTheLogOpen(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("lists this process's open files through /proc")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	writeLog(t, path, numbered(1, 3))
	writeLog(t, filepath.Join(dir, "daemon-2026-10-09T10-00-00.000.log"), numbered(1, 3))

	_, follower, err := Tail(path, Options{Lines: 5, Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, numbered(4, 5))
	if _, err := follower.Next(); err != nil {
		t.Fatal(err)
	}

	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("cannot list open files: %v", err)
	}
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name()))
		if err != nil {
			continue
		}
		if filepath.Dir(target) == dir {
			t.Errorf("file descriptor %s is still open on %s", fd.Name(), target)
		}
	}
}
