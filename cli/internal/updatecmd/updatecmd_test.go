// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package updatecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/cli/internal/daemontest"
	"github.com/Alexnex31/Norite/daemon/installreceipt"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// TestMain makes the real receipt a panic: a test that forgot to replace it would write into, or remove
// from, the state directory of whoever runs the suite, and with it decide whether their daemon updates.
func TestMain(m *testing.M) {
	recordReceipt = func(installreceipt.Receipt) error { panic("a test reached the real install receipt") }
	forgetReceipt = func() error { panic("a test reached the real install receipt") }
	zone = time.UTC
	os.Exit(m.Run())
}

// fakeDaemon answers the update requests with one status, and remembers what it was asked.
type fakeDaemon struct {
	status ipc.UpdateStatus
	refuse *ipc.RelayError
	older  bool
	asked  []string
}

func (f *fakeDaemon) Do(_ context.Context, method, path string, _ any) (ipc.Result, error) {
	f.asked = append(f.asked, method+" "+path)
	if f.older {
		return ipc.Result{Status: 404, Body: json.RawMessage(`{"error":{"code":"not_found","message":"x","request_id":"r"}}`)}, nil
	}
	if f.refuse != nil && method == "POST" {
		return ipc.Result{}, f.refuse
	}
	if word, ok := strings.CutPrefix(path, ipc.PathUpdateMode); ok {
		f.status.Mode = word
	}
	if word, ok := strings.CutPrefix(path, ipc.PathUpdateChannel); ok {
		f.status.Channel = word
	}
	body, _ := json.Marshal(f.status)
	return ipc.Result{Status: 200, Body: body}, nil
}

type ran struct {
	out string
	err error
}

func norite(t *testing.T, daemon *fakeDaemon, asJSON bool, args ...string) ran {
	t.Helper()
	var connect Connector
	if daemon != nil {
		connect = func(context.Context) (daemonclient.Caller, func(), error) { return daemon, func() {}, nil }
	}
	var out bytes.Buffer
	root := &cli.Command{
		Name: "norite", Writer: &out, ErrWriter: &out,
		Flags:          []cli.Flag{&cli.BoolFlag{Name: "json"}},
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
		Commands:       []*cli.Command{Command(connect)},
	}
	argv := []string{"norite"}
	if asJSON {
		argv = append(argv, "--json")
	}
	err := root.Run(context.Background(), append(append(argv, "update"), args...))
	return ran{out: out.String(), err: err}
}

func installed() ipc.UpdateStatus {
	return ipc.UpdateStatus{Version: "0.1.0", Mode: ipc.UpdateAuto, Channel: ipc.ChannelStable, Installs: true}
}

func TestEachCommandAsksItsOwnRequestAndPrintsTheContract(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"status"}, "GET " + ipc.PathUpdate},
		{[]string{"check"}, "POST " + ipc.PathUpdateCheck},
		{[]string{"apply"}, "POST " + ipc.PathUpdateApply},
		{[]string{"mode", "notify"}, "POST " + ipc.PathUpdateMode + "notify"},
		{[]string{"mode", "off"}, "POST " + ipc.PathUpdateMode + "off"},
		{[]string{"channel", "prerelease"}, "POST " + ipc.PathUpdateChannel + "prerelease"},
	}
	for _, tc := range cases {
		d := &fakeDaemon{status: installed()}
		d.status.Available, d.status.LastCheck = "0.1.1", "2026-10-10T09:30:00Z"
		r := norite(t, d, true, tc.args...)
		require.NoError(t, r.err, tc.args)
		assert.Equal(t, []string{tc.want}, d.asked)
		daemontest.MatchesCLISchema(t, r.out, "update.schema.json", "status")
		require.NoError(t, norite(t, d, false, tc.args...).err)
	}
}

func TestWhatAPersonIsTold(t *testing.T) {
	cases := []struct {
		name   string
		change func(*ipc.UpdateStatus)
		args   []string
		says   []string
		not    []string
	}{
		{name: "nothing checked yet", says: []string{"Norite 0.1.0 is running.", "No check", "installed by themselves (releases only)"}},
		{name: "nothing newer", change: func(s *ipc.UpdateStatus) { s.LastCheck = "2026-10-10T09:30:00Z" },
			says: []string{"No newer release, as of 2026-10-10 09:30."}},
		{name: "one is out, auto", change: func(s *ipc.UpdateStatus) { s.Available = "0.1.1" },
			says: []string{"0.1.1 is out and will be installed by itself"}},
		{name: "one is out, notify", change: func(s *ipc.UpdateStatus) { s.Available, s.Mode = "0.1.1", ipc.UpdateNotify },
			says: []string{"0.1.1 is out. `norite update apply` installs it.", "installed when you ask"}},
		{name: "one is out, and this daemon installs nothing",
			change: func(s *ipc.UpdateStatus) {
				s.Available, s.Installs, s.Mode, s.Reason = "0.1.1", false, ipc.UpdateNotify, "this install was not made by Norite's installer"
			},
			says: []string{"0.1.1 is out.", "does not install releases: this install was not made by Norite's installer."},
			not:  []string{"norite update apply"}},
		{name: "off, pre-releases", change: func(s *ipc.UpdateStatus) { s.Mode, s.Channel = ipc.UpdateOff, ipc.ChannelPrerelease },
			says: []string{"Updates are off", "pre-releases too"}},
		{name: "on trial", change: func(s *ipc.UpdateStatus) { s.Version, s.Trial = "0.1.1", "0.1.1" },
			says: []string{"Norite 0.1.1 was just installed", "stayed up"}, not: []string{"is running."}},
		{name: "undone", change: func(s *ipc.UpdateStatus) { s.RolledBack = "0.1.1" },
			says: []string{"0.1.1 was installed, did not stay up, and was undone. It will not be installed again."}},
		{name: "a failure", change: func(s *ipc.UpdateStatus) { s.LastError = "the release's signature did not verify" },
			says: []string{"The last check or installation failed: the release's signature did not verify."}},
		{name: "applying", args: []string{"apply"}, change: func(s *ipc.UpdateStatus) { s.Available = "0.1.1" },
			says: []string{"Installing 0.1.1."}, not: []string{"is running", "Updates:"}},
	}
	for _, tc := range cases {
		d := &fakeDaemon{status: installed()}
		if tc.change != nil {
			tc.change(&d.status)
		}
		args := tc.args
		if args == nil {
			args = []string{"status"}
		}
		r := norite(t, d, false, args...)
		require.NoError(t, r.err, tc.name)
		for _, s := range tc.says {
			assert.Contains(t, r.out, s, tc.name)
		}
		for _, s := range tc.not {
			assert.NotContains(t, r.out, s, tc.name)
		}
	}
}

