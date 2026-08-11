package daemonproc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// startDaemon runs the daemon in a goroutine and blocks until it reports itself ready.
//
// Returns a stop function that cancels it and waits for Run to return, so a test can assert on what a
// completed run produced rather than on a snapshot taken while it was still going.
func startDaemon(t *testing.T, opts Options) (stop func() error, logs *syncBuffer) {
	t.Helper()

	logs = &syncBuffer{}
	opts.Stderr = logs
	opts.LogLevel = zerolog.DebugLevel

	ready := make(chan struct{})
	var once sync.Once
	userReady := opts.Ready
	opts.Ready = func() {
		if userReady != nil {
			userReady()
		}
		once.Do(func() { close(ready) })
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, opts) }()

	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("daemon exited before becoming ready: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("daemon did not become ready within 10s")
	}

	var stopOnce sync.Once
	var stopErr error
	stop = func() error {
		stopOnce.Do(func() {
			cancel()
			select {
			case stopErr = <-done:
			case <-time.After(10 * time.Second):
				stopErr = errors.New("daemon did not stop within 10s of cancellation")
			}
		})
		return stopErr
	}
	t.Cleanup(func() { _ = stop() })

	return stop, logs
}

func TestRunStartsAndStopsCleanly(t *testing.T) {
	dir := t.TempDir()

	stop, logs := startDaemon(t, Options{StateDir: dir, Version: "test-1.2.3"})

	if err := stop(); err != nil {
		// The single most important property of the whole milestone: a signal-initiated stop is a success.
		// A non-nil error here becomes a non-zero exit, which systemd reads as a crash and answers with a
		// restart — an ordinary `systemctl --user stop` would loop.
		t.Fatalf("a cancellation-initiated stop must return nil, got %v", err)
	}

	out := logs.String()
	for _, want := range []string{"daemon starting", "daemon ready", "daemon stopping", "daemon stopped"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in the log:\n%s", want, out)
		}
	}

	if got := indexOf(out, "daemon ready"); got > indexOf(out, "daemon stopping") {
		t.Error("the daemon logged that it was stopping before it logged that it was ready")
	}
}

func TestRunLogsToBothTheFileAndStderr(t *testing.T) {
	dir := t.TempDir()

	stop, logs := startDaemon(t, Options{StateDir: dir, Version: "test-1.2.3"})
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// The file is the durable copy the later `norite logs tail` reads; stderr is what journald and launchd
	// capture. Both have to be populated, or one of those two ways of looking at the daemon shows nothing.
	onDisk, err := os.ReadFile(filepath.Join(dir, "daemon.log"))
	if err != nil {
		t.Fatalf("reading the daemon log: %v", err)
	}
	if !strings.Contains(string(onDisk), "daemon ready") {
		t.Errorf("the log file is missing the startup lines:\n%s", onDisk)
	}
	if !strings.Contains(logs.String(), "daemon ready") {
		t.Errorf("stderr is missing the startup lines:\n%s", logs.String())
	}
}

func TestStartupLogIsStructuredAndNamesItsPaths(t *testing.T) {
	dir := t.TempDir()

	stop, logs := startDaemon(t, Options{StateDir: dir, Version: "test-1.2.3"})
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	var start map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON: %q (%v)", line, err)
		}
		if entry["message"] == "daemon starting" {
			start = entry
		}
	}
	if start == nil {
		t.Fatal(`no "daemon starting" line found`)
	}

	// An operator debugging a daemon that will not start needs to know which files it was looking at
	// without having to re-derive the per-platform path rules by hand.
	if got := start["state_dir"]; got != dir {
		t.Errorf("state_dir = %v, want %v", got, dir)
	}
	if got := start["log_file"]; got != filepath.Join(dir, "daemon.log") {
		t.Errorf("log_file = %v, want %v", got, filepath.Join(dir, "daemon.log"))
	}
	if got := start["version"]; got != "test-1.2.3" {
		t.Errorf("version = %v, want test-1.2.3", got)
	}
	if _, ok := start["pid"]; !ok {
		t.Error("the startup line does not report a pid")
	}
	if got := start["component"]; got != "daemon" {
		t.Errorf("component = %v, want daemon", got)
	}
}

