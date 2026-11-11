// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package verbs

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// TestAnInviteLivesAWeekUnlessToldOtherwise: the API's own default is no expiry, as instance invites' is,
// so the safer default is the CLI's to send. An invite that never expires is asked for by name.
func TestAnInviteLivesAWeekUnlessToldOtherwise(t *testing.T) {
	for _, tc := range []struct {
		flags []string
		body  string
	}{
		{nil, `{"expires_in_seconds":604800}`},
		{[]string{"--expires-in", "never"}, `{}`},
		{[]string{"--expires-in", "3d"}, `{"expires_in_seconds":259200}`},
		{[]string{"--expires-in", "12h", "--max-uses", "1"}, `{"expires_in_seconds":43200,"max_uses":1}`},
	} {
		f := newFake(t).On("createChannelInvite", created(apiInvite("90", "BCDFGHJKMNPQRSTV")))
		r := runVerb(t, f, "", append([]string{"invite", "create", "20"}, tc.flags...)...)
		require.NoError(t, r.err, "%v", tc.flags)
		calls := f.Requests()
		require.Len(t, calls, 1)
		assert.JSONEq(t, tc.body, string(calls[0].Body), "%v", tc.flags)
	}
}

// TestAnInviteOutsideTheInstancesBoundsIsAUsageError: a life or a use count the instance would refuse is
// refused here first, as exit 2, with nothing sent and no daemon needed.
func TestAnInviteOutsideTheInstancesBoundsIsAUsageError(t *testing.T) {
	for _, flags := range [][]string{
		{"--expires-in", "30s"}, {"--expires-in", "31d"}, {"--expires-in", "soon"}, {"--expires-in", "0"},
		{"--max-uses", "0"}, {"--max-uses", "1001"},
	} {
		f := newFake(t)
		r := runVerb(t, f, "", append([]string{"invite", "create", "20"}, flags...)...)
		var usage *clierr.UsageError
		require.ErrorAs(t, r.err, &usage, "%v: %v", flags, r.err)
		assert.Empty(t, f.Requests(), "%v", flags)
	}
}

// TestACodeTravelsInTheBodyAndNeverInAPath: the three verbs taking a code send it in a request body, since
// a path is written to the instance's request log and a code is a way into the guild (ADR 0029). The code
// goes as typed; normalizing it is the instance's.
func TestACodeTravelsInTheBodyAndNeverInAPath(t *testing.T) {
	const typed = "bcdf-ghjk-mnpq-rstv"
	f := newFake(t).
		On("previewInvite", ok(apiPreview("BCDFGHJKMNPQRSTV", "Guild"))).
		On("redeemInvite", ok(apiGuild("10", "Guild"))).
		On("revokeInvite", noContent())
	for _, verb := range []string{"show", "join", "revoke"} {
		require.NoError(t, runVerb(t, f, "", "invite", verb, typed).err, verb)
	}
	for _, call := range f.Requests() {
		assert.NotContains(t, strings.ToLower(call.Path), "bcdf", "%s %s", call.Method, call.Path)
		assert.Empty(t, call.Query)
		assert.JSONEq(t, `{"code":"`+typed+`"}`, string(call.Body), call.Path)
	}

	for _, bad := range []string{"", "BCDF\x1b[2J", strings.Repeat("B", 65)} {
		g := newFake(t)
		r := runVerb(t, g, "", "invite", "join", bad)
		var usage *clierr.UsageError
		require.ErrorAs(t, r.err, &usage, "%q", bad)
		assert.Empty(t, g.Requests())
	}
}

// TestAPreviewIsInertInTextAndExactInJSON: a preview is the guild's own text shown to somebody not yet in it
// — its name, its description, its channel and its inviter — so it is the most stranger-written output the
// CLI prints. Rule 19 in both presentations.
func TestAPreviewIsInertInTextAndExactInJSON(t *testing.T) {
	hostile := "Evil\x1b]0;owned\x07 \u202eesrever"
	p := apiPreview("BCDFGHJKMNPQRSTV", hostile)
	p.Guild.Description = &hostile
	p.Channel.Name = &hostile
	p.Inviter = &apicontract.PublicUser{Id: "1", Username: hostile, DisplayName: hostile}
	f := newFake(t).On("previewInvite", ok(p))

	text := runVerb(t, f, "", "invite", "show", "BCDFGHJKMNPQRSTV")
	require.NoError(t, text.err)
	assert.NotContains(t, text.out, "\x1b")
	assert.NotContains(t, text.out, "\u202e")
	assert.Contains(t, text.out, "norite invite join BCDFGHJKMNPQRSTV", "the next step, spelled out")

	js := runVerb(t, f, "", "--json", "invite", "show", "BCDFGHJKMNPQRSTV")
	require.NoError(t, js.err)
	assert.NotContains(t, js.out, "\u202e")
	var back map[string]any
	require.NoError(t, json.Unmarshal([]byte(js.out), &back))
	assert.Equal(t, hostile, back["guild"].(map[string]any)["name"], "lossless")
}

