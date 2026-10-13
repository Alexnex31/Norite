// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// installed is an Installer over a bin directory holding version 0.1.0's pair, and a release 0.1.1 staged
// beside it.
type installed struct {
	t      *testing.T
	in     *Installer
	staged Staged
}

func newInstalled(t *testing.T) *installed {
	t.Helper()
	root := t.TempDir()
	in := &Installer{Dir: filepath.Join(root, "update"), BinDir: filepath.Join(root, "bin"), GOOS: runtime.GOOS}
	stage := filepath.Join(in.Dir, "staged", "v0.1.1")
	for _, dir := range []string{in.BinDir, stage} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	x := &installed{t: t, in: in}
	for _, name := range in.programs() {
		x.write(filepath.Join(in.BinDir, name), "0.1.0 "+name)
		x.write(filepath.Join(stage, name), "0.1.1 "+name)
	}
	x.staged = Staged{Release: mustTag(t, "v0.1.1"), CLI: filepath.Join(stage, in.programs()[0]), Daemon: filepath.Join(stage, in.programs()[1])}
	return x
}

func (x *installed) write(path, body string) {
	x.t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // a stand-in program
		x.t.Fatal(err)
	}
}

// onDisk asserts which version each installed program is.
func (x *installed) onDisk(want string) {
	x.t.Helper()
	for _, name := range x.in.programs() {
		got, err := os.ReadFile(filepath.Join(x.in.BinDir, name))
		if err != nil || string(got) != want+" "+name {
			x.t.Errorf("%s is %q (%v), want version %s", name, got, err, want)
		}
	}
	left, _ := os.ReadDir(x.in.BinDir)
	if len(left) != 2 {
		x.t.Errorf("the bin directory holds %d files, want the two programs", len(left))
	}
}

func (x *installed) start(running string, crashed bool, want Outcome) {
	x.t.Helper()
	got, err := x.in.AtStart(running, crashed)
	if err != nil || got != want {
		x.t.Fatalf("AtStart(%s, crashed=%v) = %v, %v; want %v", running, crashed, got, err, want)
	}
}

func (x *installed) noPrevious() {
	x.t.Helper()
	if _, err := os.Stat(x.in.previousDir()); !os.IsNotExist(err) {
		x.t.Errorf("the previous pair is still kept: %v", err)
	}
}

func TestInstallSwapsThePairAndKeepsThePrevious(t *testing.T) {
	x := newInstalled(t)
	if err := x.in.Update(func(r *Record) { r.RolledBack, r.Available, r.LastError = "0.0.9", "0.1.1", "old news" }); err != nil {
		t.Fatal(err)
	}
	if err := x.in.Install(x.staged, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	x.onDisk("0.1.1")
	for _, name := range x.in.programs() {
		got, err := os.ReadFile(filepath.Join(x.in.previousDir(), name))
		if err != nil || string(got) != "0.1.0 "+name {
			t.Errorf("the kept %s is %q (%v)", name, got, err)
		}
		info, err := os.Stat(filepath.Join(x.in.BinDir, name))
		if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o755) {
			t.Errorf("%s has mode %v", name, info.Mode())
		}
	}
	r := x.in.Load()
	if r.Pending == nil || *r.Pending != (Pending{From: "0.1.0", To: "0.1.1", Phase: phaseApplied}) {
		t.Errorf("pending %+v", r.Pending)
	}
	if r.RolledBack != "" || r.Available != "" || r.LastError != "" {
		t.Errorf("an installation left old news in the record: %+v", r)
	}
	if _, err := os.Stat(x.staged.Daemon); !os.IsNotExist(err) {
		t.Errorf("the staged release was not removed")
	}
}

