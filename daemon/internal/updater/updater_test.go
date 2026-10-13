// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/daemon/installreceipt"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// world is one installation: a state directory with a receipt, a bin directory holding 0.1.0's pair, and a
// release host offering 0.1.1, whose daemon is a script that reports that version.
type world struct {
	t         *testing.T
	state     string
	bin       string
	host      *host
	listing   atomic.Value // string: the listing's body
	listed    atomic.Int32
	restarts  atomic.Int32
	changes   atomic.Int32
	busy      atomic.Bool
	instance  atomic.Value // string
	signedIn  atomic.Bool
	verifyErr atomic.Value // error
}

const newDaemonScript = "#!/bin/sh\necho 0.1.1\n"

func newWorld(t *testing.T) *world {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in daemon is a shell script")
	}
	root := t.TempDir()
	w := &world{t: t, state: filepath.Join(root, "state"), bin: filepath.Join(root, "bin")}
	for _, dir := range []string{w.state, w.bin} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{cliName, daemonName} {
		if err := os.WriteFile(filepath.Join(w.bin, name), []byte("0.1.0 "+name), 0o755); err != nil { //nolint:gosec // a stand-in program
			t.Fatal(err)
		}
	}
	if err := installreceipt.Write(w.state, installreceipt.Receipt{BinDir: w.bin, By: "test"}); err != nil {
		t.Fatal(err)
	}
	archive := fmt.Sprintf("norite_0.1.1_linux_%s.tar.gz", runtime.GOARCH)
	w.host = newHost(t, "v0.1.1", archive, tarGz(t, entry{name: "norite", body: "0.1.1 norite"},
		entry{name: "norite-daemon", body: newDaemonScript}))
	w.listing.Store(`[{"tag_name":"v0.1.1"},{"tag_name":"v0.1.0"}]`)
	w.instance.Store("")
	inner := w.host.server.Config.Handler
	w.host.server.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases" {
			w.listed.Add(1)
			_, _ = fmt.Fprint(rw, w.listing.Load().(string))
			return
		}
		inner.ServeHTTP(rw, r)
	})
	return w
}

// updater builds the Updater for a daemon of the given version running from the bin directory.
func (w *world) updater(version string, change ...func(*Options)) *Updater {
	src := sourceAt(w.host.server)
	o := Options{Version: version, StateDir: w.state, Executable: filepath.Join(w.bin, daemonName),
		Source: &src, GOOS: "linux", Log: zerolog.Nop(),
		Instance: func() (string, bool) { return w.instance.Load().(string), w.signedIn.Load() },
		Busy:     w.busy.Load,
		Restart:  func() { w.restarts.Add(1) },
		Changed:  func() { w.changes.Add(1) },
		Verify: func(_, _ []byte, _ string) error {
			if err, _ := w.verifyErr.Load().(error); err != nil {
				return err
			}
			return nil
		}}
	for _, c := range change {
		c(&o)
	}
	return New(o)
}

func (w *world) daemonOnDisk() string {
	w.t.Helper()
	got, err := os.ReadFile(filepath.Join(w.bin, daemonName))
	if err != nil {
		w.t.Fatal(err)
	}
	return string(got)
}

func TestWhichDaemonsInstallReleases(t *testing.T) {
	w := newWorld(t)
	if u := w.updater("0.1.0"); !u.installs || u.Status().Mode != ipc.UpdateAuto {
		t.Errorf("an installer's install: installs=%v (%s), mode %s", u.installs, u.reason, u.Status().Mode)
	}
	cases := map[string]*Updater{
		"development build": w.updater("dev"),
		"not the one":       w.updater("0.1.0", func(o *Options) { o.Executable = "/usr/bin/norite-daemon" }),
		"macOS":             w.updater("0.1.0", func(o *Options) { o.GOOS = "darwin" }),
	}
	if err := installreceipt.Remove(w.state); err != nil {
		t.Fatal(err)
	}
	cases["not made by"] = w.updater("0.1.0")
	for says, u := range cases {
		s := u.Status()
		if s.Installs || !strings.Contains(s.Reason, says) {
			t.Errorf("%s: installs=%v, reason %q", says, s.Installs, s.Reason)
		}
		// Told nothing would change, it is not changed: the default is to say so and wait.
		if s.Mode != ipc.UpdateNotify {
			t.Errorf("%s: mode %s", says, s.Mode)
		}
		if _, err := u.SetMode(ipc.UpdateAuto); err == nil {
			t.Errorf("%s: auto was accepted by a daemon that installs nothing", says)
		}
		if _, err := u.Apply(t.Context()); err == nil {
			t.Errorf("%s: apply was accepted", says)
		}
		if u.AtStart(true) {
			t.Errorf("%s: asked to restart at start", says)
		}
	}
	if w.restarts.Load() != 0 || w.daemonOnDisk() != "0.1.0 norite-daemon" {
		t.Errorf("something was installed or restarted")
	}
}

func TestAnAutomaticUpdateInstallsWhenTheRunEnds(t *testing.T) {
	w := newWorld(t)
	u := w.updater("0.1.0")
	if waiting := u.scheduled(t.Context()); waiting {
		t.Fatal("nothing defers it, and it waits")
	}
	if w.restarts.Load() != 1 {
		t.Fatalf("the run was asked to end %d times", w.restarts.Load())
	}
	// Nothing is on disk until the run has ended.
	if w.daemonOnDisk() != "0.1.0 norite-daemon" {
		t.Fatal("the program was replaced while the daemon was still running")
	}
	if !u.Finish(notStopped) {
		t.Fatal("Finish did not ask for the new version to be started")
	}
	if w.daemonOnDisk() != newDaemonScript {
		t.Errorf("the daemon on disk is %q", w.daemonOnDisk())
	}

	// The new version starts, is on trial, and says so.
	next := w.updater("0.1.1")
	if next.AtStart(false) {
		t.Fatal("the new version was undone at its first start")
	}
	if s := next.Status(); s.Trial != "0.1.1" || s.Available != "" {
		t.Errorf("status %+v", s)
	}
}

func TestNotifyChecksAndDownloadsNothing(t *testing.T) {
	w := newWorld(t)
	u := w.updater("0.1.0")
	if _, err := u.SetMode(ipc.UpdateNotify); err != nil {
		t.Fatal(err)
	}
	u.scheduled(t.Context())
	if s := u.Status(); s.Available != "0.1.1" || s.LastCheck == "" {
		t.Errorf("status %+v", s)
	}
	if n := w.host.asked["checksums.txt"].Load(); n != 0 || w.restarts.Load() != 0 {
		t.Errorf("notify downloaded (%d) or restarted (%d)", n, w.restarts.Load())
	}
	if u.Finish(notStopped) {
		t.Errorf("a run that ended for no update asked to be replaced")
	}

	// Asked, it installs.
	if _, err := u.Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for w.restarts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if w.restarts.Load() != 1 || !u.Finish(notStopped) || w.daemonOnDisk() != newDaemonScript {
		t.Errorf("apply: restarts=%d, on disk %q", w.restarts.Load(), w.daemonOnDisk())
	}
}

