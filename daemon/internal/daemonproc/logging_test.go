// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonproc

import (
	"bytes"
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

// A crash is a process dying, so it is tested in one: this test runs itself again with the variable set,
// and that copy directs crashes to the file and panics on a goroutine nothing recovers.
func TestAFatalCrashIsWrittenBesideTheLog(t *testing.T) {
	if path := os.Getenv(crashHelperEnv); path != "" {
		if _, err := captureCrashes(path); err != nil {
			t.Fatalf("captureCrashes: %v", err)
		}
		go panic("the crash this test is about")
		select {}
	}

	logPath := filepath.Join(t.TempDir(), "daemon.log")
	crashPath := logfile.CrashPath(logPath)

	for run := 1; run <= 2; run++ {
		child := exec.Command(os.Args[0], "-test.run=^TestAFatalCrashIsWrittenBesideTheLog$") //nolint:gosec // the test binary itself
		child.Env = append(os.Environ(), crashHelperEnv+"="+crashPath)
		if err := child.Run(); err == nil {
			t.Fatalf("run %d: the child did not crash", run)
		}

		body, err := os.ReadFile(crashPath)
		if err != nil {
			t.Fatalf("run %d: no crash file: %v", run, err)
		}
		// Appended, so a daemon crashing at every start leaves each crash and not only the last.
		if got := strings.Count(string(body), "panic: the crash this test is about"); got != run {
			t.Fatalf("run %d: the crash file holds %d crashes, want %d:\n%s", run, got, run, body)
		}
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
}

func TestACrashFilePastItsBoundIsEmptiedAtStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.crash.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), crashFileMax+1), crashFileMode); err != nil {
		t.Fatalf("seeding the crash file: %v", err)
	}
	release, err := captureCrashes(path)
	if err != nil {
		t.Fatalf("captureCrashes: %v", err)
	}
	release()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the crash file: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("a crash file past its bound is still %d bytes", info.Size())
	}
}

// A crash file at its bound or under it is kept: emptying it at every start would lose the crash that
// made somebody restart.
func TestACrashFileWithinItsBoundIsKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.crash.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), crashFileMax), crashFileMode); err != nil {
		t.Fatalf("seeding the crash file: %v", err)
	}
	release, err := captureCrashes(path)
	if err != nil {
		t.Fatalf("captureCrashes: %v", err)
	}
	release()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the crash file: %v", err)
	}
	if info.Size() != crashFileMax {
		t.Errorf("a crash file within its bound was changed to %d bytes", info.Size())
	}
}

// Whatever is at the path is appended to as this user, so a link there is refused: it would send a
// traceback to a file somebody else chose.
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

	if release, err := captureCrashes(path); err == nil {
		release()
		t.Fatal("a crash file that is a link was accepted")
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