// TestAHugeDayCountIsRefusedNotWrapped: the day count is bounded before it is multiplied, so a number that
// would overflow into a short, valid-looking duration is a usage error (M20a's second /code-review).
func TestAHugeDayCountIsRefusedNotWrapped(t *testing.T) {
	// 106752 days is the first count whose nanoseconds overflow int64; this one wraps to under a day.
	for _, days := range []string{"31d", "106752d", "213504d", "-1d"} {
		f := newFake(t)
		r := runVerb(t, f, "", "invite", "create", "20", "--expires-in", days)
		var usage *clierr.UsageError
		require.ErrorAs(t, r.err, &usage, "%s: %v", days, r.err)
		assert.Empty(t, f.Requests(), days)
	}
}

// runVerbSignedIn runs argv as runVerb does, attached as a client is, to a daemon whose READY names instance,
// or no account for "" as a daemon still starting does. The fake answers as signed in to the instance READY
// named, unless SignInFrom says otherwise.
func runVerbSignedIn(t *testing.T, f *fakeDaemon, instance string, argv ...string) ran {
	t.Helper()
	ready := ipc.Ready{Standing: ipc.StandingStarting, Guilds: []ipc.GuildSummary{}}
	if instance != "" {
		ready = ipc.Ready{Standing: ipc.StandingSignedIn, Guilds: []ipc.GuildSummary{},
			Account: &ipc.Account{InstanceURL: instance, UserID: "1", Username: "alice"}}
	}
	connect := func(context.Context) (daemonclient.Caller, func(), error) {
		return f.Attach(ready, false), func() {}, nil
	}
	var out, errOut bytes.Buffer
	root := &cli.Command{
		Name: "norite", Writer: &out, ErrWriter: &errOut,
		Flags:          []cli.Flag{&cli.BoolFlag{Name: "json"}},
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
		Commands:       Commands(connect),
	}
	err := root.Run(context.Background(), append([]string{"norite"}, argv...))
	return ran{out: out.String(), err: err}
}

// TestAnInviteCarriesItsLink: the link is the instance that answered, /join, and the code after '#', in
// text and in JSON, for a new invite and for every listed one.
func TestAnInviteCarriesItsLink(t *testing.T) {
	const link = "https://chat.example.com/join#BCDFGHJKMNPQRSTV"
	f := newFake(t).
		On("createChannelInvite", created(apiInvite("90", "BCDFGHJKMNPQRSTV"))).
		On("listGuildInvites", ok([]apicontract.GuildInvite{apiInvite("90", "BCDFGHJKMNPQRSTV")}))

	text := runVerbSignedIn(t, f, "https://chat.example.com", "invite", "create", "20")
	require.NoError(t, text.err)
	assert.Contains(t, text.out, "\n  link  "+link+"\n")
	assert.NotContains(t, text.out, "works only here")

	js := runVerbSignedIn(t, f, "https://chat.example.com", "--json", "invite", "create", "20")
	require.NoError(t, js.err)
	matchesCLISchema(t, js.out, "guild-invite.schema.json", "invite")
	var back map[string]any
	require.NoError(t, json.Unmarshal([]byte(js.out), &back))
	assert.Equal(t, link, back["link"])

	list := runVerbSignedIn(t, f, "https://chat.example.com", "--json", "invite", "list", "10")
	require.NoError(t, list.err)
	matchesCLISchema(t, list.out, "guild-invite.schema.json", "inviteList")
	var listed []map[string]any
	require.NoError(t, json.Unmarshal([]byte(list.out), &listed))
	require.Len(t, listed, 1)
	assert.Equal(t, link, listed[0]["link"])

	// The instance that answered, though the daemon was still starting when the verb attached and its READY
	// named none: the link does not depend on when the verb ran.
	starting := newFake(t).SignInFrom(func() string { return "https://chat.example.com" }).
		On("createChannelInvite", created(apiInvite("90", "BCDFGHJKMNPQRSTV")))
	early := runVerbSignedIn(t, starting, "", "--json", "invite", "create", "20")
	require.NoError(t, early.err)
	var earlyBack map[string]any
	require.NoError(t, json.Unmarshal([]byte(early.out), &earlyBack))
	assert.Equal(t, link, earlyBack["link"])

	// No instance said, no link: null in JSON, nothing in text. Only a daemon older than the answer saying
	// which instance answered gives that, signed in all the same.
	older := newFake(t).AsOlderDaemon().On("createChannelInvite", created(apiInvite("90", "BCDFGHJKMNPQRSTV")))
	none := runVerbSignedIn(t, older, "https://chat.example.com", "--json", "invite", "create", "20")
	require.NoError(t, none.err)
	var bare map[string]any
	require.NoError(t, json.Unmarshal([]byte(none.out), &bare))
	v, present := bare["link"]
	assert.True(t, present)
	assert.Nil(t, v)
	plain := runVerbSignedIn(t, older, "https://chat.example.com", "invite", "create", "20")
	require.NoError(t, plain.err)
	assert.NotContains(t, plain.out, "link")

	// A code the instance should not have sent makes no link either, rather than one that ParseInvite
	// refuses and that reads as another code to whoever is handed it.
	odd := newFake(t).On("createChannelInvite", created(apiInvite("90", "AB#CD x")))
	strange := runVerbSignedIn(t, odd, "https://chat.example.com", "--json", "invite", "create", "20")
	require.NoError(t, strange.err)
	var oddBack map[string]any
	require.NoError(t, json.Unmarshal([]byte(strange.out), &oddBack))
	assert.Nil(t, oddBack["link"])
}

