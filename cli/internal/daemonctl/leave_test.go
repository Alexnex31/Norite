// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonctl

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/Alexnex31/Norite/daemon/ipc"
)

// voiceDaemon answers the voice status with status, and every leave with 200.
type voiceDaemon struct {
	status ipc.Result
	err    error
	asked  []string
}

func (d *voiceDaemon) Do(_ context.Context, method, path string, _ any) (ipc.Result, error) {
	d.asked = append(d.asked, method+" "+path)
	if path == ipc.PathVoiceLeave {
		return ipc.Result{Status: http.StatusOK, Body: json.RawMessage(`{}`)}, nil
	}
	return d.status, d.err
}

func voiceBody(t *testing.T, st ipc.VoiceStatus) ipc.Result {
	t.Helper()
	body, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	return ipc.Result{Status: http.StatusOK, Body: body}
}

// A daemon in a call, or joining one again, is asked to leave it; anything else is asked nothing more than
// how its call stands, an older daemon's relayed answer and a daemon with none included (M36's D6).
func TestADaemonInACallIsAskedToLeaveIt(t *testing.T) {
	leave := []string{"GET " + ipc.PathVoice, "POST " + ipc.PathVoiceLeave}
	only := []string{"GET " + ipc.PathVoice}
	for _, tc := range []struct {
		name string
		d    *voiceDaemon
		want []string
	}{
		{"in a call", &voiceDaemon{status: voiceBody(t, ipc.VoiceStatus{State: ipc.VoiceInCall, Channel: "1"})}, leave},
		{"joining one again", &voiceDaemon{status: voiceBody(t, ipc.VoiceStatus{State: ipc.VoiceJoining, Channel: "1",
			Rejoin: &ipc.VoiceRejoin{Cause: ipc.VoiceRejoinDaemonRestarted}})}, leave},
		{"in none", &voiceDaemon{status: voiceBody(t, ipc.VoiceStatus{State: ipc.VoiceIdle})}, only},
		{"one given up", &voiceDaemon{status: voiceBody(t, ipc.VoiceStatus{State: ipc.VoiceIdle,
			Rejoin: &ipc.VoiceRejoin{Cause: ipc.VoiceRejoinSessionLost, Attempt: 3}})}, only},
		{"older, the instance answering", &voiceDaemon{status: ipc.Result{Status: http.StatusNotFound,
			Body: json.RawMessage(`{"code":"not_found"}`)}}, only},
		{"not answering", &voiceDaemon{err: errors.New("closed")}, only},
		{"answering something else", &voiceDaemon{status: ipc.Result{Status: http.StatusOK, Body: json.RawMessage(`[]`)}}, only},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left := leaveCall(t.Context(), tc.d)
			if !slices.Equal(tc.d.asked, tc.want) {
				t.Errorf("asked %v, want %v", tc.d.asked, tc.want)
			}
			if left != (len(tc.want) == 2) {
				t.Errorf("reported leaving %t", left)
			}
		})
	}
}

// `stop` and `uninstall` leave the call before the stop; `restart` keeps it, and the daemon joins it again.
func TestStopAndUninstallLeaveTheCallAndRestartKeepsIt(t *testing.T) {
	for cmd, want := range map[string]int{"stop": 1, "uninstall": 1, "restart": 0} {
		t.Run(cmd, func(t *testing.T) {
			mgr := &stubManager{state: State{Installed: true, Running: true}}
			answerStop(t, stopGone)
			left := answerLeave(t)
			if _, err := runCommand(t, mgr, "daemon", cmd); err != nil {
				t.Fatalf("%s: %v", cmd, err)
			}
			if *left != want {
				t.Errorf("asked to leave the call %d times, want %d", *left, want)
			}
		})
	}
}
