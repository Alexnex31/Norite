// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package verbs

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/cli/internal/output"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

type nothing struct{}

func (nothing) Do(context.Context, string, string, any) (ipc.Result, error) {
	return ipc.Result{Status: 204}, nil
}

type said struct{ Word string }

func (s said) Text(t *output.Text) { t.Line("%s", s.Word) }

// probe runs a one-verb tree the way the real ones run: through run, with the root's --json and Reader.
func probe(t *testing.T, argv []string, in string, connected *bool,
	verb func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error), names ...string,
) (string, error) {
	t.Helper()
	usage, meta := ids(names...)
	connect := func(context.Context) (daemonclient.Caller, func(), error) {
		*connected = true
		return nothing{}, func() {}, nil
	}
	var out bytes.Buffer
	root := &cli.Command{
		Name: "norite", Writer: &out, Reader: strings.NewReader(in),
		Flags:          []cli.Flag{&cli.BoolFlag{Name: "json"}},
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
		Commands: []*cli.Command{{
			Name: "verb", ArgsUsage: usage, Metadata: meta, Flags: []cli.Flag{yesFlag, limitFlag(50),
				beforeFlag("things"), afterFlag("things")},
			Action: run(connect, verb),
		}},
	}
	err := root.Run(context.Background(), append([]string{"norite"}, argv...))
	return out.String(), err
}

func TestAnIdArgumentIsDigitsAndCheckedBeforeTheDaemon(t *testing.T) {
	verb := func(context.Context, *cli.Command, *env) (output.Result, error) { return said{"ran"}, nil }

	for _, argv := range [][]string{
		{"verb"},
		{"verb", "1", "2"},
		{"verb", "../auth/tokens"},
		{"verb", "12a"},
		{"verb", "123456789012345678901"},
	} {
		connected := false
		_, err := probe(t, argv, "", &connected, verb, "guild")
		var usage *clierr.UsageError
		require.ErrorAs(t, err, &usage, "%v", argv)
		assert.False(t, connected, "a usage error is found before attaching: %v", argv)
	}

	connected := false
	out, err := probe(t, []string{"verb", "7238829238972837423"}, "", &connected, verb, "guild")
	require.NoError(t, err)
	assert.True(t, connected)
	assert.Equal(t, "ran\n", out)
}

func TestTheRootsJSONFlagChoosesTheForm(t *testing.T) {
	verb := func(context.Context, *cli.Command, *env) (output.Result, error) {
		return said{"a\u202eb"}, nil
	}
	connected := false
	out, err := probe(t, []string{"--json", "verb"}, "", &connected, verb)
	require.NoError(t, err)
	assert.JSONEq(t, `{"Word":"a\u202eb"}`, out)
	assert.Contains(t, out, `\u202e`, "through the escaping writer")
}

func TestADestructiveVerbAsksAndOnlyYesProceeds(t *testing.T) {
	destroyed := 0
	verb := func(_ context.Context, cmd *cli.Command, e *env) (output.Result, error) {
		if err := confirm(cmd, e, "delete guild 1"); err != nil {
			return nil, err
		}
		destroyed++
		return nil, nil
	}
	interactive := func(answer string) error {
		connected := false
		_, err := probe(t, []string{"verb", "1"}, answer, &connected,
			func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				e.interactive = true
				return verb(ctx, cmd, e)
			}, "guild")
		return err
	}

	require.NoError(t, interactive("y\n"))
	require.NoError(t, interactive("YES\n"))
	assert.Equal(t, 2, destroyed)

	for _, answer := range []string{"n\n", "\n", "", "yeah\n"} {
		err := interactive(answer)
		var usage *clierr.UsageError
		require.ErrorAs(t, err, &usage, "%q declines", answer)
	}
	assert.Equal(t, 2, destroyed, "nothing proceeds without a yes")

	// No terminal: a script that forgot --yes fails as a usage error naming the flag, never proceeds.
	connected := false
	_, err := probe(t, []string{"verb", "1"}, "y\n", &connected, verb, "guild")
	require.True(t, errors.Is(err, clierr.ErrNoTerminal), "got %v", err)
	assert.Contains(t, err.Error(), "--yes")
	assert.Equal(t, 2, destroyed)

	_, err = probe(t, []string{"verb", "--yes", "1"}, "", &connected, verb, "guild")
	require.NoError(t, err)
	assert.Equal(t, 3, destroyed)
}

func TestAPageCarriesTheNextCursorUntilAShortOne(t *testing.T) {
	cursor := func(s string) string { return s }

	full := newPage([]string{"9", "8", "7"}, 3, cursor)
	require.NotNil(t, full.Next)
	assert.Equal(t, "7", *full.Next)

	short := newPage([]string{"6"}, 3, cursor)
	assert.Nil(t, short.Next, "a page shorter than the limit is the last")

	empty := newPage[string](nil, 3, cursor)
	assert.NotNil(t, empty.Items, "[] in JSON, never null")
	assert.Nil(t, empty.Next)
}

func TestPagingFlagsAreCheckedAndEncoded(t *testing.T) {
	check := func(argv ...string) (string, error) {
		var got string
		connected := false
		_, err := probe(t, append([]string{"verb"}, argv...), "", &connected,
			func(_ context.Context, cmd *cli.Command, _ *env) (output.Result, error) {
				q, _, err := query(cmd, []string{"before", "after"}, nil)
				got = q
				return nil, err
			})
		return got, err
	}

	q, err := check()
	require.NoError(t, err)
	assert.Equal(t, "?limit=50", q)

	q, err = check("--limit", "10", "--before", "123")
	require.NoError(t, err)
	assert.Equal(t, "?before=123&limit=10", q)

	for _, bad := range [][]string{
		{"--limit", "0"}, {"--limit", "101"}, {"--before", "x&admin=1"}, {"--before", "1", "--after", "2"},
	} {
		_, err := check(bad...)
		var usage *clierr.UsageError
		assert.ErrorAs(t, err, &usage, "%v", bad)
	}
}
