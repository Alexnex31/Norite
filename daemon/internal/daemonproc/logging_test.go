// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonproc

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Alexnex31/Norite/daemon/logfile"
)

// backupsOf lists the rotated copies beside a log: lumberjack names them <name>-<timestamp>.log.
func backupsOf(t *testing.T, logPath string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(logPath))
	if err != nil {
		t.Fatalf("listing the log directory: %v", err)
	}
	prefix := strings.TrimSuffix(filepath.Base(logPath), ".log") + "-"
	var backups []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			backups = append(backups, entry.Name())
		}
	}
	return backups
}

// The log rotates instead of growing: driven through twice as many rotations as it keeps backups, it
// leaves logMaxFiles of them and a live file under the size it rotates at. The writer is the daemon's
// own, at a megabyte rather than ten so the test writes megabytes and not a hundred of them.
func TestTheLogRotatesAndKeepsOnlyItsBackups(t *testing.T) {
	const sizeMB = 1
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	writer := newLogWriterSized(logPath, sizeMB)
	defer func() { _ = writer.Close() }()

	line := append(bytes.Repeat([]byte("x"), 1023), '\n')
	const rotations = logMaxFiles * 2
	for range rotations {
		for range sizeMB * 1024 {
			if _, err := writer.Write(line); err != nil {
				t.Fatalf("writing the log: %v", err)
			}
		}
		// A backup is named to the millisecond, so two rotations inside one would be one file.
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := writer.Write(line); err != nil {
		t.Fatalf("writing the log: %v", err)
	}

	// Old backups are removed by a goroutine of lumberjack's, after the write that rotated returns.
	deadline := time.Now().Add(10 * time.Second)
	var backups []string
	for {
		backups = backupsOf(t, logPath)
		if len(backups) == logMaxFiles || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(backups) != logMaxFiles {
		t.Fatalf("%d rotations left %d backups, want %d: %v", rotations, len(backups), logMaxFiles, backups)
	}

	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("the live log: %v", err)
	}
	if info.Size() > sizeMB<<20 {
		t.Errorf("the live log is %d bytes, past the %d MB it rotates at", info.Size(), sizeMB)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("the log is %#o, want 0600", info.Mode().Perm())
	}
}

// crashHelperEnv names the crash file for the child below, and is what makes it the child.
const crashHelperEnv = "NORITE_TEST_CRASH_FILE"

// crashOnce runs this test binary again as a process that directs crashes to path and panics on a
// goroutine nothing recovers. A crash is a process dying, so it is tested in one.
func crashOnce(t *testing.T, path, saying string) {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^TestAFatalCrashIsWrittenBesideTheLog$") //nolint:gosec // the test binary itself
	child.Env = append(os.Environ(), crashHelperEnv+"="+path, crashHelperEnv+"_SAYING="+saying)
	if err := child.Run(); err == nil {
		t.Fatal("the child did not crash")
	}
}

func TestAFatalCrashIsWrittenBesideTheLog(t *testing.T) {
	if path := os.Getenv(crashHelperEnv); path != "" {
		if _, _, err := captureCrashes(path); err != nil {
			t.Fatalf("captureCrashes: %v", err)
		}
		go panic(os.Getenv(crashHelperEnv + "_SAYING"))
		select {}
	}

	logPath := filepath.Join(t.TempDir(), "daemon.log")
	crashPath := logfile.CrashPath(logPath)

	crashOnce(t, crashPath, "the first crash")
	body, err := os.ReadFile(crashPath)
	if err != nil {
		t.Fatalf("no crash file: %v", err)
	}
	if !strings.Contains(string(body), "panic: the first crash") {
		t.Fatalf("the crash file does not hold the crash:\n%s", body)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(crashPath)
		if err != nil {
			t.Fatalf("the crash file: %v", err)
		}
		if info.Mode().Perm() != crashFileMode {
			t.Errorf("the crash file is %#o, want %#o", info.Mode().Perm(), crashFileMode)
		}
	}

	// The next run finds it, sets it aside, and starts a file of its own: each file is one run's.
	crashOnce(t, crashPath, "the second crash")
	body, err = os.ReadFile(crashPath)
	if err != nil {
		t.Fatalf("no crash file after the second run: %v", err)
	}
	if strings.Contains(string(body), "the first crash") || !strings.Contains(string(body), "panic: the second crash") {
		t.Errorf("the crash file is not the second run's alone:\n%s", body)
	}
	kept, err := os.ReadFile(previousCrashPath(crashPath))
	if err != nil {
		t.Fatalf("the first crash was not kept: %v", err)
	}
	if !strings.Contains(string(kept), "panic: the first crash") {
		t.Errorf("the file set aside is not the first crash:\n%s", kept)
	}

	// And the one after that keeps the latest two, not three.
	crashOnce(t, crashPath, "the third crash")
	kept, err = os.ReadFile(previousCrashPath(crashPath))
	if err != nil {
		t.Fatalf("the second crash was not kept: %v", err)
	}
	if strings.Contains(string(kept), "the first crash") || !strings.Contains(string(kept), "panic: the second crash") {
		t.Errorf("the file set aside is not the second crash alone:\n%s", kept)
	}
}

// A run that ended without crashing leaves an empty file, and an empty file is not a crash.
func TestAnEmptyCrashFileIsNotAPreviousCrash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.crash.log")
	for range 2 {
		previous, release, err := captureCrashes(path)
		if err != nil {
			t.Fatalf("captureCrashes: %v", err)
		}
		release()
		if previous != "" {
			t.Fatalf("a run that did not crash was reported as one: %q", previous)
		}
	}
	if _, err := os.Stat(previousCrashPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("something was set aside with no crash to keep: %v", err)
	}
}