func TestAVersionThatDoesNotStayUpIsRolledBack(t *testing.T) {
	x := newInstalled(t)
	if err := x.in.Install(x.staged, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	// Two starts that are never confirmed and never end cleanly: killed, or exiting on an error.
	x.start("0.1.1", false, Counted)
	x.start("0.1.1", false, Counted)
	x.onDisk("0.1.1")
	x.start("0.1.1", false, RolledBack)

	x.onDisk("0.1.0")
	x.noPrevious()
	r := x.in.Load()
	if r.Pending != nil || r.RolledBack != "0.1.1" || !slices.Contains(r.Bad, "0.1.1") {
		t.Errorf("after a rollback: %+v", r)
	}
	// The previous version starts, and finds nothing to do.
	x.start("0.1.0", false, Nothing)
	// And the version is not chosen again.
	if got, ok := Choose(Policy{Current: version(t, "0.1.0"), Bad: r.Bad}, releasesOf(t, "v0.1.1")); ok {
		t.Errorf("a version rolled back from was chosen again: %s", got.Tag)
	}
}

func TestACrashRollsBackAtTheNextStart(t *testing.T) {
	x := newInstalled(t)
	if err := x.in.Install(x.staged, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	// A traceback found by the new version's first start was the old version's: it does not count.
	x.start("0.1.1", true, Counted)
	x.onDisk("0.1.1")
	// One found by its second start is its own.
	x.start("0.1.1", true, RolledBack)
	x.onDisk("0.1.0")
}

func TestStartsThatEndCleanlyAreNotCounted(t *testing.T) {
	x := newInstalled(t)
	if err := x.in.Install(x.staged, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		x.start("0.1.1", false, Counted)
		x.in.CleanExit()
	}
	x.onDisk("0.1.1")
	if p := x.in.Load().Pending; p == nil || p.Starts != 0 {
		t.Errorf("pending %+v", p)
	}
}

func TestConfirmEndsTheTrial(t *testing.T) {
	x := newInstalled(t)
	if err := x.in.Install(x.staged, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	x.start("0.1.1", false, Counted)
	if err := x.in.Confirm(); err != nil {
		t.Fatal(err)
	}
	x.noPrevious()
	for i := 0; i < 4; i++ {
		x.start("0.1.1", true, Nothing)
	}
	x.onDisk("0.1.1")
	if r := x.in.Load(); r.Pending != nil || len(r.Bad) != 0 {
		t.Errorf("after confirming: %+v", r)
	}
}

// A machine switched off between the two renames: the command is the new one and the daemon the old.
func TestAnInterruptedInstallationIsUndoneAtTheNextStart(t *testing.T) {
	x := newInstalled(t)
	if err := x.in.Install(x.staged, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	daemon := x.in.programs()[1]
	x.write(filepath.Join(x.in.BinDir, daemon), "0.1.0 "+daemon)
	if err := x.in.Update(func(r *Record) { r.Pending.Phase = phaseSwapping }); err != nil {
		t.Fatal(err)
	}

	x.start("0.1.0", false, Recovered)
	x.onDisk("0.1.0")
	x.noPrevious()
	if r := x.in.Load(); r.Pending != nil || len(r.Bad) != 0 {
		t.Errorf("an interrupted installation is nobody's bad version: %+v", r)
	}
}

// Switched off after both renames and before the record said so: the daemon that starts is the new one.
func TestAnInstallationWhoseRecordWasLeftBehindIsOnTrial(t *testing.T) {
	x := newInstalled(t)
	if err := x.in.Install(x.staged, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	if err := x.in.Update(func(r *Record) { r.Pending.Phase = phaseSwapping }); err != nil {
		t.Fatal(err)
	}
	x.start("0.1.1", false, Counted)
	x.onDisk("0.1.1")
	if p := x.in.Load().Pending; p == nil || p.Phase != phaseApplied || p.Starts != 1 {
		t.Errorf("pending %+v", p)
	}
}

func TestARecordAboutAnotherInstallationIsDropped(t *testing.T) {
	x := newInstalled(t)
	if err := x.in.Install(x.staged, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	// Somebody ran the installer by hand: 0.1.2 is what starts.
	for i := 0; i < 5; i++ {
		x.start("0.1.2", true, Nothing)
	}
	x.onDisk("0.1.1") // untouched: nothing was restored over what the installer put there
	x.noPrevious()
}

func TestAFailedInstallationLeavesThePairThatWasRunning(t *testing.T) {
	// The staged daemon has gone: the command is replaced, the daemon cannot be.
	x := newInstalled(t)
	if err := x.in.Update(func(r *Record) { r.RolledBack, r.Available = "0.0.9", "0.1.1" }); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(x.staged.Daemon); err != nil {
		t.Fatal(err)
	}
	if err := x.in.Install(x.staged, "0.1.0"); err == nil {
		t.Fatal("an installation with no daemon to install succeeded")
	}
	x.onDisk("0.1.0")
	r := x.in.Load()
	if r.Pending != nil {
		t.Errorf("a failed installation is still pending: %+v", r.Pending)
	}
	// What was true before it is still said: the last version undone, and the release that is out.
	if r.RolledBack != "0.0.9" || r.Available != "0.1.1" {
		t.Errorf("a failed installation took what the record said with it: %+v", r)
	}
	x.start("0.1.0", false, Nothing)

	// The installed command is not there to keep: nothing is touched.
	x = newInstalled(t)
	if err := os.Remove(filepath.Join(x.in.BinDir, x.in.programs()[0])); err != nil {
		t.Fatal(err)
	}
	if err := x.in.Install(x.staged, "0.1.0"); err == nil {
		t.Fatal("an installation over a missing program succeeded")
	}
	if got, _ := os.ReadFile(filepath.Join(x.in.BinDir, x.in.programs()[1])); string(got) != "0.1.0 "+x.in.programs()[1] {
		t.Errorf("the daemon was replaced by an installation that failed: %q", got)
	}
	if p := x.in.Load().Pending; p != nil {
		t.Errorf("pending %+v", p)
	}
}

func TestTheRecordIsForgivingAndBounded(t *testing.T) {
	x := newInstalled(t)
	if err := os.MkdirAll(x.in.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	x.write(x.in.recordPath(), `{"pending":{"phase":"something else","to":"9.9.9"}}`)
	if r := x.in.Load(); r.Pending != nil {
		t.Errorf("a phase that is not one: pending %+v", r.Pending)
	}
	x.start("0.1.0", true, Nothing)
	// A record that is there and cannot be read is not an empty one. Nothing is written over it: read as
	// empty, the next change saved a record that had forgotten "off" and every version rolled back from
	// (M24 /code-review).
	for _, body := range []string{"", "not json", `[1,2]`, `{"mode":"off","bad":["0.1.1"]`} {
		x.write(x.in.recordPath(), body)
		x.start("0.1.0", true, Nothing)
		if err := x.in.Update(func(r *Record) { r.LastError = "something" }); !errors.Is(err, errUnreadable) {
			t.Errorf("record %q: a change was made over it: %v", body, err)
		}
		x.in.CleanExit()
		if err := x.in.Confirm(); err != nil {
			t.Errorf("record %q: confirm: %v", body, err)
		}
		if err := x.in.Install(x.staged, "0.1.0"); !errors.Is(err, errUnreadable) {
			t.Errorf("record %q: installed over it: %v", body, err)
		}
		if got, _ := os.ReadFile(x.in.recordPath()); string(got) != body {
			t.Errorf("record %q was written over with %q", body, got)
		}
	}
	if err := os.Remove(x.in.recordPath()); err != nil {
		t.Fatal(err)
	}
	if err := x.in.Update(func(r *Record) {
		for i := 0; i < maxBad+10; i++ {
			r.Bad = append(r.Bad, fmt.Sprintf("0.0.%d", i))
		}
	}); err != nil {
		t.Fatal(err)
	}
	if bad := x.in.Load().Bad; len(bad) != maxBad || bad[len(bad)-1] != fmt.Sprintf("0.0.%d", maxBad+9) {
		t.Errorf("the bad list holds %d, ending %v", len(bad), bad[len(bad)-1:])
	}
	if info, err := os.Stat(x.in.recordPath()); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Errorf("the record's mode: %v", info.Mode())
	}
}

// An installation was interrupted, and before the daemon next started somebody ran the installer by hand:
// what starts is neither version of the interrupted one. The kept pair is not theirs to be restored over.
func TestAnInterruptedInstallationIsNotRestoredOverAThirdVersion(t *testing.T) {
	x := newInstalled(t)
	if err := x.in.Install(x.staged, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	if err := x.in.Update(func(r *Record) { r.Pending.Phase = phaseSwapping }); err != nil {
		t.Fatal(err)
	}
	for _, name := range x.in.programs() {
		x.write(filepath.Join(x.in.BinDir, name), "0.1.2 "+name)
	}
	x.start("0.1.2", false, Nothing)
	x.onDisk("0.1.2")
	x.noPrevious()
	if p := x.in.Load().Pending; p != nil {
		t.Errorf("pending %+v", p)
	}
}

// The programs Windows set aside because they were running are removed once nothing runs them.
func TestProgramsSetAsideAreSweptWhenTheyCanBe(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "norite.exe")
	for _, name := range []string{"norite.exe", "norite.exe.old-a1", "norite.exe.old-b2", "norite.exe.older", "norite-daemon.exe.old-c3"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sweepOld(dst)
	left, _ := os.ReadDir(dir)
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"norite-daemon.exe.old-c3", "norite.exe", "norite.exe.older"}) {
		t.Errorf("left %v", names)
	}
}

// The programs set aside on Windows are found by listing the directory. A pattern made from the path read
// a "[" in a folder's name as the start of a set, matched nothing, and left every one of them for good.
func TestSweepingFindsWhatWasSetAsideInAnyDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin [work]")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "norite.exe")
	for _, name := range []string{"norite.exe", "norite.exe.old-abc", "norite.exe.old-def", "norite-daemon.exe.old-abc", "norite.exe.older"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sweepOld(dst)
	var left []string
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		left = append(left, entry.Name())
	}
	want := []string{"norite-daemon.exe.old-abc", "norite.exe", "norite.exe.older"}
	if strings.Join(left, " ") != strings.Join(want, " ") {
		t.Errorf("left %v, want %v", left, want)
	}
}

// An undoing whose record could not be written is still an undoing: the previous pair is on disk, the
// daemon makes way for it, and the start that follows lists the version as bad. It used to be reported
// as not done, and the version, never listed, was installed again at the next check.
func TestAnUndoingThatCouldNotBeRecordedIsFinishedAtTheNextStart(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs a directory this account cannot write in")
	}
	x := newInstalled(t)
	if err := x.in.Install(x.staged, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	x.start("0.1.1", false, Counted)
	// The record cannot be replaced: its directory takes no new file.
	if err := os.Chmod(x.in.Dir, 0o500); err != nil {
		t.Fatal(err)
	}
	outcome, err := x.in.AtStart("0.1.1", true)
	if err := os.Chmod(x.in.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if outcome != RolledBack || err == nil {
		t.Fatalf("outcome %v, error %v: want the undoing done and its record failed", outcome, err)
	}
	if got, _ := os.ReadFile(filepath.Join(x.in.BinDir, daemonName)); string(got) != "0.1.0 "+daemonName {
		t.Fatalf("on disk: %q", got)
	}
	// The previous version starts, and finds the record as it was.
	x.start("0.1.0", false, Nothing)
	r := x.in.Load()
	if r.Pending != nil || len(r.Bad) != 1 || r.Bad[0] != "0.1.1" || r.RolledBack != "0.1.1" {
		t.Errorf("the record after: %+v", r)
	}
}

// The record is asked about before it is opened: a pipe at its path would hold the daemon's start for
// ever, with its lock taken.
func TestARecordThatIsNotAFileIsNotOpened(t *testing.T) {
	x := newInstalled(t)
	if err := os.MkdirAll(x.in.recordPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if r := x.in.Load(); !r.unreadable {
		t.Errorf("a directory was read as a record: %+v", r)
	}
	if err := x.in.Update(func(r *Record) { r.Mode = "off" }); !errors.Is(err, errUnreadable) {
		t.Errorf("written over: %v", err)
	}
}

// An installation begun while a version is on trial, and failing as it keeps the current pair, leaves the
// pair kept for that trial as it was: it is what a rollback restores.
func TestAFailedSecondInstallationKeepsTheFirstOnesWayBack(t *testing.T) {
	x := newInstalled(t)
	if err := x.in.Install(x.staged, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	x.start("0.1.1", false, Counted)
	// The second installation cannot keep the current pair: the daemon on disk is gone.
	daemon := filepath.Join(x.in.BinDir, daemonName)
	now, err := os.ReadFile(daemon)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(daemon); err != nil {
		t.Fatal(err)
	}
	if err := x.in.Install(x.staged, "0.1.1"); err == nil {
		t.Fatal("installed with no daemon to keep")
	}
	x.write(daemon, string(now))
	kept, err := os.ReadFile(filepath.Join(x.in.previousDir(), daemonName))
	if err != nil || string(kept) != "0.1.0 "+daemonName {
		t.Errorf("the pair kept for the trial is now %q (%v)", kept, err)
	}
	if _, err := os.Stat(x.in.previousDir() + ".new"); err == nil {
		t.Errorf("the half-kept pair was left behind")
	}
	// And the trial is still undone to it.
	x.start("0.1.1", true, RolledBack)
	if got, _ := os.ReadFile(daemon); string(got) != "0.1.0 "+daemonName {
		t.Errorf("rolled back to %q", got)
	}
}
