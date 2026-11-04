// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceworker

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/voiceipc"
)

// drain reads the worker's events until it is done, failing the test if that takes past d: a worker the
// daemon should have killed and did not must fail here, not hang the suite.
func drain(t *testing.T, w *Worker, d time.Duration) {
	t.Helper()
	go func() {
		for range w.Events() {
		}
	}()
	if err := within(d, w.Done()); err != nil {
		w.Kill("the test gave up on it")
		<-w.Done()
		t.Fatalf("the worker was still running after %s", d)
	}
}

func fast(t *testing.T) {
	t.Helper()
	h, l, e, sw := helloWithin, leaveWithin, exitWithin, sendWithin
	helloWithin, leaveWithin, exitWithin = 5*time.Second, 2*time.Second, 2*time.Second
	t.Cleanup(func() { helloWithin, leaveWithin, exitWithin, sendWithin = h, l, e, sw })
}

// joinStandin starts mode and walks it to ready.
func joinStandin(t *testing.T, mode string, log *logBuffer) *Worker {
	t.Helper()
	var w *Worker
	var err error
	if log == nil {
		w, err = Start(context.Background(), standinOptions(t, mode, nil))
	} else {
		w, err = Start(context.Background(), standinOptions(t, mode, log))
	}
	require.NoError(t, err)
	t.Cleanup(func() { w.Kill("the test ended"); <-w.Done() })
	require.NoError(t, w.Send(voiceipc.OpJoin, voiceipc.Join{ICEServers: []voiceipc.ICEServer{}}))
	ev := <-w.Events()
	require.NotNil(t, ev.Offer)
	require.NoError(t, w.Send(voiceipc.OpAnswer, voiceipc.Answer{SDP: "v=0\r\n"}))
	require.Equal(t, voiceipc.StateConnecting, (<-w.Events()).State.State)
	require.Equal(t, voiceipc.StateReady, (<-w.Events()).State.State)
	return w
}

func TestAWorkerJoinsAndLeavesCleanly(t *testing.T) {
	fast(t)
	w := joinStandin(t, "good", nil)
	w.Leave()
	require.NoError(t, within(time.Second, w.Done()))
	require.Equal(t, Exit{Reason: "left"}, w.Exit())
}

// A crash is a pipe that closed without the worker saying so or being asked, or an exit that is not 0.
func TestACrashIsSeenThroughTheClosedPipe(t *testing.T) {
	fast(t)
	cases := map[string]func(t *testing.T) *Worker{
		"a panic": func(t *testing.T) *Worker {
			w, err := Start(context.Background(), standinOptions(t, "panic", nil))
			require.NoError(t, err)
			require.NoError(t, w.Send(voiceipc.OpJoin, voiceipc.Join{ICEServers: []voiceipc.ICEServer{}}))
			return w
		},
		"a kill": func(t *testing.T) *Worker {
			w := joinStandin(t, "good", nil)
			p, err := os.FindProcess(w.PID())
			require.NoError(t, err)
			require.NoError(t, p.Kill())
			return w
		},
		"an exit in a call": func(t *testing.T) *Worker { return joinStandin(t, "crashincall", nil) },
		"an exit at once": func(t *testing.T) *Worker {
			w, err := Start(context.Background(), standinOptions(t, "exit3", nil))
			require.NoError(t, err)
			return w
		},
	}
	for name, start := range cases {
		t.Run(name, func(t *testing.T) {
			w := start(t)
			drain(t, w, 5*time.Second)
			exit := w.Exit()
			require.True(t, exit.Crashed, "%+v", exit)
			require.Contains(t, exit.Reason, "stopped unexpectedly")
		})
	}
}

