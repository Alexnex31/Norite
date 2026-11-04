// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/voiceipc"
)

// The stand-in worker is this test binary, started again with standinVariable naming what to play: a
// correct worker, or each way a hostile or broken one could behave. The daemon's half under test is the
// real one; only the worker is played.
const standinVariable = "NORITE_VOICE_STANDIN"

func TestMain(m *testing.M) {
	if mode := os.Getenv(standinVariable); mode != "" {
		os.Exit(standin(mode))
	}
	os.Exit(m.Run())
}

// standinOptions starts the stand-in playing mode.
func standinOptions(t *testing.T, mode string, log io.Writer) Options {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if log == nil {
		log = io.Discard
	}
	return Options{Program: exe, Version: "0.4.0", Log: zerolog.New(log),
		Env: []string{standinVariable + "=" + mode}}
}

// standin plays mode on the pipes -pipes names, and returns its exit code.
func standin(mode string) int {
	if mode == "parent" {
		return standinParent()
	}
	fds := listFDs() // before anything here opens one
	fs := flag.NewFlagSet("standin", flag.ContinueOnError)
	pipes := fs.String(voiceipc.PipesFlag, "", "")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return 9
	}
	r, w, err := voiceipc.ParsePipes(*pipes)
	if err != nil {
		return 9
	}
	in, out := os.NewFile(r, "control"), os.NewFile(w, "events")
	send := func(op gatewayproto.Opcode, v any) { _ = voiceipc.Write(out, op, v) }
	version := "0.4.7"

	switch mode {
	case "silent":
		time.Sleep(time.Minute)
		return 0
	case "badversion":
		version = "9.0.0"
	case "newerhello":
		// A newer MINOR whose HELLO says more, as a MINOR change may.
		_ = ipc.WriteEncoded(out, []byte(`{"op":1,"d":{"version":"0.5.0","capabilities":["video"]},"s":null,"t":null}`))
		time.Sleep(time.Minute)
		return 0
	case "extrahello":
		_ = ipc.WriteEncoded(out, []byte(`{"op":1,"d":{"version":"0.4.7","token":"x"},"s":null,"t":null}`))
		time.Sleep(time.Minute)
		return 0
	case "fds":
		fmt.Println("fds " + strings.Join(fds, " "))
	case "env":
		for _, kv := range os.Environ() {
			name, _, _ := strings.Cut(kv, "=")
			fmt.Println("env " + name)
		}
	case "noise":
		// What a C library's printf does: straight to descriptor 1, mid-protocol.
		fmt.Print("\x00\x00\x00\x05hello not a frame\n")
	}
	send(voiceipc.OpHello, voiceipc.Hello{Version: version})

	switch mode {
	case "oversized":
		_ = ipc.WriteEncoded(out, append([]byte(`{"op":3,"d":{"state":"ready","reason":"`),
			bytes.Repeat([]byte("a"), voiceipc.MaxWorkerFrame)...))
	case "garbage":
		_, _ = out.Write([]byte("\x00\x00\x00\x08not json"))
	case "unknownop":
		send(99, struct{}{})
	case "daemonop":
		send(voiceipc.OpJoin, voiceipc.Join{ICEServers: []voiceipc.ICEServer{}})
	case "badstate":
		send(voiceipc.OpState, voiceipc.State{State: "dancing"})
	case "flood":
		for range 10 * framesPerSecond {
			send(voiceipc.OpState, voiceipc.State{State: voiceipc.StateConnecting})
		}
	case "printflood":
		line := strings.Repeat("x", 4096) + "\n"
		for range 4096 {
			if _, err := os.Stderr.WriteString(line); err != nil {
				break
			}
		}
	case "exit3":
		return 3
	case "deaf":
		// Says who it is, then never reads its control pipe again.
		time.Sleep(time.Minute)
		return 0
	case "nonewline":
		chunk := strings.Repeat("x", 64<<10)
		for range 1 << 12 {
			if _, err := os.Stderr.WriteString(chunk); err != nil {
				break
			}
		}
		time.Sleep(time.Minute)
	case "shortlines":
		// Under the byte bound, and a log entry each. Empty lines make none.
		for range 20000 {
			_, _ = os.Stdout.WriteString("x\n")
		}
	case "closepipe":
		// Closes its pipe and stays: the daemon must not wait on it for ever.
		_ = out.Close()
		time.Sleep(time.Minute)
		return 0
	}

	// A correct worker from here, with a few modes that break it at a step.
	for {
		f, err := voiceipc.Read(in, voiceipc.MaxDaemonFrame)
		if err != nil {
			if mode == "stubborn" {
				time.Sleep(time.Minute)
			}
			return 0
		}
		switch f.Op {
		case voiceipc.OpJoin:
			switch mode {
			case "panic":
				panic("a decoder fed something that is not Opus")
			case "endearly":
				send(voiceipc.OpState, voiceipc.State{State: voiceipc.StateEnded, Reason: "no route to the voice server"})
				return 0
			case "stateFirst":
				send(voiceipc.OpState, voiceipc.State{State: voiceipc.StateConnecting})
				continue
			}
			send(voiceipc.OpOffer, voiceipc.Offer{SDP: "v=0\r\nstandin\r\n"})
		case voiceipc.OpAnswer:
			send(voiceipc.OpState, voiceipc.State{State: voiceipc.StateConnecting})
			if mode == "neverready" {
				continue
			}
			send(voiceipc.OpState, voiceipc.State{State: voiceipc.StateReady})
			if mode == "dropcall" {
				send(voiceipc.OpState, voiceipc.State{State: voiceipc.StateEnded,
					Reason: "the voice server closed the call \x1b[31m"})
				return 0
			}
			if mode == "crashincall" {
				os.Exit(2)
			}
		case voiceipc.OpLeave:
			if mode == "stubborn" {
				continue
			}
			send(voiceipc.OpState, voiceipc.State{State: voiceipc.StateEnded, Reason: "left"})
			return 0
		}
	}
}

