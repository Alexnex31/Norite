// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package updater

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Alexnex31/Norite/daemon/installreceipt"
)

// packaged makes the world's daemon one a package manager installed: no receipt, a marker beside it.
func packaged(t *testing.T, w *world) {
	t.Helper()
	if err := installreceipt.Remove(w.state); err != nil {
		t.Fatal(err)
	}
	writeMarker(t, filepath.Join(w.bin, MarkerName), "pacman")
}

// packageUpgrade does what a package manager does: a new file renamed over the old one's name.
func packageUpgrade(t *testing.T, w *world, script string) {
	t.Helper()
	next := filepath.Join(w.bin, ".next")
	if err := os.WriteFile(next, []byte(script), 0o755); err != nil { //nolint:gosec // a stand-in program
		t.Fatal(err)
	}
	if err := os.Rename(next, filepath.Join(w.bin, daemonName)); err != nil {
		t.Fatal(err)
	}
}

// watching runs u until the test ends, with the watch looking every few milliseconds.
func watching(t *testing.T, u *Updater) {
	t.Helper()
	was := replacedEvery
	replacedEvery = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { u.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; replacedEvery = was })
}

func (w *world) restartedWithin(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if w.restarts.Load() > 0 {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func atProgram(w *world) func(*Options) {
	return func(o *Options) { o.Program = filepath.Join(w.bin, daemonName) }
}

// After `pacman -Syu` the running daemon starts the version installed over it, rather than staying the old
// one until the next login and saying a release is out that is already installed (ADR 0035 §4).
func TestAPackagedDaemonStartsTheVersionInstalledOverIt(t *testing.T) {
	w := newWorld(t)
	packaged(t, w)
	u := w.updater("0.1.0", atProgram(w))
	watching(t, u)

	packageUpgrade(t, w, "#!/bin/sh\necho 0.1.1\n")
	if !w.restartedWithin(5 * time.Second) {
		t.Fatal("the daemon did not end its run for the version installed over it")
	}
	if !u.Finish(func() bool { return false }) {
		t.Error("Finish did not ask for the program on disk to be started")
	}
}

// Asked to stop, it stops, as an update does: the new version starts next time.
func TestAStopWinsOverStartingTheReplacedVersion(t *testing.T) {
	w := newWorld(t)
	packaged(t, w)
	u := w.updater("0.1.0", atProgram(w))
	watching(t, u)
	packageUpgrade(t, w, "#!/bin/sh\necho 0.1.1\n")
	if !w.restartedWithin(5 * time.Second) {
		t.Fatal("no restart")
	}
	if u.Finish(func() bool { return true }) {
		t.Error("a daemon asked to stop was started again")
	}
}

// A reinstall of the same version, and a file that does not run yet, are not restarted for; the second is
// looked at again, and restarted for once it runs.
func TestOnlyAProgramThatRunsAsAnotherVersionIsStarted(t *testing.T) {
	w := newWorld(t)
	packaged(t, w)
	u := w.updater("0.1.0", atProgram(w))
	watching(t, u)

	packageUpgrade(t, w, "#!/bin/sh\necho 0.1.0\n")
	if w.restartedWithin(200 * time.Millisecond) {
		t.Fatal("restarted for a reinstall of the same version")
	}
	packageUpgrade(t, w, "#!/bin/sh\nexit 1\n")
	if w.restartedWithin(200 * time.Millisecond) {
		t.Fatal("restarted for a program that does not run")
	}
	packageUpgrade(t, w, "#!/bin/sh\necho 0.1.2\n")
	if !w.restartedWithin(5 * time.Second) {
		t.Fatal("a program that runs, looked at again, was not started")
	}
}

// A voice session puts the restart off, as it puts off an update.
func TestAVoiceSessionPutsTheRestartOff(t *testing.T) {
	w := newWorld(t)
	packaged(t, w)
	w.busy.Store(true)
	u := w.updater("0.1.0", atProgram(w))
	watching(t, u)

	packageUpgrade(t, w, "#!/bin/sh\necho 0.1.1\n")
	if w.restartedWithin(200 * time.Millisecond) {
		t.Fatal("restarted during a voice session")
	}
	w.busy.Store(false)
	if !w.restartedWithin(5 * time.Second) {
		t.Fatal("not restarted once the session ended")
	}
}

// Only a packaged daemon watches: the updater restarts its own install, and a development build or a copy
// run from elsewhere is replaced by somebody who did not ask for it to restart.
func TestOnlyAPackagedDaemonWatchesItsProgram(t *testing.T) {
	for name, setup := range map[string]func(*testing.T, *world){
		"the installer's": func(*testing.T, *world) {},
		"unknown": func(t *testing.T, w *world) {
			if err := installreceipt.Remove(w.state); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			setup(t, w)
			watching(t, w.updater("0.1.0", atProgram(w)))
			packageUpgrade(t, w, "#!/bin/sh\necho 0.1.1\n")
			if w.restartedWithin(200 * time.Millisecond) {
				t.Error("restarted for a program it does not watch")
			}
		})
	}
}
