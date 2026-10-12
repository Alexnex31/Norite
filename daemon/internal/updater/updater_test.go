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
	if !u.Finish() {
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
	if u.Finish() {
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
	if w.restarts.Load() != 1 || !u.Finish() || w.daemonOnDisk() != newDaemonScript {
		t.Errorf("apply: restarts=%d, on disk %q", w.restarts.Load(), w.daemonOnDisk())
	}
}

func TestAnUnverifiedReleaseIsRefusedAndTheVersionKeepsRunning(t *testing.T) {
	w := newWorld(t)
	w.verifyErr.Store(fmt.Errorf("%w: test", ErrNotVerified))
	u := w.updater("0.1.0")
	if waiting := u.scheduled(t.Context()); waiting {
		t.Error("a refused release is waiting to be installed")
	}
	_, err := u.Apply(t.Context())
	var refused *ErrRefused
	if !errors.As(err, &refused) || !strings.Contains(refused.Msg, "keeps running") {
		t.Errorf("apply: %v", err)
	}
	if w.restarts.Load() != 0 || u.Finish() || w.daemonOnDisk() != "0.1.0 norite-daemon" {
		t.Errorf("an unverified release changed something")
	}
	if s := u.Status(); !strings.Contains(s.LastError, "did not verify") {
		t.Errorf("last error %q", s.LastError)
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
	u := w.updater("0.1.0")
	u.scheduled(t.Context())
	if s := u.Status(); s.Available != "" || w.restarts.Load() != 0 {
		t.Errorf("moved within 0.1 while signed in to a 0.2 instance: %+v", s)
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
	if !u.Finish() {
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
	_, err := u.Check(t.Context())
	var refused *ErrRefused
	if !errors.As(err, &refused) || strings.ContainsRune(refused.Msg, 0x1b) {
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
	u.Finish()

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
		_, err := ask(t.Context())
		var refused *ErrRefused
		if !errors.As(err, &refused) || !strings.Contains(refused.Msg, "already under way") {
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