func TestAnUnverifiedReleaseIsRefusedAndTheVersionKeepsRunning(t *testing.T) {
	w := newWorld(t)
	w.verifyErr.Store(fmt.Errorf("%w: test", ErrNotVerified))
	u := w.updater("0.1.0")
	// A failure, and neither a refusal nor a host that could not be reached.
	_, err := u.Apply(t.Context())
	var refused *ErrRefused
	var unreachable *ErrUnreachable
	if err == nil || errors.As(err, &refused) || errors.As(err, &unreachable) || !strings.Contains(err.Error(), "keeps running") {
		t.Errorf("apply: %v", err)
	}
	if w.restarts.Load() != 0 || u.Finish(notStopped) || w.daemonOnDisk() != "0.1.0 norite-daemon" {
		t.Errorf("an unverified release changed something")
	}
	// And it is not announced as out: the listing named it, and no signature stands behind it.
	if s := u.Status(); !strings.Contains(s.LastError, "did not verify") || s.Available != "" {
		t.Errorf("status %+v", s)
	}

	// Nor at the next check, which used to announce it again and forget why it was refused; and with the
	// mode on auto its files were fetched and refused again every day (M24 /code-review).
	fetched := w.host.asked["checksums.txt"].Load()
	if waiting, again := u.turn(t.Context()); waiting || again != 0 {
		t.Errorf("the next turn: waiting=%v again=%v", waiting, again)
	}
	if s := u.Status(); !strings.Contains(s.LastError, "did not verify") || s.Available != "" || s.LastCheck == "" {
		t.Errorf("after the next check: %+v", s)
	}
	if w.host.asked["checksums.txt"].Load() != fetched {
		t.Error("the refused release was downloaded again")
	}
	// Asked for again, the refusal names it.
	if _, err := u.Apply(t.Context()); !errors.As(err, &refused) || !strings.Contains(err.Error(), "0.1.1") {
		t.Errorf("a second apply: %v", err)
	}
	// It is this run's judgement and not the install's: a signature can fail because this install's root
	// is too old, and the next run asks again.
	passes := func(o *Options) { o.Verify = func(_, _ []byte, _ string) error { return nil } }
	if next := w.updater("0.1.0", passes); next.scheduled(t.Context()) || w.restarts.Load() != 1 {
		t.Errorf("the next run was not offered the release: restarts=%d", w.restarts.Load())
	}
}

func TestADowngradeIsNeverChosen(t *testing.T) {
	w := newWorld(t)
	// The host offers only what is older than what runs, validly listed.
	u := w.updater("0.1.2")
	u.scheduled(t.Context())
	if _, err := u.Apply(t.Context()); err == nil {
		t.Error("apply moved to an older version")
	}
	if s := u.Status(); s.Available != "" || w.restarts.Load() != 0 || w.host.asked["checksums.txt"].Load() != 0 {
		t.Errorf("status %+v, restarts %d", s, w.restarts.Load())
	}
}

func TestTheInstanceBoundsTheChoice(t *testing.T) {
	w := newWorld(t)
	w.instance.Store("0.2.0")
	w.signedIn.Store(true)
	u := w.updater("0.1.0")
	u.scheduled(t.Context())
	if s := u.Status(); s.Available != "" || w.restarts.Load() != 0 {
		t.Errorf("moved within 0.1 while signed in to a 0.2 instance: %+v", s)
	}
	// Signed out of it, its version is nobody's: the running version's own patches are taken, and it
	// used to go on bounding the choice (M24 /code-review).
	w.signedIn.Store(false)
	if _, err := u.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s := u.Status(); s.Available != "0.1.1" {
		t.Errorf("signed out, an instance left behind still chose: %+v", s)
	}
}

func TestAnUpdateWaitsWhileBusy(t *testing.T) {
	w := newWorld(t)
	w.busy.Store(true)
	u := w.updater("0.1.0")
	if !u.scheduled(t.Context()) || w.restarts.Load() != 0 {
		t.Fatalf("a busy daemon was restarted (%d)", w.restarts.Load())
	}
	if _, err := u.Apply(t.Context()); err == nil || !strings.Contains(err.Error(), "ready") {
		t.Errorf("apply while busy: %v", err)
	}
	asked := w.listed.Load()
	w.busy.Store(false)
	if u.scheduled(t.Context()) || w.restarts.Load() != 1 {
		t.Errorf("not installed once free: restarts %d", w.restarts.Load())
	}
	// What was staged is installed; the listing was not asked again for it.
	if w.listed.Load() != asked {
		t.Errorf("the listing was read again for a release already staged")
	}
}

func TestAFailedInstallationStillStartsTheDaemonAgain(t *testing.T) {
	w := newWorld(t)
	u := w.updater("0.1.0")
	u.scheduled(t.Context())
	// The installed command vanishes before the run ends, so the pair cannot be kept and nothing is swapped.
	if err := os.Remove(filepath.Join(w.bin, cliName)); err != nil {
		t.Fatal(err)
	}
	if !u.Finish(notStopped) {
		t.Error("a run that ended to install, and could not, did not ask to be started again")
	}
	if w.daemonOnDisk() != "0.1.0 norite-daemon" {
		t.Errorf("on disk: %q", w.daemonOnDisk())
	}
}

func TestOffAsksNobodyAnything(t *testing.T) {
	w := newWorld(t)
	lower(t, &firstCheck, 10*time.Millisecond)
	lower(t, &checkJitter, 0)
	lower(t, &checkEvery, 20*time.Millisecond)
	u := w.updater("0.1.0")
	if _, err := u.SetMode(ipc.UpdateOff); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { u.Run(ctx); close(done) }()
	time.Sleep(200 * time.Millisecond)
	if n := w.listed.Load(); n != 0 {
		t.Errorf("with updates off the listing was read %d times", n)
	}
	// Asked outright, it still answers.
	if s, err := u.Check(t.Context()); err != nil || s.Available != "0.1.1" {
		t.Errorf("a check asked for: %+v, %v", s, err)
	}
	if _, err := u.SetMode(ipc.UpdateNotify); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for w.listed.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if w.listed.Load() < 3 {
		t.Errorf("with updates on the schedule read the listing %d times", w.listed.Load())
	}
}

func TestTheFirstCheckWaitsOutARecentOne(t *testing.T) {
	w := newWorld(t)
	u := w.updater("0.1.0")
	if wait := u.untilFirstCheck(); wait > firstCheck+checkJitter {
		t.Errorf("never checked, and waits %s", wait)
	}
	if _, err := u.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A daemon restarted in a loop does not ask the host at every start.
	if wait := w.updater("0.1.0").untilFirstCheck(); wait < 23*time.Hour {
		t.Errorf("checked a moment ago, and waits only %s", wait)
	}
}

