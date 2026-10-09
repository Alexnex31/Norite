// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonctl

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Alexnex31/Norite/daemon/ipc"
)

// stopDaemon answers one stop request as a test dictates.
type stopDaemon struct {
	res   ipc.Result
	err   error
	asked []string
}

func (d *stopDaemon) Do(_ context.Context, method, path string, body any) (ipc.Result, error) {
	d.asked = append(d.asked, method+" "+path)
	if body != nil {
		panic("a stop request carries no body")
	}
	return d.res, d.err
}

func stoppingBody(t *testing.T, pid int) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(ipc.Stopping{PID: pid})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestAskingTheDaemonToStop(t *testing.T) {
	const noWait = -1
	cases := []struct {
		name string
		res  ipc.Result
		err  error
		// gone is what waiting finds. waitedFor is what must have been waited for: a process id, 0 for a
		// daemon that named none, or noWait.
		gone      bool
		waitedFor int
		want      stopOutcome
	}{
		{name: "agreed and exited", res: ipc.Result{Status: 200, Body: stoppingBody(t, 4242)},
			gone: true, waitedFor: 4242, want: stopGone},
		{name: "agreed and still there", res: ipc.Result{Status: 200, Body: stoppingBody(t, 4242)},
			waitedFor: 4242, want: stopPending},
		// The connection closing under the request is a daemon that began to stop before its answer left.
		// Which process is not known, so its socket is what is watched, and it is watched: a pending that
		// had waited for nothing was reported as twenty seconds of waiting.
		{name: "closed as a stopping daemon does, and gone",
			err:  &ipc.CloseError{Code: ipc.CloseGoingAway, Reason: "the daemon is stopping"},
			gone: true, waitedFor: 0, want: stopGone},
		{name: "closed as a stopping daemon does, and still answering",
			err:       &ipc.CloseError{Code: ipc.CloseGoingAway, Reason: "the daemon is stopping"},
			waitedFor: 0, want: stopPending},
		{name: "closed without a word, and gone", err: ipc.ErrClosed, gone: true, waitedFor: 0, want: stopGone},
		// A daemon from before the request relays the path: signed out it declines, signed in it reports
		// whatever its instance made of it. Neither is an agreement, and the service manager is still owed.
		{name: "an older daemon, signed out",
			err: &ipc.RelayError{Code: ipc.RelayNotSignedIn, Message: "nobody is signed in"}, waitedFor: noWait, want: stopNotAsked},
		{name: "an older daemon, signed in", res: ipc.Result{Status: 404, Body: json.RawMessage(`{}`)},
			waitedFor: noWait, want: stopNotAsked},
		{name: "refused", err: &ipc.RelayError{Code: ipc.RelayBadRequest, Message: "no"}, waitedFor: noWait, want: stopNotAsked},
		{name: "closed for another reason",
			err: &ipc.CloseError{Code: ipc.CloseTooSlow, Reason: "slow"}, waitedFor: noWait, want: stopNotAsked},
		// Agreed, with no process that can be waited on: 0 is the caller's whole process group to a
		// signal, 1 is init, and a negative id is a group. The id is never used; the socket is watched.
		{name: "agreed, pid 0", res: ipc.Result{Status: 200, Body: stoppingBody(t, 0)}, gone: true, waitedFor: 0, want: stopGone},
		{name: "agreed, pid 1", res: ipc.Result{Status: 200, Body: stoppingBody(t, 1)}, waitedFor: 0, want: stopPending},
		{name: "agreed, a negative pid", res: ipc.Result{Status: 200, Body: stoppingBody(t, -7)}, gone: true, waitedFor: 0, want: stopGone},
		{name: "agreed, a body that is not one", res: ipc.Result{Status: 200, Body: json.RawMessage(`[]`)},
			waitedFor: 0, want: stopPending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &stopDaemon{res: tc.res, err: tc.err}
			waited := []int{}
			got := askToStop(t.Context(), d, func(pid int) bool {
				waited = append(waited, pid)
				return tc.gone
			})
			if got != tc.want {
				t.Errorf("outcome %d, want %d", got, tc.want)
			}
			if len(d.asked) != 1 || d.asked[0] != "POST "+ipc.PathStop {
				t.Errorf("asked %v, want one POST %s", d.asked, ipc.PathStop)
			}
			switch {
			case tc.waitedFor == noWait && len(waited) != 0:
				t.Errorf("waited for %v, want no wait", waited)
			case tc.waitedFor != noWait && (len(waited) != 1 || waited[0] != tc.waitedFor):
				t.Errorf("waited for %v, want one wait for %d", waited, tc.waitedFor)
			}
		})
	}
}