// Every string in a status is the daemon's, and parts of one began as a release host's or an instance's.
func TestNothingTheDaemonSaysReachesTheTerminalRaw(t *testing.T) {
	hostile := "\x1b[2J0.1.1\u202e"
	d := &fakeDaemon{status: ipc.UpdateStatus{Version: hostile, Mode: ipc.UpdateNotify, Channel: ipc.ChannelStable,
		Reason: hostile, Available: hostile, RolledBack: hostile, LastCheck: hostile, LastError: hostile}}
	for _, args := range [][]string{{"status"}, {"apply"}} {
		r := norite(t, d, false, args...)
		require.NoError(t, r.err)
		assert.NotContains(t, r.out, "\x1b")
		assert.NotContains(t, r.out, "\u202e")
	}
	// With nothing newer, the time of the last check is what is printed, and it is the daemon's text too.
	d.status.Trial, d.status.Available = hostile, ""
	r := norite(t, d, false, "status")
	assert.NotContains(t, r.out, "\x1b")
	// Under --json it is carried whole, escaped.
	js := norite(t, d, true, "status")
	require.NoError(t, js.err)
	assert.NotContains(t, js.out, "\x1b")
	var back ipc.UpdateStatus
	require.NoError(t, json.Unmarshal([]byte(js.out), &back))
	assert.Equal(t, hostile, back.Version)
}

func TestAWordThatIsNotOneIsRefusedBeforeAnythingIsSent(t *testing.T) {
	for _, args := range [][]string{
		{"mode"}, {"mode", "always"}, {"mode", "AUTO"}, {"mode", "off", "auto"}, {"mode", "../config/split"},
		{"channel"}, {"channel", "nightly"}, {"channel", "stable/../../stop"},
		{"status", "now"}, {"check", "x"}, {"apply", "0.1.1"},
	} {
		d := &fakeDaemon{status: installed()}
		r := norite(t, d, false, args...)
		var usage *clierr.UsageError
		require.ErrorAs(t, r.err, &usage, "%v", args)
		assert.Empty(t, d.asked, "%v reached the daemon", args)
	}
}

func TestARefusalIsTheDaemonsReason(t *testing.T) {
	d := &fakeDaemon{status: installed(), refuse: &ipc.RelayError{Code: ipc.RelayConflict, Message: "there is no newer release this install can move to"}}
	r := norite(t, d, false, "apply")
	require.Error(t, r.err)
	assert.Contains(t, r.err.Error(), "no newer release")

	// A daemon from before these requests relays the path; that is said as what it is.
	r = norite(t, &fakeDaemon{older: true}, false, "status")
	var unavailable *clierr.UnavailableError
	require.ErrorAs(t, r.err, &unavailable)
	assert.Contains(t, r.err.Error(), "older than this command")
}

func TestTheReceiptNamesADirectoryThatHoldsTheTwoPrograms(t *testing.T) {
	var written []installreceipt.Receipt
	var forgotten int
	recordReceipt = func(r installreceipt.Receipt) error { written = append(written, r); return nil }
	forgetReceipt = func() error { forgotten++; return nil }
	t.Cleanup(func() {
		recordReceipt = func(installreceipt.Receipt) error { panic("a test reached the real install receipt") }
		forgetReceipt = func() error { panic("a test reached the real install receipt") }
	})

	dir := t.TempDir()
	// Nothing there, then one of the two: not where Norite was installed.
	require.Error(t, norite(t, nil, false, "receipt", "--bin-dir", dir).err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "norite"), []byte("x"), 0o755)) //nolint:gosec // a stand-in program
	require.Error(t, norite(t, nil, false, "receipt", "--bin-dir", dir).err)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "norite-daemon"), 0o755)) //nolint:gosec // a directory where a program should be
	require.Error(t, norite(t, nil, false, "receipt", "--bin-dir", dir).err)
	require.Empty(t, written)

	require.NoError(t, os.Remove(filepath.Join(dir, "norite-daemon")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "norite-daemon"), []byte("x"), 0o755)) //nolint:gosec // a stand-in program
	r := norite(t, nil, false, "receipt", "--bin-dir", dir, "--by", "install.sh")
	require.NoError(t, r.err)
	assert.Empty(t, r.out, "the installer's command prints nothing")
	require.Equal(t, []installreceipt.Receipt{{BinDir: dir, By: "install.sh"}}, written)

	var usage *clierr.UsageError
	require.ErrorAs(t, norite(t, nil, false, "receipt").err, &usage)
	require.NoError(t, norite(t, nil, false, "receipt", "--forget").err)
	assert.Equal(t, 1, forgotten)
}
