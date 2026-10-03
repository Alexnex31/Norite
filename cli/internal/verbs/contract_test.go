// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package verbs

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/cli/internal/daemontest"
)

// The verbs are tested against a fake daemon that holds them to openapi.yaml: every relayed request must
// name a route and method the contract has, carry only query parameters it declares, and send a body its
// request schema accepts; every answer the tests script must match the response schema for its status. So a
// verb cannot send what the instance would refuse to decode, and a test cannot pass by answering with
// something the instance would never send (M19's lesson about fixtures, again).
//
// What every verb prints under --json is validated against its schema in contracts/cli-json/.

// The fake daemon is daemontest's since M20a, when the terminal client's tests needed it too. These names
// keep the verbs' tests reading as they did.
type (
	fakeDaemon = daemontest.Daemon
	request    = daemontest.Request
	answerFunc = daemontest.Answer
)

func newFake(t *testing.T) *fakeDaemon { return daemontest.New(t) }

// matchesCLISchema validates what a verb printed against one definition in contracts/cli-json/.
func matchesCLISchema(t *testing.T, out, file, def string) {
	t.Helper()
	daemontest.MatchesCLISchema(t, out, file, def)
}

// ---------- running a verb ----------

type ran struct {
	out string
	err error
}

// runVerb runs argv through the real verb tree, attached to f, with stdin answering any question.
func runVerb(t *testing.T, f *fakeDaemon, stdin string, argv ...string) ran {
	t.Helper()
	connect := func(context.Context) (daemonclient.Caller, func(), error) { return f, func() {}, nil }
	var out, errOut bytes.Buffer
	root := &cli.Command{
		Name: "norite", Writer: &out, ErrWriter: &errOut, Reader: strings.NewReader(stdin),
		Flags:          []cli.Flag{&cli.BoolFlag{Name: "json"}},
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
		Commands:       Commands(connect),
	}
	err := root.Run(context.Background(), append([]string{"norite"}, argv...))
	return ran{out: out.String(), err: err}
}