// A call that ends by itself is not a crash, and its reason arrives cleaned: it is the voice server's
// words, carried by a worker that may be hostile.
func TestACallThatEndsByItselfIsNotACrash(t *testing.T) {
	fast(t)
	w, err := Start(context.Background(), standinOptions(t, "dropcall", nil))
	require.NoError(t, err)
	require.NoError(t, w.Send(voiceipc.OpJoin, voiceipc.Join{ICEServers: []voiceipc.ICEServer{}}))
	<-w.Events()
	require.NoError(t, w.Send(voiceipc.OpAnswer, voiceipc.Answer{SDP: "v=0\r\n"}))
	drain(t, w, 5*time.Second)
	exit := w.Exit()
	require.False(t, exit.Crashed)
	require.Contains(t, exit.Reason, "the voice server closed the call")
	require.NotContains(t, exit.Reason, "\x1b")
}

// Every way a worker can break the protocol kills it, and the daemon goes on.
func TestAHostileWorkerIsKilled(t *testing.T) {
	fast(t)
	for _, mode := range []string{"oversized", "garbage", "unknownop", "daemonop", "badstate", "flood",
		"printflood", "nonewline"} {
		t.Run(mode, func(t *testing.T) {
			w, err := Start(context.Background(), standinOptions(t, mode, nil))
			require.NoError(t, err)
			drain(t, w, 10*time.Second)
			exit := w.Exit()
			require.True(t, exit.Crashed, "%+v", exit)
			require.NotContains(t, exit.Reason, "stopped unexpectedly", "killed by the daemon, not crashed by itself")
		})
	}
}

// A worker that does not say who it is, or is of another release, is never used.
func TestAWorkerMustSayWhoItIs(t *testing.T) {
	fast(t)
	helloWithin = 200 * time.Millisecond
	_, err := Start(context.Background(), standinOptions(t, "silent", nil))
	require.ErrorIs(t, err, ErrStart)
	require.Contains(t, err.Error(), "did not say who it is")

	// One of this release saying more than its HELLO declares broke the protocol.
	_, err = Start(context.Background(), standinOptions(t, "extrahello", nil))
	require.ErrorIs(t, err, ErrStart)
	require.Contains(t, err.Error(), "broke the protocol")

	// A newer MINOR saying more in its HELLO is told it is of another release, not that it broke anything.
	_, err = Start(context.Background(), standinOptions(t, "newerhello", nil))
	require.ErrorIs(t, err, ErrStart)
	require.Contains(t, err.Error(), "reinstall Norite")
	require.NotContains(t, err.Error(), "broke the protocol")

	_, err = Start(context.Background(), standinOptions(t, "badversion", nil))
	require.ErrorIs(t, err, ErrStart)
	require.Contains(t, err.Error(), "9.0.0")
	require.Contains(t, err.Error(), "0.4.0")
	require.NotContains(t, err.Error(), "upgrade the server", "the two are one install, replaced together")
}

// A worker that ignores its leave and its closed pipe is killed after the bound, and one that closes its
// pipe and stays is killed too.
func TestAWorkerThatWillNotGoIsMadeTo(t *testing.T) {
	fast(t)
	w := joinStandin(t, "stubborn", nil)
	start := time.Now()
	w.Leave()
	require.Less(t, time.Since(start), leaveWithin+2*time.Second)
	require.True(t, w.Exit().Crashed, "a worker that had to be killed is reported so")

	w, err := Start(context.Background(), standinOptions(t, "closepipe", nil))
	require.NoError(t, err)
	drain(t, w, exitWithin+2*time.Second)
	require.True(t, w.Exit().Crashed)
}

