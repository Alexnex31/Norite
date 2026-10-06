// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/cli/internal/daemontest"
	"github.com/Alexnex31/Norite/cli/internal/verbs"
)

func runAbout(t *testing.T, connect verbs.Connector, asJSON bool) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := &cli.Command{
		Name: "norite", Writer: &out, ErrWriter: &out,
		Flags:          []cli.Flag{&cli.BoolFlag{Name: JSONFlagName}},
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
		Commands:       []*cli.Command{aboutCommand(connect)},
	}
	argv := []string{"norite", "about"}
	if asJSON {
		argv = []string{"norite", "--json", "about"}
	}
	err := root.Run(context.Background(), argv)
	return out.String(), err
}

func through(d *daemontest.Daemon) verbs.Connector {
	return func(context.Context) (daemonclient.Caller, func(), error) { return d, func() {}, nil }
}

// TestAboutPrintsTheNoticeAndTheInstancesOffer: this build's notice and the instance's offer, each labeled
// as what it is, in both presentations; --json matches contracts/cli-json/about.schema.json (rule 15).
func TestAboutPrintsTheNoticeAndTheInstancesOffer(t *testing.T) {
	d := daemontest.New(t).On("getMeta", daemontest.OK(apicontract.InstanceMeta{
		License: "AGPL-3.0-or-later", SourceUrl: "https://git.example.org/fork", SourceRevision: "abc123",
	}))

	text, err := runAbout(t, through(d), false)
	require.NoError(t, err)
	for _, want := range []string{
		"This build", "Your instance", "https://git.example.org/fork", "abc123",
		"ABSOLUTELY NO WARRANTY", "GNU Affero General Public License", "Copyright (C) 2026 Alexandre Duffez",
		"https://www.gnu.org/licenses/agpl-3.0.html", SourceURL,
	} {
		assert.Contains(t, text, want)
	}

	js, err := runAbout(t, through(d), true)
	require.NoError(t, err)
	daemontest.MatchesCLISchema(t, js, "about.schema.json", "about")
	var back aboutView
	require.NoError(t, json.Unmarshal([]byte(js), &back))
	require.NotNil(t, back.Instance)
	assert.Equal(t, "https://git.example.org/fork", back.Instance.SourceURL)
	assert.Equal(t, SourceURL, back.Build.SourceURL, "the build's own source, not the instance's")
}

// TestAboutPrintsWithoutTheInstance: a stopped or signed-out daemon leaves the instance's block saying why,
// and the command still succeeds, because the notice is what it is for.
func TestAboutPrintsWithoutTheInstance(t *testing.T) {
	down := func(context.Context) (daemonclient.Caller, func(), error) {
		return nil, nil, clierr.Unavailable("the daemon is not running")
	}
	text, err := runAbout(t, down, false)
	require.NoError(t, err)
	assert.Contains(t, text, "could not be asked: the daemon is not running")
	assert.Contains(t, text, "ABSOLUTELY NO WARRANTY")

	js, err := runAbout(t, down, true)
	require.NoError(t, err)
	daemontest.MatchesCLISchema(t, js, "about.schema.json", "about")
	var back aboutView
	require.NoError(t, json.Unmarshal([]byte(js), &back))
	assert.Nil(t, back.Instance)
	require.NotNil(t, back.InstanceUnavailable)
}

// TestTheInstancesOfferIsAStrangersText: the instance's source and revision are printed, and an instance is
// a stranger's server (rule 19).
func TestTheInstancesOfferIsAStrangersText(t *testing.T) {
	// The URL is declared a URI, which cannot carry a control character, so the fake would refuse one
	// there; a bidi override is a valid URI and the reordering it causes is what rule 19 removes.
	hostile := "Evil\x1b]0;owned\x07\u202eesrever"
	d := daemontest.New(t).On("getMeta", daemontest.OK(apicontract.InstanceMeta{
		License: hostile, SourceUrl: "https://evil.example/\u202egnp.exe", SourceRevision: hostile,
	}))
	text, err := runAbout(t, through(d), false)
	require.NoError(t, err)
	assert.NotContains(t, text, "\x1b")
	assert.NotContains(t, text, "\u202e")
}

// TestTheRevisionIsStampedThenReadFromTheBuildThenUnknown: the done-when asks for a revision that resolves
// in the repository the notice names, so the order is how much each can be trusted. A stamp wins; then the
// commit `go build` recorded, flagged when the checkout had uncommitted changes; then "unknown", never a
// plausible-looking placeholder.
func TestTheRevisionIsStampedThenReadFromTheBuildThenUnknown(t *testing.T) {
	vcs := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "1111111111111111111111111111111111111111"},
		{Key: "vcs.modified", Value: "true"},
	}}

	stamped := revisionOf("2222222222222222222222222222222222222222", vcs)
	assert.Equal(t, "2222222222222222222222222222222222222222", stamped.Revision)
	assert.Equal(t, "stamped", stamped.RevisionFrom)
	assert.False(t, stamped.Modified, "a stamped build makes no claim about a checkout")

	read := revisionOf("", vcs)
	assert.Equal(t, "1111111111111111111111111111111111111111", read.Revision)
	assert.Equal(t, "vcs", read.RevisionFrom)
	assert.True(t, read.Modified)

	for _, info := range []*debug.BuildInfo{nil, {Settings: []debug.BuildSetting{{Key: "vcs.modified", Value: "true"}}}} {
		none := revisionOf("", info)
		assert.Equal(t, "unknown", none.Revision)
		assert.Equal(t, "unknown", none.RevisionFrom)
		assert.False(t, none.Modified)
	}
}
