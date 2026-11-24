// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/guilds"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// M36's done-when, as the processes run it: a real daemon, its real worker and the real CLI, built from this
// checkout, against the real instance with its voice coordinator and an SFU on loopback. Killing the daemon
// mid-call is followed, once it is started again, by the call joined again within a few seconds, and a
// caller who had muted themselves comes back muted, never heard in between.
//
// The binaries are built by path, so this module imports nothing of theirs (the edges are daemon → backend
// and cli → both). Everything they touch is the test's: their state, config and runtime directories and
// home are temporary, the session bus they are given does not exist, so the credential store falls back to
// its file and no keyring is touched, and the worker is built with norite_noaudio and opens no device.
// Linux alone: it is the one OS whose daemon this can start without a session of its own.
func TestAKilledDaemonJoinsItsCallAgainMuted(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the client, starts a real daemon and makes real calls")
	}
	if runtime.GOOS != "linux" {
		t.Skip("starts a daemon outside any service manager, which only Linux runs without a session")
	}
	f := newVoiceFixture(t, nil)
	instance := serveInstance(t, f)
	bins := buildClient(t)
	c := newClientHome(t, bins, instance)

	c.norite(t, "login", "--instance", instance, "--email", "owner@example.com", "--device-name", "rejoin-test")
	daemon := c.startDaemon(t)
	c.waitStatus(t, "the daemon to answer about voice", func(st voiceStatus) bool { return st.Available })

	watcher := f.connect(t, f.memberToken)
	c.norite(t, "voice", "mute")
	c.norite(t, "voice", "join", f.channel)
	st := c.status(t)
	require.Equal(t, "in_call", st.State, "%+v", st)
	first := watcher.untilState(t, func(s map[string]any) bool { return s["user_id"] == f.ownerID })
	require.Equal(t, true, first["self_mute"], "the caller joined heard")
	c.breadcrumbHoldsNoSecret(t)

	// Killed where it stands: nothing it runs gets to say goodbye, and the worker goes with its pipe.
	daemon.kill(t)
	restarted := time.Now()
	c.startDaemon(t)
	st = c.waitStatus(t, "the call to be joined again", func(st voiceStatus) bool {
		return st.State == "in_call" && st.Rejoin != nil
	})
	took := time.Since(restarted)
	t.Logf("joined again %s after the daemon started again", took.Round(10*time.Millisecond))
	require.Less(t, took, 15*time.Second, "not within a few seconds of the restart")
	require.Equal(t, f.channel, st.Channel)
	require.Equal(t, "daemon_restarted", st.Rejoin.Cause)
	require.True(t, st.Self.Mute, "the daemon came back unmuted")

	// The channel saw the new connection muted, and nothing of this account unmuted in between.
	for {
		_, raw := watcher.until(gatewayproto.EventVoiceStateUpdate)
		var s map[string]any
		require.NoError(t, json.Unmarshal(raw, &s))
		if s["user_id"] != f.ownerID {
			continue
		}
		require.Equal(t, true, s["self_mute"], "the channel heard the caller unmuted: %v", s)
		if s["connection_id"] != first["connection_id"] {
			break
		}
	}
	states := f.voiceStates(t, f.memberToken, f.channel)
	require.Len(t, states, 1, "the killed daemon's call is still listed beside the new one")
	require.Equal(t, f.ownerID, states[0]["user_id"])
	require.Equal(t, true, states[0]["self_mute"])
}

// A daemon stopped by a signal, as its service manager or a restart stops it, keeps the call too; one whose
// person left it does not join it again, killed or not.
func TestADaemonJoinsAgainAfterAStopAndNotAfterALeave(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the client, starts a real daemon and makes real calls")
	}
	if runtime.GOOS != "linux" {
		t.Skip("starts a daemon outside any service manager, which only Linux runs without a session")
	}
	f := newVoiceFixture(t, nil)
	instance := serveInstance(t, f)
	bins := buildClient(t)
	c := newClientHome(t, bins, instance)
	c.norite(t, "login", "--instance", instance, "--email", "owner@example.com", "--device-name", "rejoin-test")

	daemon := c.startDaemon(t)
	c.waitStatus(t, "the daemon to answer about voice", func(st voiceStatus) bool { return st.Available })
	c.norite(t, "voice", "join", f.channel)
	daemon.stop(t)
	daemon = c.startDaemon(t)
	c.waitStatus(t, "the call to be joined again", func(st voiceStatus) bool {
		return st.State == "in_call" && st.Rejoin != nil
	})

	c.norite(t, "voice", "leave")
	daemon.kill(t)
	c.startDaemon(t)
	c.waitStatus(t, "the daemon to answer about voice", func(st voiceStatus) bool { return st.Available })
	time.Sleep(5 * time.Second)
	st := c.status(t)
	require.Equal(t, "idle", st.State, "a call the person left was joined again: %+v", st)
	require.Nil(t, st.Rejoin)
	require.Empty(t, f.voiceStates(t, f.memberToken, f.channel))
}