// What the worker prints reaches the log as quoted fields, and never the protocol: a C library writing to
// descriptor 1 mid-call changes nothing.
func TestWhatTheWorkerPrintsIsLoggedNotRead(t *testing.T) {
	fast(t)
	log := &logBuffer{}
	w := joinStandin(t, "noise", log)
	w.Leave()
	require.False(t, w.Exit().Crashed)
	require.Eventually(t, func() bool {
		for _, l := range log.workerLines() {
			if strings.Contains(l, "hello not a frame") {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond)
}

// The worker is given the allow-list and nothing else of the daemon's environment.
func TestTheWorkerDoesNotInheritTheDaemonsSecrets(t *testing.T) {
	fast(t)
	t.Setenv("NORITE_API_TOKEN", "nrt_fake_for_the_test")
	t.Setenv("NORITE_PASSWORD", "fake-password-for-the-test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fake-for-the-test")
	t.Setenv("LC_ALL", "C.UTF-8")
	log := &logBuffer{}
	w := joinStandin(t, "env", log)
	w.Leave()
	var names []string
	require.Eventually(t, func() bool {
		names = nil
		for _, l := range log.workerLines() {
			if n, ok := strings.CutPrefix(l, "env "); ok {
				names = append(names, n)
			}
		}
		return len(names) > 0
	}, 2*time.Second, 10*time.Millisecond)
	require.NotContains(t, names, "NORITE_API_TOKEN")
	require.NotContains(t, names, "NORITE_PASSWORD")
	require.NotContains(t, names, "AWS_SECRET_ACCESS_KEY")
	if runtime.GOOS != "windows" {
		require.Contains(t, names, "LC_ALL")
	}
	require.Contains(t, names, standinVariable, "the test's own, appended after the list")
}

// On Linux the worker holds descriptors 0 to 4 and nothing else the daemon had open.
func TestTheWorkerInheritsOnlyItsPipes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	fast(t)
	held, err := os.Open(os.Args[0]) // something the daemon holds, which must not cross
	require.NoError(t, err)
	defer func() { _ = held.Close() }()
	log := &logBuffer{}
	w := joinStandin(t, "fds", log)
	w.Leave()
	var fds []string
	require.Eventually(t, func() bool {
		for _, l := range log.workerLines() {
			if list, ok := strings.CutPrefix(l, "fds "); ok {
				fds = strings.Fields(list)
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond)
	var numbers []string
	for _, fd := range fds {
		n, target, _ := strings.Cut(fd, "=")
		numbers = append(numbers, n)
		require.NotEqual(t, held.Name(), target, "a file the daemon holds crossed to the worker")
	}
	require.Equal(t, []string{"0", "1", "2", "3", "4"}, numbers, "%v", fds)
}

// An exit code reads as whoever looks it up writes it.
func TestAnExitCodeReadsAsTheOSWritesIt(t *testing.T) {
	require.Equal(t, "exit status 2", exitStatus("linux", 2))
	require.Equal(t, "exit status 2", exitStatus("windows", 2))
	require.Equal(t, "exit status 0xFFFFFFFF", exitStatus("windows", 4294967295))
	require.Equal(t, "exit status 0xC0000005", exitStatus("windows", 0xC0000005))
}

// A worker that stops reading its pipe cannot hold the daemon's writes: the write is bounded, and the
// worker killed, so the Leave and the stop behind it go on.
func TestAWorkerThatStopsReadingCannotHoldTheDaemon(t *testing.T) {
	fast(t)
	sendWithin = 300 * time.Millisecond
	w, err := Start(context.Background(), standinOptions(t, "deaf", nil))
	require.NoError(t, err)
	big := voiceipc.Answer{SDP: strings.Repeat("a", 60<<10)}
	failed := make(chan error, 1)
	go func() {
		for {
			if err := w.Send(voiceipc.OpAnswer, big); err != nil {
				failed <- err
				return
			}
		}
	}()
	select {
	case err := <-failed:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("a write to a worker that does not read was not bounded")
	}
	left := make(chan struct{})
	go func() { w.Leave(); close(left) }()
	require.NoError(t, within(5*time.Second, left), "Leave waited on a held write")
	require.True(t, w.Exit().Crashed)
	require.Contains(t, w.Exit().Reason, "stopped reading")
}

// A worker that says how its call ended and exits at once is never taken for a crash, however the
// goroutines that read it and reap it are ordered.
func TestAnEndSaidLastIsNeverACrash(t *testing.T) {
	fast(t)
	// Every frame is read late, so the worker has exited, and been reaped, before its last one is read.
	readHook = func() { time.Sleep(200 * time.Millisecond) }
	t.Cleanup(func() { readHook = nil })
	for range 3 {
		w, err := Start(context.Background(), standinOptions(t, "dropcall", nil))
		require.NoError(t, err)
		require.NoError(t, w.Send(voiceipc.OpJoin, voiceipc.Join{ICEServers: []voiceipc.ICEServer{}}))
		<-w.Events()
		require.NoError(t, w.Send(voiceipc.OpAnswer, voiceipc.Answer{SDP: "v=0\r\n"}))
		// Nobody reads the events from here: the reader must still record the end.
		require.NoError(t, within(5*time.Second, w.Done()))
		require.False(t, w.Exit().Crashed, "%+v", w.Exit())
		require.Contains(t, w.Exit().Reason, "the voice server closed the call")
	}
}

// Empty lines cost a byte each and a log entry each: the entries are bounded apart, and what is dropped is
// counted.
func TestPrintingIsBoundedInLogEntriesToo(t *testing.T) {
	fast(t)
	log := &logBuffer{}
	w, err := Start(context.Background(), standinOptions(t, "shortlines", log))
	require.NoError(t, err)
	w.Leave()
	entries, warned := 0, false
	for _, m := range log.lines() {
		switch m["message"] {
		case "voice worker":
			entries++
		case "voice worker lines past the rate, not logged":
			warned = true
		}
	}
	require.LessOrEqual(t, entries, 4*linesPerSecond, "20,000 short lines made %d entries", entries)
	require.True(t, warned, "the lines not logged were not counted")
}

// A daemon that does not know its own path does not look its worker up on PATH. A working worker named
// norite-voice is put first on PATH, so only the guard stands between it and being run.
func TestAWorkerIsStartedByPathOnly(t *testing.T) {
	fast(t)
	require.Empty(t, ProgramBeside(""))
	exe, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	name := Program
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	body, err := os.ReadFile(exe) //nolint:gosec // this test binary
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), body, 0o755)) //nolint:gosec // a stand-in program
	t.Setenv("PATH", dir)
	for _, program := range []string{"", Program, "./" + Program} {
		o := standinOptions(t, "good", nil)
		o.Program = program
		w, err := Start(context.Background(), o)
		if err == nil {
			w.Kill("the test ended")
			<-w.Done()
		}
		require.ErrorIs(t, err, ErrStart, "%q was started", program)
	}
}

// The daemon's ends of a worker's pipes are closed once it is reaped, not left for the collector: every
// call opens three.
func TestAReapedWorkersPipesAreClosed(t *testing.T) {
	fast(t)
	w := joinStandin(t, "good", nil)
	w.Leave()
	_, err := w.fromWorker.Read(make([]byte, 1))
	require.ErrorIs(t, err, os.ErrClosed)
	_, err = w.output.Read(make([]byte, 1))
	require.ErrorIs(t, err, os.ErrClosed)
}

// What a worker says in its HELLO is believed past no bound: neither the version it names nor a field
// strict decoding quotes reaches the answer or the log whole.
func TestAHelloOfAnyLengthIsQuotedWithinBounds(t *testing.T) {
	fast(t)
	for _, mode := range []string{"longversion", "longfield"} {
		t.Run(mode, func(t *testing.T) {
			log := &logBuffer{}
			_, err := Start(context.Background(), standinOptions(t, mode, log))
			require.Error(t, err)
			if mode == "longversion" {
				require.Less(t, len(err.Error()), 1024, "the answer quotes the whole version")
			}
			require.Eventually(t, func() bool { return strings.Contains(log.String(), "crashed") }, 5*time.Second,
				10*time.Millisecond)
			for _, line := range strings.Split(log.String(), "\n") {
				require.Less(t, len(line), 2048, "a log line of %d bytes", len(line))
			}
		})
	}
}

// A pipe something the worker started still holds open does not keep the daemon's readers for ever: they
// are ended once the worker is reaped and its last words drained.
func TestPipesAnHeirHoldsAreLetGo(t *testing.T) {
	fast(t)
	w, err := Start(context.Background(), standinOptions(t, "heir", nil))
	require.NoError(t, err)
	go func() {
		for range w.Events() {
		}
	}()
	require.NoError(t, within(5*time.Second, w.Done()))
	require.NoError(t, within(3*time.Second, w.readDone), "the frame reader is still reading the heir's pipe")
	require.NoError(t, within(3*time.Second, w.outDone), "the output reader is still reading the heir's pipe")
}