func TestASecondDaemonRefusesToStart(t *testing.T) {
	dir := t.TempDir()

	stop, _ := startDaemon(t, Options{StateDir: dir, Version: "first"})

	// One daemon per OS user is the invariant the token store and, later, the E2E keystore depend on
	// (ADR 0010). The second must decline, and must do so with the error the caller can recognize rather
	// than a generic startup failure — daemond maps exactly this to its own exit code.
	err := Run(t.Context(), Options{StateDir: dir, Version: "second"})
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("the second daemon returned %v, want ErrAlreadyRunning", err)
	}

	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestTheLockIsReleasedWhenTheDaemonStops(t *testing.T) {
	dir := t.TempDir()

	stop, _ := startDaemon(t, Options{StateDir: dir, Version: "first"})
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// A restart is the ordinary case — `systemctl --user restart`, a machine waking up, a crash loop
	// recovering. If the lock outlived the process that took it, the daemon could be started exactly once
	// per boot, which is worse than having no lock at all.
	stopAgain, _ := startDaemon(t, Options{StateDir: dir, Version: "second"})
	if err := stopAgain(); err != nil {
		t.Fatalf("the second run did not stop cleanly: %v", err)
	}
}

func TestRunCreatesTheStateDirectoryItWasGiven(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state")

	// StateDir() creates the default location, but an explicitly-passed directory has to work too: this is
	// the path a test takes, and it is one os.MkdirAll away from being the path a future --state-dir flag
	// takes.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("preparing the state directory: %v", err)
	}

	stop, _ := startDaemon(t, Options{StateDir: dir, Version: "test"})
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "daemon.lock")); err != nil {
		t.Errorf("no lock file in the state directory: %v", err)
	}
}

func TestRaiseFileLimitDoesNotFail(t *testing.T) {
	// Asserting a specific number would be asserting the machine's hard limit, which CI containers set
	// wherever they like. What matters is that the call is safe to make unprivileged and never returns an
	// error that would show up as a warning on every single start.
	limit, err := raiseFileLimit()
	if err != nil {
		t.Fatalf("raiseFileLimit: %v", err)
	}
	t.Logf("resulting soft RLIMIT_NOFILE: %d", limit)
}

func indexOf(haystack, needle string) int { return strings.Index(haystack, needle) }

// syncBuffer is a bytes.Buffer safe to write from the daemon goroutine and read from the test's.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The launchd backend passes -log-file so the daemon's rotating log lands in ~/Library/Logs, where macOS
// users and Console.app look. The lock deliberately stays in the state directory regardless: it is the
// per-user rendezvous point, and moving it with the logs would let two daemons take different locks.
func TestLogFileCanBeRedirectedAwayFromTheStateDir(t *testing.T) {
	stateDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "elsewhere", "norite-daemon.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatalf("preparing the log directory: %v", err)
	}

	stop, _ := startDaemon(t, Options{StateDir: stateDir, LogFile: logPath, Version: "test"})
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading the redirected log: %v", err)
	}
	if !strings.Contains(string(body), "daemon ready") {
		t.Errorf("the redirected log is missing the startup lines:\n%s", body)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "daemon.log")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a log was still written to the state directory (%v)", err)
	}
	// The lock must not have followed the log.
	if _, err := os.Stat(filepath.Join(stateDir, "daemon.lock")); err != nil {
		t.Errorf("the lock left the state directory: %v", err)
	}
}

// launchd writes stderr to a file it never rotates, so under it the daemon must not mirror there — that
// would duplicate the rotated log into an unbounded one. A nil Stderr is how the caller asks for that.
func TestStderrMirroringCanBeTurnedOff(t *testing.T) {
	dir := t.TempDir()

	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			StateDir: dir,
			Version:  "test",
			LogLevel: zerolog.InfoLevel,
			Stderr:   nil, // the point of the test
			Ready:    func() { close(ready) },
		})
	}()

	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("daemon exited before becoming ready: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("daemon did not become ready within 10s")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("stop: %v", err)
	}

	// The file copy must still be complete — turning the mirror off must not turn logging off.
	body, err := os.ReadFile(filepath.Join(dir, "daemon.log"))
	if err != nil {
		t.Fatalf("reading the daemon log: %v", err)
	}
	for _, want := range []string{"daemon starting", "daemon ready", "daemon stopped"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the log file is missing %q:\n%s", want, body)
		}
	}
}
