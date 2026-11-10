// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voicecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/cli/internal/daemontest"
	"github.com/Alexnex31/Norite/daemon/config"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// fakeDaemon answers the voice requests and remembers what it was asked.
type fakeDaemon struct {
	status ipc.VoiceStatus
	refuse *ipc.RelayError
	older  bool
	asked  []string
	// bodies is what each request carried, encoded as the attach socket would, "null" for none.
	bodies []string
}

func (f *fakeDaemon) Do(_ context.Context, method, path string, body any) (ipc.Result, error) {
	f.asked = append(f.asked, method+" "+path)
	encoded, _ := json.Marshal(body)
	f.bodies = append(f.bodies, string(encoded))
	if f.older {
		return ipc.Result{Status: 404, Body: json.RawMessage(`{"error":{"code":"not_found","message":"x","request_id":"r"}}`)}, nil
	}
	if f.refuse != nil && method == "POST" {
		return ipc.Result{}, f.refuse
	}
	if ch, ok := ipc.VoiceJoinChannel(path); ok {
		f.status.State, f.status.Channel = ipc.VoiceInCall, ch
	}
	if path == ipc.PathVoiceLeave {
		f.status = ipc.VoiceStatus{State: ipc.VoiceIdle, Reason: "left", Available: f.status.Available}
	}
	answer, _ := json.Marshal(f.status)
	return ipc.Result{Status: 200, Body: answer}, nil
}

type ran struct {
	out string
	err error
}

func norite(t *testing.T, daemon *fakeDaemon, asJSON bool, args ...string) ran {
	t.Helper()
	return noriteWith(t, daemon, allOn, asJSON, args...)
}

// allOn is a config.toml that says nothing about voice.
func allOn() (ipc.VoiceJoin, []string, error) {
	return ipc.VoiceJoin{EchoCancellation: true, NoiseSuppression: true, AutoGain: true}, nil, nil
}

func noriteWith(t *testing.T, daemon *fakeDaemon, settings Settings, asJSON bool, args ...string) ran {
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
		Commands:       []*cli.Command{Command(connect, settings)},
	}
	argv := []string{"norite"}
	if asJSON {
		argv = append(argv, "--json")
	}
	err := root.Run(context.Background(), append(append(argv, "voice"), args...))
	return ran{out: out.String(), err: err}
}

func available() ipc.VoiceStatus { return ipc.VoiceStatus{State: ipc.VoiceIdle, Available: true} }

func TestEachCommandAsksItsOwnRequestAndPrintsTheContract(t *testing.T) {
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"status"}, []string{"GET " + ipc.PathVoice}},
		// The join asks how voice stands first, and sends the channel only to a daemon that answered.
		{[]string{"join", "1234"}, []string{"GET " + ipc.PathVoice, "POST " + ipc.PathVoiceJoin("1234")}},
		{[]string{"leave"}, []string{"POST " + ipc.PathVoiceLeave}},
	}
	for _, tc := range cases {
		d := &fakeDaemon{status: available()}
		r := norite(t, d, true, tc.args...)
		require.NoError(t, r.err, tc.args)
		assert.Equal(t, tc.want, d.asked)
		daemontest.MatchesCLISchema(t, r.out, "voice.schema.json", "status")
	}
	// With a call, the settings it runs with are in the output, as the contract says.
	d := &fakeDaemon{status: ipc.VoiceStatus{State: ipc.VoiceInCall, Channel: "42", Available: true,
		Processing: &ipc.VoiceJoin{NoiseSuppression: true}}}
	r := norite(t, d, true, "status")
	require.NoError(t, r.err)
	daemontest.MatchesCLISchema(t, r.out, "voice.schema.json", "status")
	var got struct{ Processing *ipc.VoiceJoin }
	require.NoError(t, json.Unmarshal([]byte(r.out), &got))
	assert.Equal(t, &ipc.VoiceJoin{NoiseSuppression: true}, got.Processing)
}

// A daemon older than these requests is asked the request that carries nothing, and never the channel.
func TestAnOlderDaemonIsNeverSentTheChannel(t *testing.T) {
	d := &fakeDaemon{older: true}
	r := norite(t, d, false, "join", "1234")
	require.Error(t, r.err)
	assert.Equal(t, []string{"GET " + ipc.PathVoice}, d.asked)
	assert.Contains(t, r.err.Error(), "older")
}