// With no daemon listening the socket is silent at once. TestMain points the state directory somewhere
// empty, so this is the real dial.
func TestWaitingForSilenceReturnsWhenNothingListens(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	start := time.Now()
	if !waitForSilence(t.Context(), 10*time.Second) {
		t.Fatal("a socket nobody listens on was not reported silent")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("it took %s to notice", took)
	}
}

// With no daemon at the socket there is nobody to ask, and the service manager's stop is what is left.
// TestMain points the state directory somewhere empty, so this is the real dial finding nothing.
func TestTheSocketStopFindsNoDaemonInAnEmptyStateDirectory(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	if got := realSocketStop(t.Context(), "test"); got != stopNotAsked {
		t.Errorf("outcome %d with no daemon running, want stopNotAsked", got)
	}
}

// sleeper starts a process that outlives the test unless killed, and returns it.
func sleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	// The test binary itself, running a test that waits: no assumption about what is on PATH.
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperThatWaits$") //nolint:gosec // the test binary
	cmd.Env = append(os.Environ(), "NORITE_TEST_WAIT=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting a process to wait on: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

func TestHelperThatWaits(t *testing.T) {
	if os.Getenv("NORITE_TEST_WAIT") == "" {
		t.Skip("a helper for the tests that wait on a process")
	}
	time.Sleep(time.Minute)
}

func TestWaitingForAProcessThatExits(t *testing.T) {
	cmd := sleeper(t)
	pid := cmd.Process.Pid

	if waitForExit(t.Context(), pid, 150*time.Millisecond) {
		t.Fatal("a running process was reported gone")
	}

	done := make(chan bool, 1)
	go func() { done <- waitForExit(context.Background(), pid, 20*time.Second) }()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("ending the process: %v", err)
	}
	// Reaped, as the daemon's own parent reaps it: until then the id still names a process.
	_, _ = cmd.Process.Wait()

	select {
	case gone := <-done:
		if !gone {
			t.Error("an exited process was not reported gone")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("waiting for an exited process did not return")
	}
}

func TestWaitingStopsWhenItsCallerDoes(t *testing.T) {
	cmd := sleeper(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	if waitForExit(ctx, cmd.Process.Pid, time.Hour) {
		t.Error("a running process was reported gone")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("a canceled wait took %s", took)
	}
}

func TestStopAsksTheSocketAndThenTheServiceManager(t *testing.T) {
	for _, outcome := range []stopOutcome{stopNotAsked, stopGone, stopPending} {
		mgr := &stubManager{state: State{Installed: true, Running: true}}
		asked := answerStop(t, outcome)
		out, err := runCommand(t, mgr, "daemon", "stop")
		if err != nil {
			t.Fatalf("outcome %d: stop: %v", outcome, err)
		}
		// Whatever the socket said: the service manager's record has to say stopped too, and stopping a
		// stopped daemon succeeds.
		if *asked != 1 || mgr.stops != 1 {
			t.Errorf("outcome %d: asked the socket %d times and the service manager %d, want 1 and 1",
				outcome, *asked, mgr.stops)
		}
		if !strings.Contains(out, "Stopped "+ServiceName) {
			t.Errorf("outcome %d: output %q", outcome, out)
		}
	}
}

// A daemon started by hand is one the service manager has never heard of. Before M23 `norite daemon stop`
// answered that it was not installed, and left it running.
func TestStopStopsADaemonNoServiceManages(t *testing.T) {
	mgr := &stubManager{stopErr: ErrNotInstalled}
	answerStop(t, stopGone)
	out, err := runCommand(t, mgr, "daemon", "stop")
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !strings.Contains(out, "Stopped "+ServiceName) || !strings.Contains(out, "not installed as a service") {
		t.Errorf("output %q does not say it stopped a daemon that is not a service", out)
	}
}

func TestStopWithNoServiceAndNoDaemonSaysNotInstalled(t *testing.T) {
	mgr := &stubManager{stopErr: ErrNotInstalled}
	answerStop(t, stopNotAsked)
	out, err := runCommand(t, mgr, "daemon", "stop")
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("stop: %v, want ErrNotInstalled", err)
	}
	if strings.Contains(out, "Stopped") {
		t.Errorf("output %q claims a stop", out)
	}
}

// Agreed, not seen to exit, and no service to fall back on: saying "stopped" would be a guess.
func TestStopDoesNotClaimAStopItCouldNotSee(t *testing.T) {
	mgr := &stubManager{stopErr: ErrNotInstalled}
	answerStop(t, stopPending)
	out, err := runCommand(t, mgr, "daemon", "stop")
	if err == nil || errors.Is(err, ErrNotInstalled) {
		t.Fatalf("stop: %v, want an error of its own", err)
	}
	if !strings.Contains(err.Error(), "has not exited") || strings.Contains(out, "Stopped") {
		t.Errorf("error %q, output %q", err, out)
	}
}

// A failure of the service manager's own is not hidden by the socket having worked.
func TestStopReportsAServiceManagerFailure(t *testing.T) {
	failure := errors.New("systemctl --user stop norite-daemon failed")
	mgr := &stubManager{stopErr: failure}
	answerStop(t, stopGone)
	if _, err := runCommand(t, mgr, "daemon", "stop"); !errors.Is(err, failure) {
		t.Fatalf("stop: %v, want the service manager's failure", err)
	}
}

func TestRestartAsksTheSocketBeforeTheServiceManager(t *testing.T) {
	mgr := &stubManager{state: State{Installed: true, Running: true}}
	asked := answerStop(t, stopGone)
	if _, err := runCommand(t, mgr, "daemon", "restart"); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if *asked != 1 || mgr.stops != 1 || mgr.starts != 1 {
		t.Errorf("socket %d, stops %d, starts %d; want one of each", *asked, mgr.stops, mgr.starts)
	}
}

// With no service there is nothing to start the daemon again, so a restart must not stop one first.
func TestRestartStopsNothingWhenNoServiceWouldStartItAgain(t *testing.T) {
	mgr := &stubManager{state: State{}}
	asked := answerStop(t, stopGone)
	_, err := runCommand(t, mgr, "daemon", "restart")
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("restart: %v, want ErrNotInstalled", err)
	}
	if *asked != 0 || mgr.stops != 0 || mgr.starts != 0 {
		t.Errorf("socket %d, stops %d, starts %d; want none", *asked, mgr.stops, mgr.starts)
	}
	if !strings.Contains(err.Error(), "norite daemon stop") {
		t.Errorf("the refusal does not say how a hand-started daemon is stopped: %v", err)
	}
}

func TestUninstallAsksTheDaemonToStopFirst(t *testing.T) {
	mgr := &stubManager{state: State{Installed: true, Running: true}}
	asked := answerStop(t, stopGone)
	if _, err := runCommand(t, mgr, "daemon", "uninstall"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if *asked != 1 || mgr.uninstalls != 1 {
		t.Errorf("socket %d, uninstalls %d; want one of each", *asked, mgr.uninstalls)
	}
}

func TestProcessExitedIsBuiltForThisPlatform(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skipf("no way to wait on another process is written for %s", runtime.GOOS)
	}
	// This process is running, whatever else is true.
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if processExited(ctx, os.Getpid()) {
		t.Error("this very process was reported gone")
	}
}
