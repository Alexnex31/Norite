// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonctl

import (
	"context"
	"fmt"
	"os"
	"testing"
)

// The commands here stop a daemon, and since M23 they ask it over its socket first. Run from somebody's
// shell, that socket is their own daemon's. So before any test runs the state directory is pointed
// somewhere empty, where no daemon listens, and the socket is replaced by one that fails the suite: a test
// that reaches it has forgotten to say what the socket answers.
// realSocketStop is the socket as the commands use it outside a test, for the one test that points it at
// an empty state directory.
var realSocketStop func(context.Context, string) stopOutcome

// realSocketStatus is the same for the status question.
var realSocketStatus func(context.Context, string) socketAnswer

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "norite-daemonctl-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, name := range []string{"XDG_STATE_HOME", "LOCALAPPDATA", "HOME", "USERPROFILE"} {
		_ = os.Setenv(name, dir)
	}
	realSocketStop = socketStop
	socketStop = func(context.Context, string) stopOutcome {
		panic("a daemonctl test reached the real attach socket; say what it answers with answerStop")
	}
	realSocketStatus = socketStatus
	socketStatus = func(context.Context, string) socketAnswer {
		panic("a daemonctl test reached the real attach socket; say what it answers with answerStatus")
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