// A daemon that cannot join says why, and is not sent the channel either.
func TestAJoinThisDaemonCannotMakeIsRefusedBeforeTheChannelIsSent(t *testing.T) {
	d := &fakeDaemon{status: ipc.VoiceStatus{State: ipc.VoiceIdle,
		Unavailable: "voice calls arrive in a later version of Norite"}}
	r := norite(t, d, false, "join", "1234")
	var refused *clierr.RefusedError
	require.True(t, errors.As(r.err, &refused), "%v", r.err)
	assert.Contains(t, refused.Message, "arrive in a later version")
	assert.Equal(t, []string{"GET " + ipc.PathVoice}, d.asked)
}

// The settings travel in the join's body, which goes only where the channel does: never with the GET that
// finds out whether the daemon knows these requests, and never to a daemon that did not answer it (the
// ledger's entry on the prefix, reopened at M30).
func TestTheSettingsAreSentOnlyWithTheChannel(t *testing.T) {
	off := func() (ipc.VoiceJoin, []string, error) {
		return ipc.VoiceJoin{EchoCancellation: true, NoiseSuppression: false, AutoGain: false}, nil, nil
	}
	d := &fakeDaemon{status: available()}
	r := noriteWith(t, d, off, false, "join", "1234")
	require.NoError(t, r.err)
	assert.Equal(t, []string{"null", `{"echo_cancellation":true,"noise_suppression":false,"auto_gain":false}`},
		d.bodies)

	older := &fakeDaemon{older: true}
	require.Error(t, noriteWith(t, older, off, false, "join", "1234").err)
	assert.Equal(t, []string{"null"}, older.bodies)
	unable := &fakeDaemon{status: ipc.VoiceStatus{State: ipc.VoiceIdle, Unavailable: "not in this build"}}
	require.Error(t, noriteWith(t, unable, off, false, "join", "1234").err)
	assert.Equal(t, []string{"null"}, unable.bodies)
}

// A config that does not load is not the defaults, and nothing is asked: somebody who turned a switch off
// and then broke the file would otherwise be put in a call with it on.
func TestAConfigThatDoesNotLoadJoinsNothing(t *testing.T) {
	const why = "the voice settings could not be read: config.toml: line 3"
	broken := func() (ipc.VoiceJoin, []string, error) { return ipc.VoiceJoin{}, nil, errors.New(why) }
	d := &fakeDaemon{status: available()}
	r := noriteWith(t, d, broken, false, "join", "1234")
	require.Error(t, r.err)
	assert.Equal(t, why, r.err.Error())
	assert.Empty(t, d.asked)

	// And it is said as itself with no daemon running: reading the file is a local check, made before
	// anything is attached to.
	r = noriteWith(t, nil, broken, false, "join", "1234")
	require.Error(t, r.err)
	assert.Equal(t, why, r.err.Error())
}

// What the file got wrong under voice is said, a misspelled switch and a voice that is not a table
// included, and the join goes ahead with every switch the file did not set right on. The GUI's section
// applies to nothing here, and neither does a color.
func TestWhatTheFileGotWrongAboutVoiceIsSaid(t *testing.T) {
	cfg, err := config.Parse([]byte("[shared.voice]\nauto_gain = \"of\"\necho_cancelation = \"off\"\n"+
		"[tui]\nvoice = false\n[tui.colors]\naccent = \"red\"\n[gui.voice]\nauto_gain = \"no\"\n"), config.TUI)
	require.NoError(t, err)
	body, warnings, err := settingsOf(cfg, "config.toml")
	require.NoError(t, err)
	assert.Equal(t, ipc.VoiceJoin{EchoCancellation: true, NoiseSuppression: true, AutoGain: true}, body)
	assert.Equal(t, []string{
		`config.toml: shared.voice.auto_gain: must be one of "on" or "off"; so automatic gain is on`,
		"config.toml: shared.voice.echo_cancelation: not a key this version of Norite knows; it changes nothing",
		"config.toml: tui.voice: not a key this version of Norite knows; it changes nothing",
	}, warnings)

	d := &fakeDaemon{status: available()}
	r := noriteWith(t, d, func() (ipc.VoiceJoin, []string, error) { return body, warnings, nil }, false,
		"join", "1234")
	require.NoError(t, r.err)
	for _, w := range warnings {
		assert.Contains(t, r.out, w+"\n")
	}
}

