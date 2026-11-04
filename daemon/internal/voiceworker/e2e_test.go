// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceworker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/ipc"
)

// The done-when, end to end: the daemon's calls start the real norite-voice, which connects to M26's SFU
// through M27's harness, with TURN beside it; leaving ends it; killing it is seen. Both programs are built
// from this checkout.

func buildProgram(t *testing.T, moduleDir, pkg, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", out, pkg) //nolint:gosec // fixed arguments
	cmd.Dir = moduleDir
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", pkg, err, b)
	}
	return out
}

func freePort(t *testing.T, network string) int {
	t.Helper()
	if network == "udp" {
		c, err := net.ListenPacket("udp", "127.0.0.1:0")
		require.NoError(t, err)
		defer func() { _ = c.Close() }()
		return c.LocalAddr().(*net.UDPAddr).Port
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// harness is M26's harness running, its log kept to read.
type harness struct {
	url string
	mu  sync.Mutex
	log strings.Builder
}

func (h *harness) logged(s string) bool { return strings.Contains(h.output(), s) }

func (h *harness) output() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.log.String()
}

func startHarness(t *testing.T) *harness {
	t.Helper()
	sfu := buildProgram(t, filepath.Join("..", "..", "..", "backend"), "./internal/voice/voicetest/cmd/voice-sfu", "voice-sfu")
	httpAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t, "tcp"))
	cmd := exec.Command(sfu, "-loopback", //nolint:gosec // the harness just built
		"-media", fmt.Sprintf("127.0.0.1:%d", freePort(t, "udp")),
		"-turn", fmt.Sprintf("127.0.0.1:%d", freePort(t, "tcp")),
		"-http", httpAddr)
	out, err := cmd.StderrPipe()
	require.NoError(t, err)
	cmd.Stdout = io.Discard
	require.NoError(t, cmd.Start())
	h := &harness{url: "http://" + httpAddr}
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			h.mu.Lock()
			h.log.WriteString(sc.Text() + "\n")
			h.mu.Unlock()
		}
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	require.Eventually(t, func() bool {
		res, err := http.Post(h.url+"/credentials", "application/json", strings.NewReader(`{"user":"1"}`))
		if err != nil {
			return false
		}
		_ = res.Body.Close()
		return res.StatusCode == http.StatusOK
	}, 30*time.Second, 50*time.Millisecond, "the harness did not come up")
	return h
}

type statuses struct {
	mu   sync.Mutex
	seen []ipc.VoiceStatus
}

func (s *statuses) add(st ipc.VoiceStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, st)
}

func (s *statuses) states() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, st := range s.seen {
		out = append(out, st.State)
	}
	return out
}

func TestJoiningVoiceStartsTheWorkerAndLeavingEndsIt(t *testing.T) {
	if testing.Short() {
		t.Skip("builds norite-voice and M26's harness, and makes real calls")
	}
	worker := buildProgram(t, filepath.Join("..", "..", "..", "voiceworker"), "./cmd/norite-voice", Program)
	h := startHarness(t)
	seen := &statuses{}
	v := NewVoice(VoiceOptions{
		Program: worker, Version: "dev", Signaler: Harness{URL: h.url},
		Account: func() (string, error) { return "42", nil },
		Changed: seen.add, Log: zerolog.New(io.Discard),
	})
	t.Cleanup(v.Close)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	st, err := v.Join(ctx, "1001")
	require.NoError(t, err)
	require.Equal(t, ipc.VoiceStatus{State: ipc.VoiceInCall, Channel: "1001", Available: true}, st)
	require.True(t, h.logged("provisioned"), "the SFU provisioned the worker's connection")
	require.True(t, v.Busy())

	st = v.Leave()
	require.Equal(t, ipc.VoiceStatus{State: ipc.VoiceIdle, Reason: "left", Available: true}, st)
	require.False(t, v.Busy())
	require.Equal(t, []string{ipc.VoiceJoining, ipc.VoiceInCall, ipc.VoiceIdle}, seen.states())

	// A crash, seen through the closed pipe, ends the call with its reason; the next join works. Each step
	// takes a room of its own: the SFU counts a killed worker's connection until ICE gives up on it, and
	// holds two per user in a room (M36's entry).
	_, err = v.Join(ctx, "1002")
	require.NoError(t, err)
	v.mu.Lock()
	pid := v.current.worker.PID()
	v.mu.Unlock()
	p, err := os.FindProcess(pid)
	require.NoError(t, err)
	require.NoError(t, p.Kill())
	require.Eventually(t, func() bool { return v.Status().State == ipc.VoiceIdle }, 10*time.Second, 10*time.Millisecond)
	require.Contains(t, v.Status().Reason, "stopped unexpectedly")
	require.False(t, v.Busy())

	// Joining another channel while in a call leaves the first.
	_, err = v.Join(ctx, "1003")
	require.NoError(t, err)
	st, err = v.Join(ctx, "1004")
	require.NoError(t, err)
	require.Equal(t, "1004", st.Channel)
	require.Equal(t, ipc.VoiceInCall, st.State)
}