func TestAListingThatCannotBeReadIsSaidAndNothingElse(t *testing.T) {
	w := newWorld(t)
	w.listing.Store("<html>rate limited \x1b[2J</html>")
	u := w.updater("0.1.0")
	// Not a refusal: a script tells "nothing newer" from "could not find out" (M24 /code-review).
	_, err := u.Check(t.Context())
	var unreachable *ErrUnreachable
	if !errors.As(err, &unreachable) || strings.ContainsRune(unreachable.Msg, 0x1b) {
		t.Errorf("check: %v", err)
	}
	if s := u.Status(); s.LastError == "" || strings.ContainsRune(s.LastError, 0x1b) || s.Available != "" {
		t.Errorf("status %+v", s)
	}
	if w.changes.Load() == 0 {
		t.Errorf("nobody was told the status changed")
	}
}

func TestModeAndChannelTakeOnlyTheirWords(t *testing.T) {
	w := newWorld(t)
	u := w.updater("0.1.0")
	for _, word := range []string{"", "AUTO", "on", "always", "../off"} {
		if _, err := u.SetMode(word); err == nil {
			t.Errorf("mode %q accepted", word)
		}
		if _, err := u.SetChannel(word); err == nil {
			t.Errorf("channel %q accepted", word)
		}
	}
	w.listing.Store(`[{"tag_name":"v0.1.2-rc.1"}]`)
	if s, _ := u.Check(t.Context()); s.Available != "" {
		t.Errorf("a stable install was offered %s", s.Available)
	}
	if _, err := u.SetChannel(ipc.ChannelPrerelease); err != nil {
		t.Fatal(err)
	}
	if s, _ := u.Check(t.Context()); s.Available != "0.1.2-rc.1" || s.Channel != ipc.ChannelPrerelease {
		t.Errorf("opted in: %+v", s)
	}
	// Going back forgets what the other channel offered.
	if s, _ := u.SetChannel(ipc.ChannelStable); s.Available != "" {
		t.Errorf("back on stable, still offered %s", s.Available)
	}
}

func TestARolledBackVersionIsSaidAndNotOfferedAgain(t *testing.T) {
	w := newWorld(t)
	u := w.updater("0.1.0")
	u.scheduled(t.Context())
	u.Finish(notStopped)

	// A 0.1.1 that is not the installed daemon, a build somebody is running from a checkout, is not the one
	// on trial: however it starts, it counts for nothing and writes over nothing.
	elsewhere := func(o *Options) { o.Executable = "/home/somebody/src/bin/norite-daemon" }
	for i := 0; i < 4; i++ {
		if w.updater("0.1.1", elsewhere).AtStart(true) {
			t.Fatal("a daemon that is not the installed one undid the installed one")
		}
	}
	if w.daemonOnDisk() != newDaemonScript {
		t.Fatalf("on disk: %q", w.daemonOnDisk())
	}

	// 0.1.1 starts three times and never stays up. The third start undoes it.
	for i, want := range []bool{false, false, true} {
		if got := w.updater("0.1.1").AtStart(false); got != want {
			t.Fatalf("start %d of the new version: restart=%v", i+1, got)
		}
	}
	if w.daemonOnDisk() != "0.1.0 norite-daemon" {
		t.Fatalf("on disk after the rollback: %q", w.daemonOnDisk())
	}
	back := w.updater("0.1.0")
	if back.AtStart(false) {
		t.Fatal("the restored version was asked to restart")
	}
	restarts := w.restarts.Load()
	back.scheduled(t.Context())
	if s := back.Status(); s.RolledBack != "0.1.1" || s.Available != "" || w.restarts.Load() != restarts {
		t.Errorf("after a rollback: %+v", s)
	}
}

// ---------- what /code-review found ----------

// A home or a bin directory reached through a link: the daemon's own path arrives resolved and the
// receipt's as the installer typed it.
func TestAnInstallReachedThroughALinkStillUpdatesItself(t *testing.T) {
	w := newWorld(t)
	link := filepath.Join(filepath.Dir(w.bin), "linked-bin")
	if err := os.Symlink(w.bin, link); err != nil {
		t.Fatal(err)
	}
	if err := installreceipt.Write(w.state, installreceipt.Receipt{BinDir: link, By: "test"}); err != nil {
		t.Fatal(err)
	}
	if u := w.updater("0.1.0"); !u.installs {
		t.Errorf("a receipt naming the directory through a link: %s", u.reason)
	}
	// Windows names files without regard to case; nothing else does.
	if !samePath(`C:\Users\Ada\norite-daemon.exe`, `c:\users\ada\NORITE-DAEMON.EXE`, "windows") {
		t.Error("two spellings of one Windows path were told apart")
	}
	if samePath("/home/ada/bin/norite-daemon", "/home/ada/BIN/norite-daemon", "linux") {
		t.Error("two Linux paths differing in case were taken for one")
	}
}

// The record outlives the daemon that wrote it. After somebody runs the installer by hand, the version a
// check called available is the one running.
func TestAVersionAlreadyRunningIsNotAnnouncedAsOut(t *testing.T) {
	w := newWorld(t)
	old := w.updater("0.1.0")
	if _, err := old.SetMode(ipc.UpdateNotify); err != nil {
		t.Fatal(err)
	}
	if s, err := old.Check(t.Context()); err != nil || s.Available != "0.1.1" {
		t.Fatalf("check: %+v, %v", s, err)
	}
	for _, running := range []string{"0.1.1", "0.1.2"} {
		if s := w.updater(running).Status(); s.Available != "" {
			t.Errorf("a daemon at %s says %s is out", running, s.Available)
		}
	}
	if s := w.updater("0.1.0").Status(); s.Available != "0.1.1" {
		t.Errorf("the version still below it lost the notice: %+v", s)
	}
}