// A bad value in one section says what applies, which the other section may have set: off, here.
func TestABadSwitchSaysWhatApplies(t *testing.T) {
	cfg, err := config.Parse([]byte("[shared.voice]\necho_cancellation = \"off\"\n"+
		"[tui.voice]\necho_cancellation = \"of\"\n"), config.TUI)
	require.NoError(t, err)
	body, warnings, err := settingsOf(cfg, "config.toml")
	require.NoError(t, err)
	assert.False(t, body.EchoCancellation)
	assert.Equal(t, []string{`config.toml: tui.voice.echo_cancellation: must be one of "on" or "off"; ` +
		"so echo cancellation is off"}, warnings)
}

// Problems past the bound the config keeps are counted, and one may be a switch's, so the join says so.
func TestProblemsPastTheBoundAreSaidToMayConcernVoice(t *testing.T) {
	var doc strings.Builder
	doc.WriteString("[shared]\n")
	for i := range config.MaxWarnings + 2 {
		fmt.Fprintf(&doc, "nonsense%d = 1\n", i)
	}
	doc.WriteString("[shared.voice]\nauto_gain = \"of\"\n")
	cfg, err := config.Parse([]byte(doc.String()), config.TUI)
	require.NoError(t, err)
	_, warnings, err := settingsOf(cfg, "config.toml")
	require.NoError(t, err)
	assert.Equal(t, []string{"config.toml: 3 more problems, not listed, may concern the voice settings; " +
		"`norite config get` says what applies"}, warnings)
}

func TestAFailedJoinSaysWhy(t *testing.T) {
	d := &fakeDaemon{status: available(),
		refuse: &ipc.RelayError{Code: ipc.RelayFailed, Message: "the voice server refused: 409 room full"}}
	r := norite(t, d, false, "join", "1234")
	require.Error(t, r.err)
	assert.Contains(t, r.err.Error(), "room full")
}

// What is not an id is a usage error, before anything is attached to.
func TestArgumentsAreCheckedFirst(t *testing.T) {
	for _, args := range [][]string{{"join"}, {"join", "general"}, {"join", "12", "34"}, {"join", "-1"},
		{"join", "123456789012345678901"}, {"leave", "now"}, {"status", "x"}} {
		r := norite(t, nil, false, args...)
		var usage *clierr.UsageError
		require.True(t, errors.As(r.err, &usage), "%v: %v", args, r.err)
	}
}

func TestTheTextSaysHowTheCallStands(t *testing.T) {
	cases := []struct {
		st   ipc.VoiceStatus
		want []string
		not  []string
	}{
		{ipc.VoiceStatus{State: ipc.VoiceInCall, Channel: "42", Available: true}, []string{"In a voice call in channel 42."}, nil},
		{ipc.VoiceStatus{State: ipc.VoiceJoining, Channel: "42", Available: true}, []string{"Joining voice channel 42."}, nil},
		{ipc.VoiceStatus{State: ipc.VoiceInCall, Channel: "42", Available: true,
			Processing: &ipc.VoiceJoin{EchoCancellation: true, AutoGain: true}},
			[]string{"In a voice call in channel 42.\nJoined with echo cancellation on, noise suppression off, automatic gain on.\n"}, nil},
		{ipc.VoiceStatus{State: ipc.VoiceInCall, Channel: "42", Available: true}, nil, []string{"Joined with"}},
		{ipc.VoiceStatus{State: ipc.VoiceIdle, Reason: "left", Available: true}, []string{"Not in a voice call."}, []string{"ended"}},
		{ipc.VoiceStatus{State: ipc.VoiceIdle, Reason: "the voice server closed the call\x1b[2J", Available: true},
			[]string{"The last call ended: the voice server closed the call"}, []string{"\x1b"}},
		{ipc.VoiceStatus{State: ipc.VoiceIdle, Unavailable: "not yet"}, []string{"cannot join voice calls: not yet."}, nil},
	}
	for _, tc := range cases {
		r := norite(t, &fakeDaemon{status: tc.st}, false, "status")
		require.NoError(t, r.err)
		for _, w := range tc.want {
			assert.Contains(t, r.out, w)
		}
		for _, n := range tc.not {
			assert.False(t, strings.Contains(r.out, n), "%q in %q", n, r.out)
		}
	}
}
