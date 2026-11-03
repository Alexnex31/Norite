// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voicecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/cli/internal/daemontest"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// fakeDaemon answers the voice requests and remembers what it was asked.
type fakeDaemon struct {
	status ipc.VoiceStatus
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
	if ch, ok := ipc.VoiceJoinChannel(path); ok {
		f.status.State, f.status.Channel = ipc.VoiceInCall, ch
	}
	if path == ipc.PathVoiceLeave {
		f.status = ipc.VoiceStatus{State: ipc.VoiceIdle, Reason: "left", Available: f.status.Available}
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
		Unavailable: "this version of Norite cannot join voice calls yet"}}
	r := norite(t, d, false, "join", "1234")
	var refused *clierr.RefusedError
	require.True(t, errors.As(r.err, &refused), "%v", r.err)
	assert.Contains(t, refused.Message, "cannot join voice calls yet")
	assert.Equal(t, []string{"GET " + ipc.PathVoice}, d.asked)
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
