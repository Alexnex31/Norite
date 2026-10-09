// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Alexnex31/Norite/daemon/credentials"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// descriptorLimit is the limit the daemon is run under: what macOS gives a process by default, and the
// figure three documents said the daemon had to raise itself above.
const descriptorLimit = 256

// buildDaemon builds this package's binary, the one a release ships, into a temporary directory.
func buildDaemon(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "norite-daemon")
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the daemon: %v\n%s", err, out)
	}
	return binary
}

// The daemon's handles are bounded by its own caps, and the caps are low enough for the lowest limit an
// operating system hands out.
//
// M3 raised RLIMIT_NOFILE at startup and M23 found that it never had: Go's runtime raises the soft limit
// to the hard one before main. So nothing here relies on a raise. The real binary is started with the
// hard limit itself at 256, which no program can raise, and then every cap it has is filled and pushed
// past: 64 attached clients, the port's 16 scripts and 32 connections still to present a secret, and
// several hundred more of each that it has to refuse. It goes on answering throughout, and its log never
// says it ran out.
//
// It fails if a cap goes: with ipc.MaxClients raised past the limit, the connections this test expects
// to be refused are held instead, and the daemon runs out of descriptors.
func TestTheDaemonHoldsEveryCapUnderTheLowestDescriptorLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real daemon binary")
	}
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("needs a shell to lower the limit the daemon starts under")
	}
	binary := buildDaemon(t)

	instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/refresh":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "eyJ.access", "refresh_token": "nrt_rotated", "token_type": "Bearer",
				"expires_at": time.Now().Add(15 * time.Minute).Format(time.RFC3339Nano),
			})
		case "/api/v1/users/@me":
			_, _ = w.Write([]byte(`{"id":"1","username":"ada"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(instance.Close)

	// Short, because the attach socket's path has a limit of its own.
	home, err := os.MkdirTemp("", "nd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	stateDir := filepath.Join(home, "s", "norite")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := credentials.OpenLocalForTest(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(credentials.Record{
		InstanceURL: instance.URL, UserID: "1", Username: "ada", DeviceID: "dev_test", DeviceName: "laptop",
	}, "nrt_from_login"); err != nil {
		t.Fatal(err)
	}

	// `ulimit -n` sets the hard limit and the soft one with it. The daemon replaces the shell, so the
	// process this test waits on is the daemon.
	daemon := exec.Command(shell, "-c", `ulimit -n `+strconv.Itoa(descriptorLimit)+` && exec "$0" -stderr-log=false`, binary) //nolint:gosec // a fixed script and the binary just built
	daemon.Env = []string{
		"HOME=" + home, "XDG_STATE_HOME=" + filepath.Join(home, "s"), "XDG_CONFIG_HOME=" + filepath.Join(home, "c"),
		"PATH=" + os.Getenv("PATH"),
	}
	var stderr bytes.Buffer
	daemon.Stderr = &stderr
	if err := daemon.Start(); err != nil {
		t.Fatalf("starting the daemon: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- daemon.Wait() }()
	t.Cleanup(func() {
		_ = daemon.Process.Kill()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
		}
	})

	socket := ipc.SocketPath(stateDir)
	logPath := filepath.Join(stateDir, "daemon.log")
	readLog := func() string {
		body, _ := os.ReadFile(logPath)
		return string(body)
	}
	fail := func(format string, args ...any) {
		t.Helper()
		t.Logf("the daemon's stderr:\n%s", stderr.String())
		t.Logf("the daemon's log:\n%s", readLog())
		t.Fatalf(format, args...)
	}

	attach := func() (*ipc.Client, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		conn, err := ipc.DialAt(ctx, socket)
		if err != nil {
			return nil, err
		}
		return ipc.Attach(ctx, conn, ipc.Options{Client: "norite-test", Version: "dev"})
	}

	// Up, and signed in, which the port needs before it can be turned on.
	var first *ipc.Client
	for deadline := time.Now().Add(20 * time.Second); first == nil; {
		select {
		case err := <-exited:
			fail("the daemon exited before it was ready: %v", err)
		default:
		}
		if c, err := attach(); err == nil {
			if c.Ready().Account != nil {
				first = c
				break
			}
			_ = c.Close()
		}
		if time.Now().After(deadline) {
			fail("the daemon never reported itself signed in")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(readLog(), `"open_file_limit":`+strconv.Itoa(descriptorLimit)+`,`) {
		fail("the daemon is not running under a limit of %d, so this test proves nothing", descriptorLimit)
	}

	local := func(c *ipc.Client, method, path string) ipc.Result {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		res, err := c.Do(ctx, method, path, nil)
		if err != nil || res.Status != http.StatusOK {
			fail("%s %s: %d %s, %v", method, path, res.Status, res.Body, err)
		}
		return res
	}

	// A port nothing else holds, found by asking for one and letting it go.
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	var status ipc.AutomationStatus
	if err := json.Unmarshal(local(first, "POST", ipc.PathAutomationEnable(port)).Body, &status); err != nil || !status.Open {
		fail("the port did not open: %+v, %v", status, err)
	}
	file, err := ipc.LoadAutomationFile(stateDir)
	if err != nil {
		fail("reading the port's file: %v", err)
	}

	// Every cap, filled.
	clients := []*ipc.Client{first}
	for len(clients) < ipc.MaxClients {
		c, err := attach()
		if err != nil {
			fail("attaching client %d of %d: %v", len(clients)+1, ipc.MaxClients, err)
		}
		clients = append(clients, c)
	}
	var scripts []*ipc.AutomationClient
	for len(scripts) < ipc.MaxAutomationConns {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		s, err := ipc.DialAutomation(ctx, file, "nat_a_token_shaped_value")
		cancel()
		if err != nil {
			fail("script %d of %d: %v", len(scripts)+1, ipc.MaxAutomationConns, err)
		}
		scripts = append(scripts, s)
	}
	var held []net.Conn
	t.Cleanup(func() {
		for _, c := range held {
			_ = c.Close()
		}
		for _, s := range scripts {
			_ = s.Close()
		}
		for _, c := range clients {
			_ = c.Close()
		}
	})

	// And pushed past, by more than the limit has room for. Whatever the daemon does with each of these,
	// it must not keep it: connections that have said nothing to the port, and clients past the cap.
	for range 2 * descriptorLimit {
		if conn, err := net.DialTimeout("tcp4", file.Address, 2*time.Second); err == nil {
			held = append(held, conn)
		}
	}
	refused := 0
	for range 2 * descriptorLimit {
		c, err := attach()
		if err == nil {
			// Held, as a client that got in would be.
			clients = append(clients, c)
			continue
		}
		var closed *ipc.CloseError
		if errors.As(err, &closed) && closed.Code == ipc.CloseTooManyClients {
			refused++
		}
	}
	if refused != 2*descriptorLimit {
		fail("%d of %d clients past the cap were refused for being past it", refused, 2*descriptorLimit)
	}

	// Still answering: every client it holds, every script, and a newcomer once somebody leaves.
	for i, c := range clients {
		var where ipc.ConfigLocation
		if err := json.Unmarshal(local(c, "GET", ipc.PathConfig).Body, &where); err != nil || where.Dir == "" {
			fail("client %d: %v", i, err)
		}
	}
	for i, s := range scripts {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		resp, err := s.Do(ctx, "GET", "/users/@me", nil)
		cancel()
		if err != nil || resp.Error != nil || resp.Status == nil || *resp.Status != http.StatusOK {
			fail("script %d: %+v, %v", i, resp, err)
		}
	}
	_ = clients[len(clients)-1].Close()
	clients = clients[:len(clients)-1]
	var newcomer *ipc.Client
	for deadline := time.Now().Add(10 * time.Second); newcomer == nil; {
		if c, err := attach(); err == nil {
			newcomer = c
			break
		}
		if time.Now().After(deadline) {
			fail("nobody could attach after a client left")
		}
		time.Sleep(20 * time.Millisecond)
	}
	clients = append(clients, newcomer)

	if log := readLog(); strings.Contains(log, "too many open files") {
		fail("the daemon ran out of descriptors")
	}

	// And it stops when asked, with the exit a service manager does not restart.
	local(newcomer, "POST", ipc.PathStop)
	select {
	case err := <-exited:
		if err != nil {
			fail("the daemon exited with %v, want 0", err)
		}
	case <-time.After(20 * time.Second):
		fail("the daemon did not stop")
	}
}
