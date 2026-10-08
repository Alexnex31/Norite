// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package daemonproc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Alexnex31/Norite/daemon/config"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/statefile"
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
		if _, err := config.SetFor(config.TUI, config.Shared, config.KeyClock, "12h"); err != nil {
			t.Fatal(err)
		}
		select {
		case ev := <-client.Events():
			seen = ev.Type == ipc.EventConfigUpdate
		case <-time.After(300 * time.Millisecond):
			if _, err := config.UnsetFor(config.TUI, config.Shared, config.KeyClock); err != nil {
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

// The toggle, through a real daemon: a client asks for the split over the socket, with nobody signed in,
// and the daemon copies the config, records the toggle where any client can read it, and tells the client
// attached for events to read its config again. Then back.
func TestTheDaemonSplitsAndUnsplitsTheConfigOnRequest(t *testing.T) {
	dir, err := os.MkdirTemp("", "nd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "st"))
	cfgDir, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	files := config.FilesIn(cfgDir)
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const shared = "# mine\n[shared]\nclock = \"12h\"\n"
	if err := os.WriteFile(files.Shared, []byte(shared), 0o600); err != nil {
		t.Fatal(err)
	}

	startDaemon(t, Options{StateDir: dir, Version: "dev"})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
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
	if client.Ready().Account != nil {
		t.Fatal("the test means to run signed out")
	}

	toggle := func(path string) ipc.ConfigToggle {
		t.Helper()
		res, err := client.Do(ctx, "POST", path, nil)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		var out ipc.ConfigToggle
		if res.Status != 200 || json.Unmarshal(res.Body, &out) != nil {
			t.Fatalf("%s answered %d %s", path, res.Status, res.Body)
		}
		select {
		case ev := <-client.Events():
			if ev.Type != ipc.EventConfigUpdate {
				t.Fatalf("%s: got %s", path, ev.Type)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the attached client was never told", path)
		}
		return out
	}
	readFile := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	if out := toggle(ipc.PathConfigSplit); !out.Split {
		t.Fatalf("split answered %+v", out)
	}
	if got := readFile(files.TUI); got != shared {
		t.Errorf("config.tui.toml is %q", got)
	}
	state, err := statefile.ReadIn(dir)
	if err != nil || !state.ConfigSplit {
		t.Fatalf("the state file says %+v, %v", state, err)
	}

	// The terminal client's own setting, made while split, is in config.toml after.
	if err := os.WriteFile(files.TUI, []byte(shared+"\n[tui.colors]\naccent = 208\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := toggle(ipc.PathConfigUnsplit); out.Split {
		t.Fatalf("unsplit answered %+v", out)
	}
	if got := readFile(files.Shared); got != shared+"\n[tui.colors]\naccent = 208\n" {
		t.Errorf("config.toml is %q", got)
	}
	if state, err := statefile.ReadIn(dir); err != nil || state.ConfigSplit {
		t.Fatalf("the state file says %+v, %v", state, err)
	}

	// Asked again it is refused, with the daemon's own error and no status: nothing went to an instance.
	_, err = client.Do(ctx, "POST", ipc.PathConfigUnsplit, nil)
	var re *ipc.RelayError
	if !errors.As(err, &re) || re.Code != ipc.RelayConflict {
		t.Fatalf("a second unsplit: %v", err)
	}
}

// The requests about the port for scripts reach a real daemon through its attach socket (M22): asked how
// the port stands, it answers; asked to turn it on with nobody signed in, it refuses and says why. That
// they are routed at all is what this is for; what they do is tested on the controller.
func TestARunningDaemonAnswersForItsAutomationPort(t *testing.T) {
	dir, err := os.MkdirTemp("", "nd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// What a killed daemon left behind, which this one must not leave standing.
	if err := os.WriteFile(ipc.AutomationFilePath(dir), []byte(`{"address":"127.0.0.1:7717","secret":"stale"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	startDaemon(t, Options{StateDir: dir, Version: "dev"})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
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

	if _, err := os.Stat(ipc.AutomationFilePath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stale automation file is still there: %v", err)
	}

	res, err := client.Do(ctx, "GET", ipc.PathAutomation, nil)
	if err != nil {
		t.Fatalf("asking how the port stands: %v", err)
	}
	var st ipc.AutomationStatus
	if res.Status != 200 || json.Unmarshal(res.Body, &st) != nil {
		t.Fatalf("answered %d %s", res.Status, res.Body)
	}
	if st.Enabled || st.Open || st.Port != ipc.DefaultAutomationPort {
		t.Fatalf("a daemon nobody asked reports %+v", st)
	}

	_, err = client.Do(ctx, "POST", ipc.PathAutomationEnable(ipc.DefaultAutomationPort), nil)
	var re *ipc.RelayError
	if !errors.As(err, &re) || re.Code != ipc.RelayConflict {
		t.Fatalf("turning the port on with nobody signed in: %v", err)
	}
}
