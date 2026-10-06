// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package verbs

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/cli/internal/clierr"
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