// Whatever is at the path is written to as this user, so a link there is refused: it would send a
// traceback to a file somebody else chose. And the file it points at is not moved or emptied.
func TestACrashFileThatIsALinkIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symbolic link needs a privilege on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, []byte("kept"), 0o600); err != nil {
		t.Fatalf("seeding the target: %v", err)
	}
	path := filepath.Join(dir, "daemon.crash.log")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("linking: %v", err)
	}

	if _, release, err := captureCrashes(path); err == nil {
		release()
		t.Fatal("a crash file that is a link was accepted")
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "kept" {
		t.Errorf("the link's target was touched: %q, %v", body, err)
	}
	if _, err := os.Lstat(previousCrashPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the link was set aside as if it were a crash: %v", err)
	}
}

// The start after a crash says so in the log, which is where `norite logs tail` looks: a crash never
// reaches the logger, so before this the log showed two starts and no reason between them.
func TestTheStartAfterACrashSaysSoInTheLog(t *testing.T) {
	stateDir := t.TempDir()
	logPath := logfile.In(stateDir)
	crashPath := logfile.CrashPath(logPath)
	if err := os.WriteFile(crashPath, []byte("panic: left by the run before\n"), crashFileMode); err != nil {
		t.Fatalf("seeding a crash: %v", err)
	}

	stop, _ := startDaemon(t, Options{StateDir: stateDir, Version: "test", SkipSession: true})
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	if !strings.Contains(string(body), "previous run crashed") ||
		!strings.Contains(string(body), filepath.Base(previousCrashPath(crashPath))) {
		t.Errorf("the log does not say the previous run crashed, or where the traceback is:\n%s", body)
	}

	// A run that ended cleanly is followed by a start that says nothing of the kind.
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	stop, _ = startDaemon(t, Options{StateDir: stateDir, Version: "test", SkipSession: true})
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	body, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	if strings.Contains(string(body), "previous run crashed") {
		t.Errorf("a clean stop was followed by a start reporting a crash:\n%s", body)
	}
}

// The daemon says where a crash would go by putting the file there at start, and says the limit it runs
// under in the line every log begins with.
func TestStartingCreatesTheCrashFileAndReportsTheLimit(t *testing.T) {
	stateDir := t.TempDir()
	stop, _ := startDaemon(t, Options{StateDir: stateDir, Version: "test", SkipSession: true})
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	logPath := logfile.In(stateDir)
	if _, err := os.Stat(logfile.CrashPath(logPath)); err != nil {
		t.Errorf("no crash file beside the log: %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	if !strings.Contains(string(body), `"open_file_limit":`) {
		t.Errorf("the starting line does not report the open-file limit:\n%s", body)
	}
}