// TestALinkToThisMachineAloneSaysSo: an inviter signed in through localhost gets a link that points every
// recipient at their own machine, and is told.
func TestALinkToThisMachineAloneSaysSo(t *testing.T) {
	f := newFake(t).On("createChannelInvite", created(apiInvite("90", "BCDFGHJKMNPQRSTV")))
	r := runVerbSignedIn(t, f, "http://127.0.0.1:8080", "invite", "create", "20")
	require.NoError(t, r.err)
	assert.Contains(t, r.out, "  link  http://127.0.0.1:8080/join#BCDFGHJKMNPQRSTV\n"+
		"  That address reaches this machine alone, so the link works only here. Send the code, with the\n"+
		"  address the others use to reach this instance.\n")
}

// TestALinkSendsItsCodeOnlyToItsOwnInstance: a link's code goes in the body to the instance the link names;
// a link for another instance, or one the daemon cannot be asked about yet, sends nothing at all, since a
// code is a way into a guild and the other instance's operator would hold it.
func TestALinkSendsItsCodeOnlyToItsOwnInstance(t *testing.T) {
	const link = "https://Chat.Example.com/join#bcdf-ghjk-mnpq-rstv"
	f := newFake(t).
		On("previewInvite", ok(apiPreview("BCDFGHJKMNPQRSTV", "Guild"))).
		On("redeemInvite", ok(apiGuild("10", "Guild"))).
		On("revokeInvite", noContent())
	for _, verb := range []string{"show", "join", "revoke"} {
		require.NoError(t, runVerbSignedIn(t, f, "https://chat.example.com", "invite", verb, link).err, verb)
	}
	require.Len(t, f.Requests(), 3)
	for _, call := range f.Requests() {
		assert.JSONEq(t, `{"code":"bcdf-ghjk-mnpq-rstv"}`, string(call.Body), call.Path)
	}

	for _, verb := range []string{"show", "join", "revoke"} {
		g := newFake(t)
		r := runVerbSignedIn(t, g, "https://chat.example.com", "invite", verb,
			"https://other.example/join#BCDFGHJKMNPQRSTV")
		var usage *clierr.UsageError
		require.ErrorAs(t, r.err, &usage, verb)
		assert.Equal(t, "invite "+verb+": that is for https://other.example, and this machine is signed in to "+
			"https://chat.example.com, so nothing was sent", r.err.Error())
		assert.Empty(t, g.Requests(), verb)

		// Signed out: exit 3, and the way on is a sign-in.
		h := newFake(t)
		r = runVerbSignedIn(t, h, "", "invite", verb, "https://chat.example.com/join#BCDFGHJKMNPQRSTV")
		var unavailable *clierr.UnavailableError
		require.ErrorAs(t, r.err, &unavailable, verb)
		assert.Contains(t, r.err.Error(), "norite login", verb)
		assert.Empty(t, h.Requests(), verb)

		// Still starting when the verb attached, and signed in to the link's instance by the time the request
		// is sent: the link is that instance's, and the code goes.
		k := newFake(t).SignInFrom(func() string { return "https://chat.example.com" }).
			On("previewInvite", ok(apiPreview("BCDFGHJKMNPQRSTV", "Guild"))).
			On("redeemInvite", ok(apiGuild("10", "Guild"))).
			On("revokeInvite", noContent())
		require.NoError(t, runVerbSignedIn(t, k, "", "invite", verb,
			"https://chat.example.com/join#BCDFGHJKMNPQRSTV").err, verb)
		assert.Len(t, k.Requests(), 1, verb)
	}
}

// TestWhatIsNeitherALinkNorACodeIsRefusedBeforeAttaching: exit 2, and the daemon never asked.
func TestWhatIsNeitherALinkNorACodeIsRefusedBeforeAttaching(t *testing.T) {
	for _, bad := range []string{"https://chat.example.com/#BCDFGHJKMNPQRSTV", "https://chat.example.com/join?x#BCDF",
		"ftp://chat.example.com/join#BCDF"} {
		attached := false
		connect := func(context.Context) (daemonclient.Caller, func(), error) {
			attached = true
			return newFake(t), func() {}, nil
		}
		root := &cli.Command{
			Name: "norite", Writer: &bytes.Buffer{}, ErrWriter: &bytes.Buffer{},
			Flags:          []cli.Flag{&cli.BoolFlag{Name: "json"}},
			ExitErrHandler: func(context.Context, *cli.Command, error) {},
			Commands:       Commands(connect),
		}
		err := root.Run(context.Background(), []string{"norite", "invite", "join", bad})
		var usage *clierr.UsageError
		require.ErrorAs(t, err, &usage, bad)
		assert.False(t, attached, bad)
	}
}
