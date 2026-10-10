// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package login

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDaemon is the daemon's service as a test dictates it.
type fakeDaemon struct {
	state      DaemonState
	installErr error
	startErr   error
	installs   int
	starts     int
}

func (d *fakeDaemon) State(context.Context) DaemonState { return d.state }
func (d *fakeDaemon) Install(_ context.Context, out io.Writer) error {
	d.installs++
	if d.installErr == nil {
		_, _ = io.WriteString(out, "Installed and started norite-daemon.\n")
	}
	return d.installErr
}
func (d *fakeDaemon) Start(context.Context) error { d.starts++; return d.startErr }

// signIn runs a password login to its end with the daemon standing as given, answering the offer with
// answer when it is made, and returns what was printed and what the offer was.
func signInWith(t *testing.T, d Daemon, interactive bool, answer string) (out string, asked []string) {
	t.Helper()
	r, _, buf := testRunner(t, newFakeInstance(t), Options{Email: "ada@example.com"})
	r.Daemon = d
	r.Interactive = interactive
	if !interactive {
		t.Setenv(passwordEnvVar, "a correct passphrase")
	}
	r.ReadLine = func(prompt string) (string, error) {
		asked = append(asked, prompt)
		return answer, nil
	}
	require.NoError(t, r.Run(context.Background()))
	return buf.String(), asked
}

func TestASignInEndsBySayingHowTheDaemonStands(t *testing.T) {
	t.Run("running: nothing to do, and nothing asked", func(t *testing.T) {
		d := &fakeDaemon{state: DaemonRunning}
		out, asked := signInWith(t, d, true, "y")
		assert.Contains(t, out, "picks this sign-in up by itself")
		assert.Empty(t, asked)
		assert.Zero(t, d.installs+d.starts)
	})
	t.Run("no service at all: the command, as before", func(t *testing.T) {
		out, asked := signInWith(t, nil, true, "y")
		assert.Contains(t, out, "norite daemon start")
		assert.Empty(t, asked)
	})
	t.Run("could not be found out: the command, and nothing claimed", func(t *testing.T) {
		d := &fakeDaemon{state: DaemonUnknown}
		out, asked := signInWith(t, d, true, "y")
		assert.Contains(t, out, "norite daemon start")
		assert.NotContains(t, out, "No daemon is installed")
		assert.Empty(t, asked)
		assert.Zero(t, d.installs+d.starts)
	})
}

// The offer is the whole of what "auto-install" means: made when there is somebody to answer, and acted
// on only when they say yes.
func TestADaemonIsInstalledOnlyWhenSomebodySaysYes(t *testing.T) {
	for answer, want := range map[string]int{"y": 1, "yes": 1, " Y ": 1, "": 0, "n": 0, "no": 0, "maybe": 0, "yep": 0} {
		d := &fakeDaemon{state: DaemonAbsent}
		out, asked := signInWith(t, d, true, answer)
		require.Len(t, asked, 1, "answer %q", answer)
		assert.Contains(t, asked[0], "[y/N]", "the default is no")
		assert.Equal(t, want, d.installs, "answer %q", answer)
		assert.Zero(t, d.starts)
		if want == 0 {
			assert.Contains(t, out, "norite daemon install", "declined, it says how to do it later")
		} else {
			assert.Contains(t, out, "Installed and started")
		}
		// What it would be agreeing to is said before the question.
		assert.Contains(t, out, "starts each time you log in")
	}
}

func TestWithNobodyToAskNothingIsInstalled(t *testing.T) {
	d := &fakeDaemon{state: DaemonAbsent}
	out, asked := signInWith(t, d, false, "y")
	assert.Empty(t, asked)
	assert.Zero(t, d.installs)
	assert.Contains(t, out, "norite daemon install")

	d = &fakeDaemon{state: DaemonStopped}
	out, asked = signInWith(t, d, false, "y")
	assert.Empty(t, asked)
	assert.Zero(t, d.starts)
	assert.Contains(t, out, "norite daemon start")
}

func TestAnInstalledDaemonThatIsStoppedIsStartedUnlessDeclined(t *testing.T) {
	for answer, want := range map[string]int{"": 1, "y": 1, "yes": 1, "n": 0, "no": 0, "what": 0} {
		d := &fakeDaemon{state: DaemonStopped}
		out, asked := signInWith(t, d, true, answer)
		require.Len(t, asked, 1, "answer %q", answer)
		assert.Contains(t, asked[0], "[Y/n]", "it is installed already, so starting it is the default")
		assert.Equal(t, want, d.starts, "answer %q", answer)
		assert.Zero(t, d.installs)
		if want == 0 {
			assert.Contains(t, out, "norite daemon start")
		}
	}
}

// The sign-in is stored and good whatever becomes of the offer: a daemon that will not install is said,
// with the service manager's words made safe to print, and the login still succeeds.
func TestAFailedInstallDoesNotFailTheLogin(t *testing.T) {
	d := &fakeDaemon{state: DaemonAbsent, installErr: errors.New("`systemctl --user enable` failed\x1b[2J")}
	r, store, buf := testRunner(t, newFakeInstance(t), Options{Email: "ada@example.com"})
	r.Daemon = d
	r.ReadLine = func(string) (string, error) { return "y", nil }
	require.NoError(t, r.Run(context.Background()))

	out := buf.String()
	assert.Contains(t, out, "could not be installed")
	assert.Contains(t, out, "systemctl --user enable")
	assert.NotContains(t, out, "\x1b", "the service manager's words reach a terminal")
	assert.Contains(t, out, "norite daemon install")
	_, _, err := store.Load()
	require.NoError(t, err, "the credential is stored")

	d = &fakeDaemon{state: DaemonStopped, startErr: errors.New("it would not start")}
	out, _ = signInWith(t, d, true, "y")
	assert.Contains(t, out, "could not be started: it would not start")
}

// A read that fails is not a yes.
func TestAQuestionNobodyAnswersIsANo(t *testing.T) {
	d := &fakeDaemon{state: DaemonAbsent}
	r, _, buf := testRunner(t, newFakeInstance(t), Options{Email: "ada@example.com"})
	r.Daemon = d
	r.ReadLine = func(prompt string) (string, error) {
		if strings.Contains(prompt, "Install") {
			return "y", errors.New("the terminal went away")
		}
		return "ada@example.com", nil
	}
	require.NoError(t, r.Run(context.Background()))
	assert.Zero(t, d.installs)
	assert.Contains(t, buf.String(), "norite daemon install")
}