// A rejoin is an ordinary join, authorized afresh (rule 1): a right taken away while the daemon was down
// refuses it, the daemon gives up at once rather than trying again, and says why.
func TestARejoinIsRefusedARightLostWhileTheDaemonWasDown(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the client, starts a real daemon and makes real calls")
	}
	if runtime.GOOS != "linux" {
		t.Skip("starts a daemon outside any service manager, which only Linux runs without a session")
	}
	f := newVoiceFixture(t, nil)
	instance := serveInstance(t, f)
	bins := buildClient(t)
	c := newClientHome(t, bins, instance)
	c.norite(t, "login", "--instance", instance, "--email", "member@example.com", "--device-name", "rejoin-test")

	daemon := c.startDaemon(t)
	c.waitStatus(t, "the daemon to answer about voice", func(st voiceStatus) bool { return st.Available })
	c.norite(t, "voice", "join", f.channel)
	before := f.voiceStates(t, f.ownerToken, f.channel)
	require.Len(t, before, 1)
	daemon.kill(t)

	res := f.api.call(http.MethodPut, fmt.Sprintf("/api/v1/channels/%s/permissions/%s", f.channel, f.everyoneID),
		map[string]any{"type": 0, "allow": "0", "deny": fmt.Sprint(roles.PermConnectVoice.Int64())},
		withToken(f.ownerToken))
	require.Equal(t, http.StatusOK, res.Code, res)

	c.startDaemon(t)
	st := c.waitStatus(t, "the rejoin to be given up", func(st voiceStatus) bool {
		return st.State == "idle" && st.Rejoin != nil
	})
	require.Equal(t, "this account may not join that voice channel", st.Reason)
	require.Equal(t, 1, st.Rejoin.Attempt, "a refusal for good was tried again")
	// No connection was made: what is listed, if anything, is the killed daemon's, until the instance lets
	// it go (M33).
	for _, s := range f.voiceStates(t, f.ownerToken, f.channel) {
		require.Equal(t, before[0]["connection_id"], s["connection_id"], "a refused rejoin is in the call")
	}
}

// serveInstance serves the whole router over HTTP, its gateway the fixture's, whose op 4 reaches the voice
// coordinator: what a daemon signs in to and calls through.
func serveInstance(t *testing.T, f *voiceFixture) string {
	t.Helper()
	health := newHealth(db.New(f.api.pool))
	health.MarkReady()
	handler, err := newRouter(routerOptions{
		Config: testConfig(), Logger: zerolog.New(io.Discard), Health: health,
		Auth: auth.NewHandler(f.api.authSvc), AuthSvc: f.api.authSvc,
		Guilds: guilds.NewHandler(f.api.guildsSvc, f.api.authSvc), Gateway: f.gw,
	})
	require.NoError(t, err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

// clientBins is the three programs a release ships, built from this checkout, side by side as installed.
type clientBins struct{ norite, daemon string }

func buildClient(t *testing.T) clientBins {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join("..", "..", "..")
	build := func(module, pkg, name string, flags ...string) string {
		out := filepath.Join(dir, name)
		cmd := exec.Command("go", append(append([]string{"build"}, flags...), "-o", out, pkg)...) //nolint:gosec // fixed arguments
		cmd.Dir = filepath.Join(root, module)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building %s: %v\n%s", pkg, err, b)
		}
		return out
	}
	// The worker beside the daemon, where the daemon looks for it.
	build("voiceworker", "./cmd/norite-voice", "norite-voice", "-tags", "norite_noaudio")
	return clientBins{norite: build("cli", "./cmd/app", "norite"), daemon: build("daemon", "./cmd/daemond", "norite-daemon")}
}

// clientHome is one machine's account, as far as the client can tell: every directory it reads or writes.
type clientHome struct {
	bins     clientBins
	instance string
	env      []string
}

func newClientHome(t *testing.T, bins clientBins, instance string) *clientHome {
	t.Helper()
	// Short, since the attach socket is a path in the state directory and a Unix socket's path is bounded.
	home, err := os.MkdirTemp("", "nrj")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	env := []string{
		"HOME=" + home,
		"XDG_STATE_HOME=" + filepath.Join(home, "s"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, "c"),
		"XDG_RUNTIME_DIR=" + filepath.Join(home, "r"),
		"XDG_CACHE_HOME=" + filepath.Join(home, "k"),
		"XDG_DATA_HOME=" + filepath.Join(home, "d"),
		// No session bus: the keyring's probe fails and the credential goes to the state directory's file.
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + filepath.Join(home, "no-bus"),
		"PATH=" + os.Getenv("PATH"),
		"NORITE_PASSWORD=" + testPassword,
	}
	require.NoError(t, os.Mkdir(filepath.Join(home, "r"), 0o700))
	return &clientHome{bins: bins, instance: instance, env: env}
}

// norite runs the CLI and returns its standard output, failing the test on a non-zero exit.
func (c *clientHome) norite(t *testing.T, args ...string) string {
	t.Helper()
	out, err := c.try(args...)
	require.NoError(t, err, "norite %s: %s", strings.Join(args, " "), out)
	return out
}

func (c *clientHome) try(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.bins.norite, args...) //nolint:gosec // the CLI just built
	cmd.Env = c.env
	cmd.Stdin = nil
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	return out.String() + errOut.String(), err
}