// listFDs is the open descriptors on Linux, each as number=target, but two this process opened itself: the
// one reading the list, closed by the time its link is read, and the cgroup file Go's runtime keeps open
// from start to follow the CPU limit (GOMAXPROCS, since Go 1.25). Elsewhere it is nothing.
func listFDs() []string {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
		if err != nil || strings.HasPrefix(target, "/sys/fs/cgroup/") {
			continue
		}
		out = append(out, e.Name()+"="+target)
	}
	num := func(s string) int { n, _ := strconv.Atoi(s[:strings.IndexByte(s, '=')]); return n }
	sort.Slice(out, func(i, j int) bool { return num(out[i]) < num(out[j]) })
	return out
}

// standinParent is a daemon in miniature: it starts a correct stand-in worker, prints its pid, and waits to
// be killed. The orphan test kills it and watches the worker go.
func standinParent() int {
	exe, err := os.Executable()
	if err != nil {
		return 9
	}
	w, err := Start(context.Background(), Options{Program: exe, Version: "0.4.0", Log: zerolog.Nop(),
		Env: []string{standinVariable + "=good"}})
	if err != nil {
		fmt.Println("error " + err.Error())
		return 9
	}
	fmt.Println("worker " + strconv.Itoa(w.PID()))
	time.Sleep(time.Minute)
	return 0
}

// logBuffer is a log the test reads while the worker writes it.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// lines are the log's entries, decoded.
func (l *logBuffer) lines() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, raw := range bytes.Split(l.buf.Bytes(), []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// workerLines are the lines the worker printed, as the daemon logged them.
func (l *logBuffer) workerLines() []string {
	var out []string
	for _, m := range l.lines() {
		if m["message"] == "voice worker" {
			s, _ := m["line"].(string)
			out = append(out, s)
		}
	}
	return out
}

var errTimeout = errors.New("timed out")

func within(d time.Duration, ch <-chan struct{}) error {
	select {
	case <-ch:
		return nil
	case <-time.After(d):
		return errTimeout
	}
}
