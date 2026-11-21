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

// writes records what the commands wrote to config.toml, key=value, in order.
type writes struct{ got []string }

func (w *writes) write(key, value string) error {
	w.got = append(w.got, key+"="+value)
	return nil
}

// written is the current test's record; noriteWith resets nothing, so a test that reads it sets it first.
var written = &writes{}

type ran struct {
	out string
	err error
}

func norite(t *testing.T, daemon *fakeDaemon, asJSON bool, args ...string) ran {
	t.Helper()
	return noriteWith(t, daemon, allOn, asJSON, args...)
}

// allOn is a config.toml that says nothing about voice.
func allOn() (Read, []string, error) {
	return Read{Processing: ipc.VoiceJoin{EchoCancellation: true, NoiseSuppression: true, AutoGain: true}}, nil, nil
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
		Commands:       []*cli.Command{Command(connect, settings, written.write)},
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
	off := func() (Read, []string, error) {
		return Read{Processing: ipc.VoiceJoin{EchoCancellation: true, NoiseSuppression: false, AutoGain: false}},
			nil, nil
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
	broken := func() (Read, []string, error) { return Read{}, nil, errors.New(why) }
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
	assert.Equal(t, Read{Processing: ipc.VoiceJoin{EchoCancellation: true, NoiseSuppression: true, AutoGain: true}},
		body)
	assert.Equal(t, []string{
		`config.toml: shared.voice.auto_gain: must be one of "on" or "off"; so automatic gain is on`,
		"config.toml: shared.voice.echo_cancelation: not a key this version of Norite knows; it changes nothing",
		"config.toml: tui.voice: not a key this version of Norite knows; it changes nothing",
	}, warnings)

	d := &fakeDaemon{status: available()}
	r := noriteWith(t, d, func() (Read, []string, error) { return body, warnings, nil }, false,
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
	assert.False(t, body.Processing.EchoCancellation)
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

// A join whose call ended as it started, neither device usable, says what the devices were doing, from the
// status the daemon keeps; and says nothing of devices an earlier call left there (M32).
func TestAJoinEndedByItsDevicesSaysWhatTheyWereDoing(t *testing.T) {
	const why = "neither the microphone nor the speakers can be used"
	none := &ipc.VoiceDevice{State: ipc.VoiceDeviceNoSession}
	d := &fakeDaemon{status: ipc.VoiceStatus{State: ipc.VoiceIdle, Available: true, Reason: why, Microphone: none,
		Speakers: none}, refuse: &ipc.RelayError{Code: ipc.RelayFailed, Message: why}}
	r := norite(t, d, false, "join", "1234")
	require.Error(t, r.err)
	assert.Contains(t, r.out, "Nothing can use the microphone or speakers until you log in at this machine.\n")

	d = &fakeDaemon{status: ipc.VoiceStatus{State: ipc.VoiceIdle, Available: true, Reason: why, Microphone: none,
		Speakers: none}, refuse: &ipc.RelayError{Code: ipc.RelayFailed, Message: "the voice server refused: 409 room full"}}
	r = norite(t, d, false, "join", "1234")
	require.Error(t, r.err)
	assert.NotContains(t, r.out, "logs in")
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
		{ipc.VoiceStatus{State: ipc.VoiceIdle, Reason: "the voice server closed the call\x1b[2J", EndedInCall: true,
			Available: true}, []string{"The last call ended: the voice server closed the call"}, []string{"\x1b"}},
		// A join that failed is said as one, not as a call that ended (M33's manual pass).
		{ipc.VoiceStatus{State: ipc.VoiceIdle, Reason: "this account may not join that voice channel", Available: true},
			[]string{"Not in a voice call.\nThe last join failed: this account may not join that voice channel.\n"},
			[]string{"call ended"}},
		{ipc.VoiceStatus{State: ipc.VoiceIdle, Unavailable: "not yet"}, []string{"cannot join voice calls: not yet."}, nil},
		// The devices (M32): during a call, the worker's reason cleaned; after a call their failing ended.
		{ipc.VoiceStatus{State: ipc.VoiceInCall, Channel: "42", Available: true,
			Microphone: &ipc.VoiceDevice{State: ipc.VoiceDeviceFailed, Reason: "it stopped\x1b[2J delivering audio"},
			Speakers:   &ipc.VoiceDevice{State: ipc.VoiceDeviceOpen}},
			[]string{"The microphone failed: it stopped", "delivering audio.\nThe speakers are on.\n"}, []string{"\x1b"}},
		{ipc.VoiceStatus{State: ipc.VoiceIdle, Available: true, Reason: "neither the microphone nor the speakers can be used",
			Microphone: &ipc.VoiceDevice{State: ipc.VoiceDeviceNoSession},
			Speakers:   &ipc.VoiceDevice{State: ipc.VoiceDeviceNoSession}},
			[]string{"The last join failed: neither the microphone nor the speakers can be used.\nNothing can use the " +
				"microphone or speakers until you log in at this machine.\n"}, nil},
		// What the instance lets the account do (M33): said when it takes something away, and not otherwise.
		{ipc.VoiceStatus{State: ipc.VoiceInCall, Channel: "42", Available: true,
			Rights: &ipc.VoiceRights{Send: false, Hear: false}},
			[]string{"You are not heard: a moderator muted you, or this channel does not let you speak.\n" +
				"You hear nothing: a moderator deafened you.\n"}, nil},
		{ipc.VoiceStatus{State: ipc.VoiceInCall, Channel: "42", Available: true,
			Rights: &ipc.VoiceRights{Send: true, Hear: true}}, nil, []string{"not heard", "hear nothing"}},
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

// The devices are in --json as the contract has them (rule 15).
func TestTheDevicesAreInTheJSON(t *testing.T) {
	d := &fakeDaemon{status: ipc.VoiceStatus{State: ipc.VoiceInCall, Channel: "42", Available: true,
		Microphone: &ipc.VoiceDevice{State: ipc.VoiceDeviceAsking},
		Speakers:   &ipc.VoiceDevice{State: ipc.VoiceDeviceFailed, Reason: "it opened and delivers no audio"},
		Rights:     &ipc.VoiceRights{Send: false, Hear: true}, LocalMutes: []string{}, Speaking: []string{}}}
	r := norite(t, d, true, "status")
	require.NoError(t, r.err)
	daemontest.MatchesCLISchema(t, r.out, "voice.schema.json", "status")
	var got ipc.VoiceStatus
	require.NoError(t, json.Unmarshal([]byte(r.out), &got))
	assert.Equal(t, d.status, got)
}

// What the person sets for their calls asks its own request, the ones carrying an id or a number only of a
// daemon that answered GET first, and the ones carrying nothing straight away (M34). Each prints the
// contract.
func TestThePersonsCommandsAskTheirOwnRequests(t *testing.T) {
	cases := []struct {
		args   []string
		asked  []string
		writes []string
	}{
		{[]string{"mute"}, []string{"POST " + ipc.PathVoiceMute}, nil},
		{[]string{"unmute"}, []string{"POST " + ipc.PathVoiceUnmute}, nil},
		{[]string{"deafen"}, []string{"POST " + ipc.PathVoiceDeafen}, nil},
		{[]string{"undeafen"}, []string{"POST " + ipc.PathVoiceUndeafen}, nil},
		{[]string{"local-mute", "77"}, []string{"GET " + ipc.PathVoice, "POST " + ipc.PathVoiceLocalMute("77")}, nil},
		{[]string{"local-unmute", "77"}, []string{"GET " + ipc.PathVoice, "POST " + ipc.PathVoiceLocalUnmute("77")}, nil},
		{[]string{"streams", "2"}, []string{"GET " + ipc.PathVoice, "POST " + ipc.PathVoiceStreams(2)},
			[]string{"voice.max_streams=2"}},
		{[]string{"streams", "all"}, []string{"GET " + ipc.PathVoice, "POST " + ipc.PathVoiceStreams(0)},
			[]string{"voice.max_streams=all"}},
		// With no call, a change of processing is the file's alone: the next join reads it.
		{[]string{"processing", "--auto-gain", "off"}, []string{"GET " + ipc.PathVoice},
			[]string{"voice.auto_gain=off"}},
	}
	for _, tc := range cases {
		written = &writes{}
		d := &fakeDaemon{status: available()}
		r := norite(t, d, true, tc.args...)
		require.NoError(t, r.err, tc.args)
		assert.Equal(t, tc.asked, d.asked, tc.args)
		assert.Equal(t, tc.writes, written.got, tc.args)
		daemontest.MatchesCLISchema(t, r.out, "voice.schema.json", "status")
	}
}

// A change of processing during a call writes the file and sends the call the whole of it, the switches
// the file already had included.
func TestAProcessingChangeReachesTheCall(t *testing.T) {
	written = &writes{}
	d := &fakeDaemon{status: ipc.VoiceStatus{State: ipc.VoiceInCall, Channel: "42", Available: true}}
	r := norite(t, d, false, "processing", "--echo-cancellation", "off", "--auto-gain", "on")
	require.NoError(t, r.err)
	assert.Equal(t, []string{"GET " + ipc.PathVoice, "POST " + ipc.PathVoiceProcessing}, d.asked)
	assert.Equal(t, `{"echo_cancellation":false,"noise_suppression":true,"auto_gain":true}`, d.bodies[1])
	assert.Equal(t, []string{"voice.echo_cancellation=off", "voice.auto_gain=on"}, written.got)
}

// A join sends how many people the call is heard from first, when the file and the daemon disagree, and
// never otherwise.
func TestAJoinSendsTheStreamsTheFileSays(t *testing.T) {
	two := func() (Read, []string, error) { return Read{Processing: ipc.VoiceAllOn(), Streams: 2}, nil, nil }
	d := &fakeDaemon{status: available()}
	require.NoError(t, noriteWith(t, d, two, false, "join", "1234").err)
	assert.Equal(t, []string{"GET " + ipc.PathVoice, "POST " + ipc.PathVoiceStreams(2),
		"POST " + ipc.PathVoiceJoin("1234")}, d.asked)

	n := 2
	agrees := &fakeDaemon{status: ipc.VoiceStatus{State: ipc.VoiceIdle, Available: true, MaxStreams: &n}}
	require.NoError(t, noriteWith(t, agrees, two, false, "join", "1234").err)
	assert.Equal(t, []string{"GET " + ipc.PathVoice, "POST " + ipc.PathVoiceJoin("1234")}, agrees.asked)
}

// What these commands are given wrong is a usage error, said before anything is attached to or written.
func TestThePersonsCommandsRefuseWhatIsNotTheirs(t *testing.T) {
	for _, args := range [][]string{
		{"local-mute"}, {"local-mute", "bob"}, {"local-unmute", "7", "8"}, {"mute", "7"},
		{"streams"}, {"streams", "4"}, {"streams", "0"}, {"streams", "two"},
		{"processing"}, {"processing", "--auto-gain", "maybe"},
	} {
		written = &writes{}
		d := &fakeDaemon{status: available()}
		r := norite(t, d, false, args...)
		var usage *clierr.UsageError
		require.ErrorAs(t, r.err, &usage, "%v", args)
		assert.Empty(t, d.asked, "%v", args)
		assert.Empty(t, written.got, "%v", args)
	}
}

// A local mute and a report are separate acts (architecture.md §6): muting somebody asks the daemon alone,
// and never reaches the instance's reports.
func TestALocalMuteFilesNoReport(t *testing.T) {
	d := &fakeDaemon{status: available()}
	require.NoError(t, norite(t, d, false, "local-mute", "77").err)
	for _, a := range d.asked {
		assert.True(t, strings.Contains(a, " "+ipc.PathVoice), "a local mute asked %s", a)
	}
}

// The status says who speaks, what the person set on themselves, whom they muted, how many they hear, and
// what the processing is doing, each in words.
func TestTheStatusSaysTheCallsControl(t *testing.T) {
	two := 2
	d := &fakeDaemon{status: ipc.VoiceStatus{State: ipc.VoiceInCall, Channel: "42", Available: true,
		Self: ipc.VoiceSelf{Mute: true}, LocalMutes: []string{"7", "8"}, MaxStreams: &two,
		Speaking: []string{"9"}, Processing: new(ipc.VoiceAllOn()),
		DSP: &ipc.VoiceDSP{State: ipc.VoiceDSPRunning, NoiseSuppression: true}}}
	r := norite(t, d, false, "status")
	require.NoError(t, r.err)
	assert.Equal(t, "In a voice call in channel 42.\n"+
		"Processing the microphone with echo cancellation off, noise suppression on, automatic gain off.\n"+
		"Speaking now: 9.\n"+
		"You muted yourself: nobody hears you.\n"+
		"Muted for you alone: 7, 8.\n"+
		"You hear at most 2 people at once.\n", r.out)

	d.status.DSP = &ipc.VoiceDSP{State: ipc.VoiceDSPFailed, Reason: "it trapped \x1b[31m"}
	d.status.Speaking, d.status.Self = nil, ipc.VoiceSelf{Mute: true, Deaf: true}
	r = norite(t, d, false, "status")
	require.NoError(t, r.err)
	assert.Contains(t, r.out, "The microphone goes unprocessed: it trapped")
	assert.NotContains(t, r.out, "\x1b")
	assert.Contains(t, r.out, "Nobody is speaking.\n")
	assert.Contains(t, r.out, "You deafened yourself: you hear nobody, and nobody hears you.\n")
}
