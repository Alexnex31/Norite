// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package daemonproc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Alexnex31/Norite/daemon/config"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// M21's first done-when, at the daemon: with a real daemon running and a client attached for events,
// editing config.toml reaches the client without a restart. Nobody is signed in, which is the state of a
// machine somebody is setting up, and the event arrives all the same.
func TestTheDaemonTellsAnAttachedClientTheConfigChanged(t *testing.T) {
	dir, err := os.MkdirTemp("", "nd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "st"))
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}

	stop, _ := startDaemon(t, Options{StateDir: dir, Version: "dev"})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := ipc.DialAt(ctx, ipc.SocketPath(dir))
	if err != nil {
		t.Fatalf("dialing the daemon: %v", err)
	}
	client, err := ipc.Attach(ctx, conn, ipc.Options{Client: "norite-test", Version: "dev", Events: true})
	if err != nil {
		t.Fatalf("attaching: %v", err)
	}
	defer func() { _ = client.Close() }()

	expect := func(what string) {
		t.Helper()
		select {
		case ev, open := <-client.Events():
			if !open {
				t.Fatalf("%s: the connection closed: %v", what, client.Err())
			}
			if ev.Type != ipc.EventConfigUpdate || string(ev.Data) != `{}` {
				t.Fatalf("%s: got %s %s", what, ev.Type, ev.Data)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the client was never told", what)
		}
	}

	// The watch is set up after the socket is served, so the first write is retried until it is seen.
	deadline := time.Now().Add(5 * time.Second)
	for seen := false; !seen; {
		if err := config.Set(path, config.Shared, config.KeyClock, "12h"); err != nil {
			t.Fatal(err)
		}
		select {
		case ev := <-client.Events():
			seen = ev.Type == ipc.EventConfigUpdate
		case <-time.After(300 * time.Millisecond):
			if err := config.Unset(path, config.Shared, config.KeyClock); err != nil {
				t.Fatal(err)
			}
			if time.Now().After(deadline) {
				t.Fatal("`norite config set` was never noticed")
			}
		}
	}
	// Let anything from the retries settle before the edit that is asserted on.
	time.Sleep(400 * time.Millisecond)
	for len(client.Events()) > 0 {
		<-client.Events()
	}

	// A person saving in an editor, by rename, with the daemon running.
	tmp := path + ".swp"
	if err := os.WriteFile(tmp, []byte("# by hand\n[shared]\nclock = \"24h\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	expect("a save from an editor")

	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}
