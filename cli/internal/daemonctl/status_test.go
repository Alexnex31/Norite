// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonctl

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/daemontest"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var coder cli.ExitCoder
	if !errors.As(err, &coder) {
		t.Fatalf("status returned an error with no exit code: %v", err)
	}
	return coder.ExitCode()
}

var signedIn = socketAnswer{Running: true, Attached: true, Version: "0.1.0", Standing: ipc.StandingSignedIn,
	Account: &ipc.Account{InstanceURL: "https://chat.example.com", UserID: "1", Username: "ada"}}

func TestStatusAsksTheDaemonAsWellAsTheServiceManager(t *testing.T) {
	cases := []struct {
		name   string
		state  State
		socket socketAnswer
		code   int
		view   statusView
		// text is what the output must say, and absent what it must not.
		text   []string
		absent []string
	}{
		{
			name: "the service, running and signed in", state: State{Installed: true, Running: true, Detail: "active"},
			socket: signedIn, code: 0,
			view: statusView{Running: true, Answering: true, Installed: true, Service: "active", Version: "0.1.0",
				Standing: "signed_in", Instance: "https://chat.example.com", Username: "ada"},
			text: []string{"is running (active)", "version:  0.1.0", "ada at https://chat.example.com", "norite logs tail"},
		},
		{
			// Before M23 this was "not installed", exit 2, about a daemon serving clients.
			name: "started by hand", state: State{}, socket: signedIn, code: 0,
			view: statusView{Running: true, Answering: true, Version: "0.1.0", Standing: "signed_in",
				Instance: "https://chat.example.com", Username: "ada"},
			text:   []string{"started by hand", "norite daemon install"},
			absent: []string{"is not installed."},
		},
		{
			name: "installed and stopped", state: State{Installed: true, Detail: "inactive"}, code: 1,
			view:   statusView{Installed: true, Service: "inactive"},
			text:   []string{"installed but not running (inactive)", "norite daemon start"},
			absent: []string{"version:", "account:"},
		},
		{
			// A word from a service manager about a service that is not installed is not reported.
			name: "neither", state: State{Detail: "unknown"}, code: 2,
			view:   statusView{},
			text:   []string{"is not installed"},
			absent: []string{"norite logs tail"},
		},
		{
			// Not signed out: telling somebody to log in here would have them supersede a good sign-in.
			name: "signing in", state: State{Installed: true, Running: true, Detail: "active"},
			socket: socketAnswer{Running: true, Attached: true, Version: "0.1.0", Standing: ipc.StandingStarting}, code: 0,
			view:   statusView{Running: true, Answering: true, Installed: true, Service: "active", Version: "0.1.0", Standing: "starting"},
			text:   []string{"account:  signing in"},
			absent: []string{"norite login"},
		},
		{
			name: "signed out", state: State{Installed: true, Running: true, Detail: "active"},
			socket: socketAnswer{Running: true, Attached: true, Version: "0.1.0", Standing: ipc.StandingSignedOut}, code: 0,
			view: statusView{Running: true, Answering: true, Installed: true, Service: "active", Version: "0.1.0", Standing: "signed_out"},
			text: []string{"not signed in; run `norite login`"},
		},
		{
			name: "another version, which cannot be attached to", state: State{Installed: true, Running: true, Detail: "active"},
			socket: socketAnswer{Running: true, Version: "0.2.0", Problem: "the running daemon is version 0.2.0 and this client is 0.1.0"},
			code:   0,
			view: statusView{Running: true, Installed: true, Service: "active", Version: "0.2.0",
				Problem: "the running daemon is version 0.2.0 and this client is 0.1.0"},
			text:   []string{"is running (active)", "problem:  the running daemon is version 0.2.0"},
			absent: []string{"account:"},
		},
		{
			name: "the service manager says running and nothing answers", state: State{Installed: true, Running: true, Detail: "active"},
			code: 0,
			view: statusView{Running: true, Installed: true, Service: "active",
				Problem: "the service manager reports it running, and nothing answers on its socket: it is still " +
					"starting, or it was started with another state directory than this shell's (XDG_STATE_HOME)"},
			text: []string{"is running (active)", "XDG_STATE_HOME"},
		},
		{
			// A service that is stopped beside a daemon started by hand: running is what is true.
			name: "installed, stopped, and one running by hand", state: State{Installed: true, Detail: "inactive"},
			socket: signedIn, code: 0,
			view: statusView{Running: true, Answering: true, Installed: true, Service: "inactive", Version: "0.1.0",
				Standing: "signed_in", Instance: "https://chat.example.com", Username: "ada"},
			text: []string{"is running (inactive)"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			answerStatus(t, tc.socket)
			out, err := runCommand(t, &stubManager{state: tc.state}, "daemon", "status")
			if got := exitCodeOf(t, err); got != tc.code {
				t.Errorf("exit %d, want %d", got, tc.code)
			}
			for _, want := range tc.text {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			for _, not := range tc.absent {
				if strings.Contains(out, not) {
					t.Errorf("output says %q:\n%s", not, out)
				}
			}

			answerStatus(t, tc.socket)
			out, err = runCommand(t, &stubManager{state: tc.state}, "--json", "daemon", "status")
			if got := exitCodeOf(t, err); got != tc.code {
				t.Errorf("--json: exit %d, want %d: a script reads both", got, tc.code)
			}
			daemontest.MatchesCLISchema(t, out, "daemon.schema.json", "status")
			var view statusView
			if err := json.Unmarshal([]byte(out), &view); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if view != tc.view {
				t.Errorf("--json:\n got %+v\nwant %+v", view, tc.view)
			}
		})
	}
}

// What the daemon and the service manager say about themselves is their text, and a terminal is where it
// is printed (rule 19).
func TestStatusPrintsNothingThatActsOnTheTerminal(t *testing.T) {
	hostile := "\x1b]0;owned\x07\x1b[2J\u202e"
	answerStatus(t, socketAnswer{Running: true, Attached: true, Version: "1" + hostile, Standing: ipc.StandingSignedIn,
		Account: &ipc.Account{InstanceURL: "https://x" + hostile, Username: "ada" + hostile}, Problem: "p" + hostile})
	state := State{Installed: true, Running: true, Detail: "active" + hostile}
	for _, argv := range [][]string{{"daemon", "status"}, {"--json", "daemon", "status"}} {
		answerStatus(t, socketAnswer{Running: true, Attached: true, Version: "1" + hostile, Standing: ipc.StandingSignedIn,
			Account: &ipc.Account{InstanceURL: "https://x" + hostile, Username: "ada" + hostile}, Problem: "p" + hostile})
		out, err := runCommand(t, &stubManager{state: state}, argv...)
		if err != nil {
			t.Fatalf("%v: %v", argv, err)
		}
		for i, r := range out {
			if r != '\n' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || (r >= 0x202a && r <= 0x202e)) {
				t.Fatalf("%v: output holds U+%04X at byte %d: %q", argv, r, i, out)
			}
		}
	}
}

// With no daemon at the socket there is nobody to ask. TestMain points the state directory somewhere
// empty, so this is the real dial finding nothing.
func TestTheSocketStatusFindsNoDaemonInAnEmptyStateDirectory(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	if got := realSocketStatus(t.Context(), "test"); got != (socketAnswer{}) {
		t.Errorf("with no daemon running: %+v", got)
	}
}