// breadcrumbHoldsNoSecret reads state.json while a call is up: it names the call, and holds nothing of the
// sign-in's secret, which is in the state directory beside it, nor of what the call was given to connect
// with (rule 8).
func (c *clientHome) breadcrumbHoldsNoSecret(t *testing.T) {
	t.Helper()
	dir := filepath.Join(strings.TrimPrefix(c.env[1], "XDG_STATE_HOME="), "norite")
	var state []byte
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(filepath.Join(dir, "state.json"))
		state = data
		return err == nil && bytes.Contains(data, []byte(`"voice_call"`))
	}, 10*time.Second, 50*time.Millisecond, "the call is not in state.json")
	opaque := regexp.MustCompile(`[A-Za-z0-9_\-]{24,}`)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	secrets := 0
	for _, e := range entries {
		if !e.Type().IsRegular() || e.Name() == "state.json" || strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err)
		for _, s := range opaque.FindAll(data, -1) {
			secrets++
			require.NotContains(t, string(state), string(s), "state.json holds a value from %s", e.Name())
		}
	}
	require.NotZero(t, secrets, "the sign-in's secret was not found to compare: the test proves nothing")
	for _, s := range []string{"turn", "v=0", "credential", "token"} {
		require.NotContains(t, strings.ToLower(string(state)), s)
	}
}

// voiceStatus is what `norite voice status --json` prints, as far as these tests read it.
type voiceStatus struct {
	State     string `json:"state"`
	Channel   string `json:"channel"`
	Reason    string `json:"reason"`
	Available bool   `json:"available"`
	Self      struct {
		Mute bool `json:"mute"`
	} `json:"self"`
	Rejoin *struct {
		Cause   string `json:"cause"`
		Attempt int    `json:"attempt"`
	} `json:"rejoin"`
}

func (c *clientHome) status(t *testing.T) voiceStatus {
	t.Helper()
	st, err := c.tryStatus()
	require.NoError(t, err)
	return st
}

func (c *clientHome) tryStatus() (voiceStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.bins.norite, "--json", "voice", "status") //nolint:gosec // the CLI just built
	cmd.Env = c.env
	out, err := cmd.Output()
	if err != nil {
		return voiceStatus{}, err
	}
	var st voiceStatus
	return st, json.Unmarshal(out, &st)
}

// waitStatus polls the status until ok, for at most 30 s.
func (c *clientHome) waitStatus(t *testing.T, what string, ok func(voiceStatus) bool) voiceStatus {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last voiceStatus
	var lastErr error
	for time.Now().Before(deadline) {
		last, lastErr = c.tryStatus()
		if lastErr == nil && ok(last) {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("waited for %s: last status %+v, %v", what, last, lastErr)
	return last
}

// daemonProc is a running daemon, started by hand.
type daemonProc struct {
	cmd  *exec.Cmd
	done chan struct{}
	log  *bytes.Buffer
}

func (c *clientHome) startDaemon(t *testing.T) *daemonProc {
	t.Helper()
	log := &bytes.Buffer{}
	cmd := exec.Command(c.bins.daemon) //nolint:gosec // the daemon just built
	cmd.Env = c.env
	cmd.Stdout, cmd.Stderr = log, log
	// Its own process group, so a test that fails leaves nothing behind.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	d := &daemonProc{cmd: cmd, done: make(chan struct{}), log: log}
	go func() {
		_ = cmd.Wait()
		close(d.done)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-d.done
		if t.Failed() {
			t.Logf("daemon %d's log:\n%s", cmd.Process.Pid, log.String())
		}
	})
	return d
}

// kill ends the daemon alone with SIGKILL, as a crash would, and waits for it to be gone. Its worker is
// left to notice its pipe closing and exit by itself (M28).
func (d *daemonProc) kill(t *testing.T) {
	t.Helper()
	require.NoError(t, d.cmd.Process.Kill())
	d.wait(t)
}

// stop ends the daemon as its service manager does, with SIGTERM.
func (d *daemonProc) stop(t *testing.T) {
	t.Helper()
	require.NoError(t, d.cmd.Process.Signal(syscall.SIGTERM))
	d.wait(t)
}

func (d *daemonProc) wait(t *testing.T) {
	t.Helper()
	select {
	case <-d.done:
	case <-time.After(20 * time.Second):
		t.Fatalf("the daemon did not exit:\n%s", d.log.String())
	}
}