func TestARefusedHandshakeAsksForACheckAtOnce(t *testing.T) {
	w := newWorld(t)
	lower(t, &firstCheck, time.Hour)
	lower(t, &checkJitter, 0)
	u := w.updater("0.1.0")
	if _, err := u.SetMode(ipc.UpdateNotify); err != nil {
		t.Fatal(err)
	}
	u.unasked()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { u.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	time.Sleep(100 * time.Millisecond)
	if w.listed.Load() != 0 {
		t.Fatal("the listing was read before anything asked")
	}
	u.Nudge()
	u.Nudge() // twice before the loop looks is once
	deadline := time.Now().Add(5 * time.Second)
	for w.listed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if w.listed.Load() != 1 {
		t.Fatalf("after a refused handshake the listing was read %d times", w.listed.Load())
	}
	// An instance that refuses in a loop asks the host once, not once a refusal.
	for i := 0; i < 5; i++ {
		u.Nudge()
		time.Sleep(30 * time.Millisecond)
	}
	if n := w.listed.Load(); n != 1 {
		t.Errorf("refused again a moment later, the listing was read %d times", n)
	}
}

// With updates off, a refused handshake asks nobody anything either.
func TestARefusedHandshakeAsksNothingWithUpdatesOff(t *testing.T) {
	w := newWorld(t)
	lower(t, &firstCheck, time.Hour)
	lower(t, &checkJitter, 0)
	u := w.updater("0.1.0")
	if _, err := u.SetMode(ipc.UpdateOff); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { u.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	u.Nudge()
	time.Sleep(200 * time.Millisecond)
	if n := w.listed.Load(); n != 0 {
		t.Errorf("with updates off a refused handshake read the listing (%d)", n)
	}
}

// A request does not wait out a download the schedule is making.
func TestARequestSaysWhenACheckIsAlreadyUnderWay(t *testing.T) {
	w := newWorld(t)
	lower(t, &requestWait, 50*time.Millisecond)
	u := w.updater("0.1.0")
	if !u.acquire(t.Context(), 0) {
		t.Fatal("the turn could not be taken")
	}
	started := time.Now()
	for name, ask := range map[string]func(context.Context) (ipc.UpdateStatus, error){"check": u.Check, "apply": u.Apply} {
		// Not a refusal: a script that tries again on "unavailable" and stops on "refused" must not stop
		// for a daemon that was busy for a moment.
		_, err := ask(t.Context())
		var later *ErrUnreachable
		var refused *ErrRefused
		if !errors.As(err, &later) || errors.As(err, &refused) || !strings.Contains(later.Msg, "already under way") {
			t.Errorf("%s behind a download: %v", name, err)
		}
	}
	if time.Since(started) > 2*time.Second {
		t.Errorf("the two requests waited %s", time.Since(started))
	}
	u.release()
	if _, err := u.Check(t.Context()); err != nil {
		t.Errorf("once it is free: %v", err)
	}
	if w.restarts.Load() != 0 {
		t.Error("a refused request restarted the daemon")
	}
}

// A signed-in daemon that has not heard from its instance in this run moves nowhere.
func TestNothingIsChosenUntilTheInstanceHasBeenHeardFrom(t *testing.T) {
	w := newWorld(t)
	w.signedIn.Store(true)
	u := w.updater("0.1.0")
	u.scheduled(t.Context())
	if s := u.Status(); s.Available != "" || w.restarts.Load() != 0 {
		t.Fatalf("moved before the instance said its version: %+v", s)
	}
	w.instance.Store("0.1.0")
	u.scheduled(t.Context())
	if w.restarts.Load() != 1 {
		t.Errorf("heard from, and still not moved")
	}
}

// A release host's words are bounded before they are kept. An error names the address a redirect sent
// the request to, which is the host's to choose and megabytes long if it likes: kept whole as the last
// error, it made a record too large to be read back, and the mode, the channel and every version rolled
// back from were forgotten (M24 /security-sweep).
func TestAHostsWordsCannotOutgrowTheRecord(t *testing.T) {
	w := newWorld(t)
	u := w.updater("0.1.0")
	if _, err := u.SetMode(ipc.UpdateNotify); err != nil {
		t.Fatal(err)
	}
	if err := u.installer.Update(func(r *Record) { r.Bad = []string{"0.1.1"} }); err != nil {
		t.Fatal(err)
	}
	w.host.server.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		// Somewhere nothing listens, with a megabyte and a half of "<", which JSON writes six bytes for.
		http.Redirect(rw, r, "http://127.0.0.1:1/"+strings.Repeat("<", 1500<<10), http.StatusFound)
	})
	_, err := u.Check(context.Background())
	if err == nil {
		t.Fatal("a listing that redirects nowhere was read")
	}
	if len(err.Error()) > 1024 {
		t.Errorf("the refusal is %d bytes of the host's words", len(err.Error()))
	}
	r := u.installer.Load()
	if r.Mode != ipc.UpdateNotify || len(r.Bad) != 1 {
		t.Errorf("the record forgot what it held: mode %q, bad %v", r.Mode, r.Bad)
	}
	if r.LastError == "" || len(r.LastError) > 512 {
		t.Errorf("the last error is %d bytes", len(r.LastError))
	}
}

// An install that cannot be written over is found out before anything is downloaded and before the
// daemon stops: it used to download, stop, fail and start again once a day (M24 /code-review).
func TestAnInstallThatCannotBeReplacedIsNotStoppedFor(t *testing.T) {
	w := newWorld(t)
	// The command is a link to a program kept somewhere else, as a dotfiles manager leaves it.
	elsewhere := filepath.Join(t.TempDir(), "norite")
	if err := os.Rename(filepath.Join(w.bin, cliName), elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(w.bin, cliName)); err != nil {
		t.Fatal(err)
	}
	u := w.updater("0.1.0")
	if u.scheduled(t.Context()) {
		t.Errorf("a release was left waiting")
	}
	if w.restarts.Load() != 0 {
		t.Errorf("the daemon was stopped for an installation that could not happen")
	}
	for name, n := range w.host.asked {
		if n.Load() != 0 {
			t.Errorf("%s was downloaded for it", name)
		}
	}
	if s := u.Status(); !strings.Contains(s.LastError, "not a regular file") || s.Available != "0.1.1" {
		t.Errorf("status %+v", s)
	}
	if _, err := u.Apply(t.Context()); err == nil || !strings.Contains(err.Error(), "cannot be installed here") {
		t.Errorf("apply: %v", err)
	}
	if entries, _ := os.ReadDir(w.bin); len(entries) != 2 {
		t.Errorf("the check left something in the bin directory: %v", entries)
	}
}

