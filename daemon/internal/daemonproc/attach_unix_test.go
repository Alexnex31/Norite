// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package daemonproc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/daemon/credentials"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// The daemon opens its attach socket and serves it: a client attaches, is told who the daemon is signed in
// as, and has a request relayed with the daemon's token. Once the daemon stops, nothing is listening and no
// socket file is left behind for the next start to wonder about.
func TestTheDaemonServesItsAttachSocket(t *testing.T) {
	instance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/refresh":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "eyJ.access", "refresh_token": "nrt_rotated", "token_type": "Bearer",
				"expires_at": time.Now().Add(15 * time.Minute).Format(time.RFC3339Nano),
			})
		case "/api/v1/users/@me":
			if r.Header.Get("Authorization") != "Bearer eyJ.access" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"id":"1","username":"ada"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(instance.Close)

	dir, err := os.MkdirTemp("", "nd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	store, err := credentials.OpenLocalForTest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(credentials.Record{
		InstanceURL: instance.URL, UserID: "1", Username: "ada", DeviceID: "dev_test", DeviceName: "laptop",
	}, "nrt_from_login"); err != nil {
		t.Fatal(err)
	}

	stop, _ := startDaemon(t, Options{StateDir: dir, Version: "dev"})

	var client *ipc.Client
	deadline := time.Now().Add(10 * time.Second)
	for client == nil {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		conn, err := ipc.DialAt(ctx, ipc.SocketPath(dir))
		if err != nil {
			cancel()
			t.Fatalf("dialing the daemon: %v", err)
		}
		c, err := ipc.Attach(ctx, conn, ipc.Options{Client: "norite-test", Version: "dev"})
		cancel()
		if err != nil {
			t.Fatalf("attaching: %v", err)
		}
		if c.Ready().Account != nil {
			client = c
			break
		}
		_ = c.Close()
		if time.Now().After(deadline) {
			t.Fatal("the daemon never reported itself signed in")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := client.Ready().Account.UserID; got != "1" {
		t.Errorf("READY names user %q, want 1", got)
	}

	res, err := client.Do(t.Context(), "GET", "/users/@me", nil)
	if err != nil {
		t.Fatalf("relaying: %v", err)
	}
	if res.Status != http.StatusOK || !strings.Contains(string(res.Body), `"ada"`) {
		t.Errorf("relayed GET /users/@me answered %d %s", res.Status, res.Body)
	}
	_ = client.Close()

	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := os.Stat(ipc.SocketPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the socket outlived the daemon: %v", err)
	}
	if _, err := ipc.DialAt(t.Context(), ipc.SocketPath(dir)); !errors.Is(err, ipc.ErrNotRunning) {
		t.Errorf("dialing a stopped daemon: %v, want ErrNotRunning", err)
	}
}

// A daemon that cannot open its socket stops with ErrMisconfigured, which daemond exits 4 for and the
// systemd unit does not retry: a restart cannot fix a file that is not a socket where the socket goes.
func TestADaemonThatCannotListenIsMisconfigured(t *testing.T) {
	dir, err := os.MkdirTemp("", "nd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.WriteFile(ipc.SocketPath(dir), []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}

	err = Run(t.Context(), Options{StateDir: dir, Version: "dev", Stderr: io.Discard})
	if !errors.Is(err, ErrMisconfigured) {
		t.Fatalf("Run returned %v, want ErrMisconfigured", err)
	}
	if !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("the reason is lost: %v", err)
	}
}

// A daemon asked to stop through its socket stops as it does on a signal: the asker reads its answer, Run
// returns nil, which is exit 0 and what no service manager restarts, and nothing is left listening.
func TestTheDaemonStopsWhenAnAttachedClientAsks(t *testing.T) {
	dir, err := os.MkdirTemp("", "nd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	done := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		// A context nothing cancels: the only thing that can end this run is the request.
		done <- Run(context.Background(), Options{
			StateDir: dir, Version: "dev", LogLevel: zerolog.Disabled, Ready: func() { close(ready) },
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("the daemon exited before becoming ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not become ready")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := ipc.DialAt(ctx, ipc.SocketPath(dir))
	if err != nil {
		t.Fatalf("dialing the daemon: %v", err)
	}
	client, err := ipc.Attach(ctx, conn, ipc.Options{Client: "norite-test", Version: "dev"})
	if err != nil {
		t.Fatalf("attaching: %v", err)
	}
	defer func() { _ = client.Close() }()

	res, err := client.Do(ctx, "POST", ipc.PathStop, nil)
	if err != nil {
		t.Fatalf("asking the daemon to stop: %v", err)
	}
	var out ipc.Stopping
	if res.Status != http.StatusOK || json.Unmarshal(res.Body, &out) != nil || out.PID != os.Getpid() {
		t.Fatalf("the stop request answered %d %s", res.Status, res.Body)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a requested stop returned %v, want nil: anything else is a crash to a service manager", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not stop")
	}
	if _, err := ipc.DialAt(t.Context(), ipc.SocketPath(dir)); !errors.Is(err, ipc.ErrNotRunning) {
		t.Errorf("dialing a stopped daemon: %v, want ErrNotRunning", err)
	}
}