// A last check dated in the future is a clock that was set back since. It holds a refused handshake's
// check off no more than no check at all does: it used to hold it off until the clock caught up.
func TestAClockSetBackDoesNotHoldOffARefusedHandshakesCheck(t *testing.T) {
	w := newWorld(t)
	lower(t, &firstCheck, time.Hour)
	lower(t, &checkJitter, 0)
	u := w.updater("0.1.0")
	if err := u.installer.Update(func(r *Record) { r.LastCheck = time.Now().Add(48 * time.Hour) }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { u.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	u.Nudge()
	deadline := time.Now().Add(5 * time.Second)
	for w.listed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if w.listed.Load() != 1 {
		t.Errorf("the listing was read %d times", w.listed.Load())
	}
}

// notStopped is a daemon nobody has asked to stop.
func notStopped() bool { return false }

// An install that is itself a pre-release is offered the next one without being told to: the installer
// installs the newest release of any kind, and a release candidate never offered its successor stayed
// locked out of an instance that had moved to it (M24 /code-review). Saying "stable" still holds.
func TestAPreReleaseInstallIsOfferedPreReleases(t *testing.T) {
	w := newWorld(t)
	w.listing.Store(`[{"tag_name":"v0.1.0-rc.2"},{"tag_name":"v0.1.0-rc.1"}]`)
	w.instance.Store("0.1.0-rc.2")
	w.signedIn.Store(true)
	u := w.updater("0.1.0-rc.1")
	if s, err := u.Check(t.Context()); err != nil || s.Available != "0.1.0-rc.2" || s.Channel != ipc.ChannelPrerelease {
		t.Errorf("a release candidate was not offered the next: %+v (%v)", s, err)
	}
	if _, err := u.SetChannel(ipc.ChannelStable); err != nil {
		t.Fatal(err)
	}
	if s, _ := u.Check(t.Context()); s.Available != "" || s.Channel != ipc.ChannelStable {
		t.Errorf("told stable, it was offered a pre-release: %+v", s)
	}
	// A release is offered releases only, as before.
	if s := w.updater("0.1.0").Status(); s.Channel != ipc.ChannelStable {
		t.Errorf("a release's channel is %q", s.Channel)
	}
}

// On Windows nothing is installed unasked until somebody has watched it work there: the default is to
// say so and wait. Asking still installs, and so does choosing auto.
func TestWindowsWaitsToBeAsked(t *testing.T) {
	w := newWorld(t)
	onWindows := func(o *Options) {
		o.GOOS = "windows"
		o.Executable = filepath.Join(w.bin, daemonName+".exe")
	}
	for _, name := range []string{cliName, daemonName} {
		if err := os.Rename(filepath.Join(w.bin, name), filepath.Join(w.bin, name+".exe")); err != nil {
			t.Fatal(err)
		}
	}
	u := w.updater("0.1.0", onWindows)
	if s := u.Status(); !s.Installs || s.Mode != ipc.UpdateNotify {
		t.Fatalf("on Windows: %+v", s)
	}
	u.scheduled(t.Context())
	if w.restarts.Load() != 0 {
		t.Errorf("a release was installed on Windows without anybody asking")
	}
	if s, err := u.SetMode(ipc.UpdateAuto); err != nil || s.Mode != ipc.UpdateAuto {
		t.Errorf("choosing auto on Windows: %+v (%v)", s, err)
	}
	if s := w.updater("0.1.0").Status(); s.Mode != ipc.UpdateAuto {
		t.Errorf("elsewhere the default is %q", s.Mode)
	}
}

// A daemon told to stop stops. Stopping and updating end the run the same way, and a stop that arrived
// once an update had begun used to be taken for part of it: the daemon started again.
func TestAStopThatArrivesDuringAnUpdateIsAStop(t *testing.T) {
	w := newWorld(t)
	u := w.updater("0.1.0")
	u.scheduled(t.Context())
	if w.restarts.Load() != 1 {
		t.Fatal("the update did not begin")
	}
	if u.Finish(func() bool { return true }) {
		t.Error("a daemon asked to stop started again")
	}
	if w.daemonOnDisk() != "0.1.0 norite-daemon" {
		t.Errorf("installed anyway: %q", w.daemonOnDisk())
	}
	// Asked while the programs were being replaced: they are in place, and it still stops.
	w2 := newWorld(t)
	u2 := w2.updater("0.1.0")
	u2.scheduled(t.Context())
	calls := 0
	if u2.Finish(func() bool { calls++; return calls > 1 }) {
		t.Error("a daemon asked to stop while installing started again")
	}
	if w2.daemonOnDisk() != newDaemonScript {
		t.Errorf("on disk: %q", w2.daemonOnDisk())
	}
	// And a run already ending is not ended again to install. Tried many times, since a turn taken with
	// a context already done is taken only some of the time.
	w3 := newWorld(t)
	w3.busy.Store(true)
	u3 := w3.updater("0.1.0")
	if !u3.scheduled(t.Context()) {
		t.Fatal("nothing was staged")
	}
	w3.busy.Store(false)
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	for i := 0; i < 40; i++ {
		u3.scheduled(ended)
	}
	if w3.restarts.Load() != 0 {
		t.Errorf("a run that was ending was ended again to install")
	}
}

// A record that cannot be read turns nothing on: no mode can be read from it, and "off" may be what it
// said. It is said, and left for a person.
func TestAnUnreadableRecordChecksAndInstallsNothing(t *testing.T) {
	w := newWorld(t)
	u := w.updater("0.1.0")
	if _, err := u.SetMode(ipc.UpdateOff); err != nil {
		t.Fatal(err)
	}
	broken := `{"mode":"off","bad":["0.1.1"]`
	if err := os.WriteFile(u.installer.recordPath(), []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	s := u.Status()
	if s.Mode != ipc.UpdateOff || !strings.Contains(s.LastError, "cannot be read") {
		t.Errorf("status %+v", s)
	}
	lower(t, &firstCheck, 10*time.Millisecond)
	lower(t, &checkJitter, 0)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { u.Run(ctx); close(done) }()
	u.Nudge()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done
	if w.listed.Load() != 0 || w.restarts.Load() != 0 {
		t.Errorf("listed %d, restarted %d", w.listed.Load(), w.restarts.Load())
	}
	if got, _ := os.ReadFile(u.installer.recordPath()); string(got) != broken {
		t.Errorf("the record was written over: %q", got)
	}
}

// What somebody asked for during a call is installed when the call ends: soon, and whatever the mode. It
// used to wait for the next daily check, and never came with updates off.
func TestWhatWasAskedForWhileBusyIsInstalledWhenFree(t *testing.T) {
	w := newWorld(t)
	lower(t, &firstCheck, time.Hour)
	lower(t, &checkJitter, 0)
	lower(t, &retryStaged, 20*time.Millisecond)
	w.busy.Store(true)
	u := w.updater("0.1.0")
	if _, err := u.SetMode(ipc.UpdateOff); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { u.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	if _, err := u.Apply(t.Context()); err == nil || !strings.Contains(err.Error(), "ready") {
		t.Fatalf("apply while busy: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if w.restarts.Load() != 0 {
		t.Fatal("installed during the call")
	}
	w.busy.Store(false)
	deadline := time.Now().Add(5 * time.Second)
	for w.restarts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if w.restarts.Load() != 1 {
		t.Errorf("not installed once the call ended")
	}
}

// A release staged while the mode was auto is not installed by the schedule once somebody has said to
// ask first.
func TestAStagedReleaseIsNotInstalledOnceToldToAskFirst(t *testing.T) {
	w := newWorld(t)
	w.busy.Store(true)
	u := w.updater("0.1.0")
	if !u.scheduled(t.Context()) {
		t.Fatal("nothing was staged")
	}
	if _, err := u.SetMode(ipc.UpdateNotify); err != nil {
		t.Fatal(err)
	}
	w.busy.Store(false)
	u.scheduled(t.Context())
	if w.restarts.Load() != 0 {
		t.Errorf("installed after being told to ask first")
	}
}

// "Once a day" is a day by the clock, not a day of the machine being awake: a timer does not count the
// time a laptop sleeps, and one that sleeps at night checked every third day.
func TestTheScheduleFollowsTheClockAndNotTheTimeAwake(t *testing.T) {
	w := newWorld(t)
	lower(t, &firstCheck, time.Hour)
	lower(t, &checkJitter, 0)
	lower(t, &wake, 20*time.Millisecond)
	var slept atomic.Int64
	u := w.updater("0.1.0", func(o *Options) {
		o.Now = func() time.Time { return time.Now().Add(time.Duration(slept.Load())) }
	})
	if _, err := u.SetMode(ipc.UpdateNotify); err != nil {
		t.Fatal(err)
	}
	u.unasked()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { u.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	time.Sleep(150 * time.Millisecond)
	if w.listed.Load() != 0 {
		t.Fatal("the listing was read before it was due")
	}
	// The machine sleeps two hours: the clock moves and no timer counts it.
	slept.Store(int64(2 * time.Hour))
	deadline := time.Now().Add(5 * time.Second)
	for w.listed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if w.listed.Load() != 1 {
		t.Errorf("after the clock passed the time due, the listing was read %d times", w.listed.Load())
	}
}

// A check or a download cut short because the daemon is stopping, or whoever asked went away, is not a
// check that failed: recorded as one, it put the next off by a day and left "context canceled" as the
// last error after an ordinary stop.
func TestAStopDuringACheckIsNotAFailedCheck(t *testing.T) {
	w := newWorld(t)
	u := w.updater("0.1.0")
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	for i := 0; i < 40; i++ {
		u.scheduled(gone)
	}
	if _, _, err := u.check(gone); err == nil {
		t.Fatal("a check with nobody waiting read the listing")
	}
	if r := u.installer.Load(); !r.LastCheck.IsZero() || r.LastError != "" {
		t.Errorf("recorded as a failed check: %+v", r)
	}
}

// What `norite update apply` started is the daemon's to finish. Given only as long as the request waits
// and thrown away after, a download on a slow link never finished, and with the mode on notify asking is
// the only way a release is installed.
func TestAnInstallationAskedForOutlastsTheRequest(t *testing.T) {
	w := newWorld(t)
	lower(t, &applyTimeout, 50*time.Millisecond)
	slow := make(chan struct{})
	inner := w.host.server.Config.Handler
	w.host.server.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".tar.gz") {
			<-slow
		}
		inner.ServeHTTP(rw, r)
	})
	u := w.updater("0.1.0")
	if _, err := u.SetMode(ipc.UpdateNotify); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Apply(t.Context()); err != nil {
		t.Fatalf("apply on a slow link: %v", err)
	}
	if w.restarts.Load() != 0 {
		t.Fatal("restarted before the download finished")
	}
	// A turn of the schedule arriving meanwhile neither waits for ever nor drops what was asked for.
	turn := make(chan struct{})
	go func() { u.scheduled(t.Context()); close(turn) }()
	close(slow)
	deadline := time.Now().Add(10 * time.Second)
	for w.restarts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	<-turn
	if w.restarts.Load() != 1 || !u.Finish(notStopped) || w.daemonOnDisk() != newDaemonScript {
		t.Errorf("restarts %d, on disk %q", w.restarts.Load(), w.daemonOnDisk())
	}
}

// A program that is not the installed daemon, holding the state directory, neither confirms a version
// on trial nor takes a start back from it.
func TestOnlyTheInstalledDaemonSettlesATrial(t *testing.T) {
	w := newWorld(t)
	u := w.updater("0.1.0")
	u.scheduled(t.Context())
	if !u.Finish(notStopped) {
		t.Fatal("not installed")
	}
	if w.updater("0.1.1").AtStart(false) {
		t.Fatal("rolled back at its first start")
	}
	lower(t, &confirmAfter, 20*time.Millisecond)
	lower(t, &firstCheck, time.Hour)
	other := w.updater("dev", func(o *Options) { o.Executable = "/somewhere/else/norite-daemon" })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { other.Run(ctx); close(done) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done
	other.Finish(notStopped)
	r := u.installer.Load()
	if r.Pending == nil || r.Pending.Starts != 1 {
		t.Errorf("another program settled the trial: %+v", r.Pending)
	}
	if _, err := os.Stat(filepath.Join(u.installer.previousDir(), daemonName)); err != nil {
		t.Errorf("the pair kept to go back to is gone: %v", err)
	}
}

// A download that fails is the host's failure too, and not a refusal.
func TestADownloadThatFailsIsUnreachableAndNotARefusal(t *testing.T) {
	w := newWorld(t)
	u := w.updater("0.1.0")
	var unreachable *ErrUnreachable
	w.listing.Store(`[{"tag_name":"v0.1.1"}]`)
	w.host.server.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases" {
			_, _ = fmt.Fprint(rw, w.listing.Load().(string))
			return
		}
		http.Error(rw, "gone", http.StatusServiceUnavailable)
	})
	if _, err := u.Apply(t.Context()); !errors.As(err, &unreachable) {
		t.Errorf("apply with the files unreachable: %v", err)
	}
}

// Signed out, a daemon updates by itself, a pre-release included: the running version stands in for the
// instance it has none of. The author's decision, after a review's fix had held a signed-out pre-release
// back until somebody asked.
func TestASignedOutPreReleaseUpdatesByItself(t *testing.T) {
	w := newWorld(t)
	w.listing.Store(`[{"tag_name":"v0.1.1"},{"tag_name":"v0.1.1-rc.2"}]`)
	u := w.updater("0.1.1-rc.1")
	if waiting, again := u.turn(t.Context()); waiting || again != 0 {
		t.Errorf("the turn asks to come back: %v, %v", waiting, again)
	}
	if w.restarts.Load() != 1 || u.Status().Available != "0.1.1" {
		t.Errorf("not moved by itself: restarts %d, status %+v", w.restarts.Load(), u.Status())
	}
}

// Signed in to an instance not heard from yet, a check is not one that found nothing: it is said, nothing
// is recorded, the host is not asked, and the schedule comes back soon.
func TestACheckThatCannotBeMadeYetIsNotOneThatFoundNothing(t *testing.T) {
	w := newWorld(t)
	w.signedIn.Store(true)
	u := w.updater("0.1.0")
	if waiting, again := u.turn(t.Context()); waiting || again != retryStaged {
		t.Errorf("the schedule comes back in %v", again)
	}
	// Something to ask again, and so not a refusal; and said as itself, not as a listing unread.
	_, err := u.Check(t.Context())
	var later *ErrUnreachable
	if !errors.As(err, &later) || !strings.HasPrefix(later.Msg, "this daemon is signed in") {
		t.Errorf("check: %v", err)
	}
	if _, err := u.Apply(t.Context()); !errors.As(err, &later) || !strings.HasPrefix(later.Msg, "this daemon is signed in") {
		t.Errorf("apply: %v", err)
	}
	if r := u.installer.Load(); !r.LastCheck.IsZero() || w.listed.Load() != 0 {
		t.Errorf("recorded as a check, or the host was asked: %+v, listed %d", r, w.listed.Load())
	}
	// And a listing that could not be read is asked for again in an hour, not a day.
	w.instance.Store("0.1.0")
	w.listing.Store("not a listing")
	if _, again := u.turn(t.Context()); again != retryFailed {
		t.Errorf("after a failed listing the schedule comes back in %v", again)
	}
}

// With a record that cannot be read, asking installs nothing: it used to download, stop the daemon, fail
// and start it again.
func TestAskingWithAnUnreadableRecordIsRefusedAtOnce(t *testing.T) {
	w := newWorld(t)
	u := w.updater("0.1.0")
	if err := os.MkdirAll(u.installer.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(u.installer.recordPath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	var refused *ErrRefused
	for name, ask := range map[string]func(context.Context) (ipc.UpdateStatus, error){"check": u.Check, "apply": u.Apply} {
		if _, err := ask(t.Context()); !errors.As(err, &refused) || !strings.Contains(refused.Msg, "cannot be read") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if w.listed.Load() != 0 || w.restarts.Load() != 0 {
		t.Errorf("listed %d, restarts %d", w.listed.Load(), w.restarts.Load())
	}
}

// A refused handshake that asks inside the floor is answered when the floor is past, and not dropped.
func TestARefusedHandshakeInsideTheFloorIsDeferredAndNotDropped(t *testing.T) {
	w := newWorld(t)
	lower(t, &firstCheck, time.Hour)
	lower(t, &checkJitter, 0)
	lower(t, &nudgeFloor, 300*time.Millisecond)
	u := w.updater("0.1.0")
	if _, err := u.SetMode(ipc.UpdateNotify); err != nil {
		t.Fatal(err)
	}
	u.unasked()
	if _, err := u.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { u.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	u.Nudge()
	time.Sleep(100 * time.Millisecond)
	if w.listed.Load() != 1 {
		t.Fatalf("asked again inside the floor (%d)", w.listed.Load())
	}
	deadline := time.Now().Add(5 * time.Second)
	for w.listed.Load() == 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if w.listed.Load() != 2 {
		t.Errorf("the check asked for inside the floor never came")
	}
}

// scheduled is one turn of the loop, for a test that asks only whether a release is left waiting.
func (u *Updater) scheduled(ctx context.Context) bool {
	waiting, _ := u.turn(ctx)
	return waiting
}

// The time of the last check is the last time the listing was read. A failure dated there made a start
// after it wait out a day, and the status say "no newer release, as of" a moment nothing was learned at.
func TestAFailedCheckIsNotTheLastCheck(t *testing.T) {
	w := newWorld(t)
	w.listing.Store("<html>offline</html>")
	u := w.updater("0.1.0")
	if _, err := u.Check(t.Context()); err == nil {
		t.Fatal("a listing that is not one was read")
	}
	if s := u.Status(); s.LastCheck != "" || s.LastError == "" {
		t.Errorf("status %+v", s)
	}
	if wait := w.updater("0.1.0").untilFirstCheck(); wait > firstCheck+checkJitter {
		t.Errorf("after a check that failed, the next start waits %s", wait)
	}
}

// A refused handshake's check that failed still counts against the floor, in the run that made it.
func TestAFailedCheckStillHoldsOffTheNextRefusedHandshake(t *testing.T) {
	w := newWorld(t)
	lower(t, &firstCheck, time.Hour)
	lower(t, &checkJitter, 0)
	w.listing.Store("<html>offline</html>")
	u := w.updater("0.1.0")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { u.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	u.Nudge()
	deadline := time.Now().Add(5 * time.Second)
	for w.listed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; i < 5; i++ {
		u.Nudge()
		time.Sleep(30 * time.Millisecond)
	}
	if n := w.listed.Load(); n != 1 {
		t.Errorf("refused in a loop with the host unreadable, the listing was asked for %d times", n)
	}
}

// A daemon refused for being ahead of its instance has no release to find, and reads no listing for it.
func TestARefusedHandshakeAheadOfTheInstanceAsksNothing(t *testing.T) {
	w := newWorld(t)
	lower(t, &firstCheck, time.Hour)
	lower(t, &checkJitter, 0)
	w.signedIn.Store(true)
	w.instance.Store("0.1.3")
	u := w.updater("0.2.0")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { u.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	u.Nudge()
	time.Sleep(150 * time.Millisecond)
	if n := w.listed.Load(); n != 0 {
		t.Errorf("ahead of its instance, the listing was read %d times", n)
	}
	// Behind it, the listing is what it needs.
	w.instance.Store("0.3.0")
	u.Nudge()
	deadline := time.Now().Add(5 * time.Second)
	for w.listed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := w.listed.Load(); n != 1 {
		t.Errorf("behind its instance, the listing was read %d times", n)
	}
}

// A download that dropped is tried again in an hour, as a listing that could not be read is.
func TestADownloadThatFailsIsTriedAgainSoon(t *testing.T) {
	w := newWorld(t)
	w.host.server.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases" {
			_, _ = fmt.Fprint(rw, w.listing.Load().(string))
			return
		}
		http.Error(rw, "gone", http.StatusServiceUnavailable)
	})
	u := w.updater("0.1.0")
	if waiting, again := u.turn(t.Context()); waiting || again != retryFailed {
		t.Errorf("waiting=%v again=%s", waiting, again)
	}
}

// A version on trial is not installed over: its predecessor is what a rollback restores, and installing
// replaces the pair kept before anything is swapped.
func TestAVersionOnTrialIsNotInstalledOver(t *testing.T) {
	w := newWorld(t)
	u := w.updater("0.1.0")
	u.scheduled(t.Context())
	if !u.Finish(notStopped) {
		t.Fatal("not installed")
	}
	trial := w.updater("0.1.1")
	if trial.AtStart(false) {
		t.Fatal("rolled back at its first start")
	}
	w.listing.Store(`[{"tag_name":"v0.1.2"},{"tag_name":"v0.1.1"}]`)
	fetched, listed := w.host.asked["checksums.txt"].Load(), w.listed.Load()
	if waiting, again := trial.turn(t.Context()); waiting || again != retryStaged {
		t.Errorf("on trial: waiting=%v again=%s", waiting, again)
	}
	// Nor is the listing read, every five minutes, for a release that could not be acted on.
	if w.listed.Load() != listed {
		t.Error("the listing was read for a version on trial")
	}
	var refused *ErrRefused
	if _, err := trial.Apply(t.Context()); !errors.As(err, &refused) || !strings.Contains(err.Error(), "just installed") {
		t.Errorf("apply on trial: %v", err)
	}
	if w.restarts.Load() != 1 || w.host.asked["checksums.txt"].Load() != fetched {
		t.Errorf("a version on trial was stopped or downloaded for")
	}
	kept, err := os.ReadFile(filepath.Join(trial.installer.previousDir(), daemonName))
	if err != nil || string(kept) != "0.1.0 norite-daemon" {
		t.Errorf("the pair kept to go back to: %q, %v", kept, err)
	}
	// The trial is this daemon's own: another build reading the record is not "just installed".
	if s := trial.Status(); s.Trial != "0.1.1" {
		t.Errorf("status on trial: %+v", s)
	}
	if s := w.updater("0.1.3").Status(); s.Trial != "" {
		t.Errorf("a version the record does not name says it is on trial: %+v", s)
	}
	// Once kept it moves on. The host here has no files for 0.1.2, so moving on shows as a download tried.
	if err := trial.installer.Confirm(); err != nil {
		t.Fatal(err)
	}
	if _, again := trial.turn(t.Context()); again != retryFailed {
		t.Errorf("once kept, the next turn tried nothing: again=%s", again)
	}
}

// The request's wait is counted from when it arrived, and not from after the listing was read: on a slow
// link the two together outlasted the client, which then said a healthy daemon had not answered.
//
// Held by the clock and not by a stopwatch: the listing takes two hours of the daemon's clock, and the
// request has one to answer in. Counted from arrival there is nothing left to wait; counted from after the
// listing, the answer is an hour away.
func TestAnInstallationAskedForAnswersWithinItsOwnTime(t *testing.T) {
	w := newWorld(t)
	lower(t, &applyTimeout, time.Hour)
	var passed atomic.Int64
	release := make(chan struct{})
	inner := w.host.server.Config.Handler
	w.host.server.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/releases":
			passed.Store(int64(2 * time.Hour))
		case strings.HasSuffix(r.URL.Path, ".tar.gz"):
			<-release
		}
		inner.ServeHTTP(rw, r)
	})
	u := w.updater("0.1.0", func(o *Options) {
		o.Now = func() time.Time { return time.Now().Add(time.Duration(passed.Load())) }
	})
	answered := make(chan ipc.UpdateStatus, 1)
	go func() {
		st, err := u.Apply(t.Context())
		if err != nil {
			t.Errorf("apply: %v", err)
		}
		answered <- st
	}()
	select {
	case st := <-answered:
		// And the answer says the release is still on its way, not that it is being installed.
		if st.Staging != "0.1.1" {
			t.Errorf("answered mid-download with %+v", st)
		}
	case <-time.After(10 * time.Second):
		t.Error("the request was still waiting, with its hour already spent on the listing")
	}
	// The download is the daemon's and outlives the request: it is let finish before the test's directory
	// goes, which it would otherwise be unpacking into.
	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for w.restarts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if w.restarts.Load() != 1 || u.Status().Staging != "" {
		t.Errorf("the download did not finish: restarts=%d, status %+v", w.restarts.Load(), u.Status())
	}
}

// A release that cannot be staged is given up on for the run, and the newest of the others is installed.
// Only a signature that failed used to do that: a release published without its files was fetched every
// hour for good, and the release before it was never installed.
func TestAReleaseThatCannotBeFetchedGivesWayToTheOneBeforeIt(t *testing.T) {
	w := newWorld(t)
	w.listing.Store(`[{"tag_name":"v0.1.2"},{"tag_name":"v0.1.1"}]`)
	u := w.updater("0.1.0")
	for try := 1; try < maxDownloads; try++ {
		if waiting, again := u.turn(t.Context()); waiting || again != retryFailed {
			t.Fatalf("try %d: waiting=%v again=%s", try, waiting, again)
		}
		if s := u.Status(); s.Available != "0.1.2" {
			t.Fatalf("try %d: %+v", try, s)
		}
	}
	// The last try gives up on it, and the next turn comes soon.
	if waiting, again := u.turn(t.Context()); waiting || again != retryStaged {
		t.Fatalf("the last try: waiting=%v again=%s", waiting, again)
	}
	if s := u.Status(); s.Available != "" || s.LastError == "" {
		t.Errorf("given up on, and still announced or unexplained: %+v", s)
	}
	if u.scheduled(t.Context()) || w.restarts.Load() != 1 || !u.Finish(notStopped) || w.daemonOnDisk() != newDaemonScript {
		t.Errorf("the release before it was not installed: restarts=%d", w.restarts.Load())
	}
}

// What was refused earlier is named only while the listing still offers it.
func TestARefusedReleaseThatIsNoLongerListedIsNotNamed(t *testing.T) {
	w := newWorld(t)
	w.verifyErr.Store(fmt.Errorf("%w: test", ErrNotVerified))
	u := w.updater("0.1.0")
	if _, err := u.Apply(t.Context()); err == nil {
		t.Fatal("an unverified release was installed")
	}
	w.listing.Store(`[{"tag_name":"v0.1.0"}]`)
	_, err := u.Apply(t.Context())
	var refused *ErrRefused
	if !errors.As(err, &refused) || refused.Msg != "there is no newer release this install can move to" {
		t.Errorf("apply: %v", err)
	}
	if s := u.Status(); s.LastError != "" {
		t.Errorf("the reason for a release no longer offered is still the last error: %+v", s)
	}
}

// A mode or a channel that was just set is acted on now, and not at the next daily turn.
func TestAChoiceJustMadeIsActedOnNow(t *testing.T) {
	w := newWorld(t)
	lower(t, &firstCheck, time.Hour)
	lower(t, &checkJitter, 0)
	u := w.updater("0.1.0")
	if err := u.installer.Update(func(r *Record) { r.Mode = ipc.UpdateNotify }); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { u.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	// The channel's own answer is asked for, and until it is, the other channel's is not given as its.
	listed := w.listed.Load()
	st, err := u.SetChannel(ipc.ChannelPrerelease)
	if err != nil || st.LastCheck != "" || st.Available != "" {
		t.Errorf("a channel just set: %+v, %v", st, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for w.listed.Load() == listed && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if w.listed.Load() == listed || w.restarts.Load() != 0 {
		t.Fatalf("after the channel was set: listed %d times more, restarts %d", w.listed.Load()-listed, w.restarts.Load())
	}
	// "auto", with a release out, installs it.
	if _, err := u.SetMode(ipc.UpdateAuto); err != nil {
		t.Fatal(err)
	}
	for w.restarts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if w.restarts.Load() != 1 {
		t.Errorf("told to install by itself with a release out, it waited")
	}
}

// "auto" chosen by an install that no longer installs is not said as the mode in force.
func TestAutoChosenByAnInstallThatNoLongerInstallsIsNotInForce(t *testing.T) {
	w := newWorld(t)
	if _, err := w.updater("0.1.0").SetMode(ipc.UpdateAuto); err != nil {
		t.Fatal(err)
	}
	elsewhere := w.updater("0.1.0", func(o *Options) { o.Executable = "/usr/bin/norite-daemon" })
	if s := elsewhere.Status(); s.Installs || s.Mode != ipc.UpdateNotify {
		t.Errorf("status %+v", s)
	}
}

// unasked takes back the turn SetMode and SetChannel ask for, for a test of the schedule alone.
func (u *Updater) unasked() {
	select {
	case <-u.changed:
	default:
	}
}
