// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package verbs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
)

// verbCase is one verb, run once with --json against the contract-checked fake daemon.
type verbCase struct {
	argv    []string
	answers map[string]answerFunc
	// file and def name the definition in contracts/cli-json/ the output must match.
	file, def string
}

// verbCases has one case per leaf of the verb tree. TestEveryVerbHasACase holds it to the tree in both
// directions, so a verb added without one fails, and so does a case for a verb that is gone.
func verbCases() map[string]verbCase {
	return map[string]verbCase{
		"guild list": {argv: []string{"guild", "list"},
			answers: map[string]answerFunc{"listCurrentUserGuilds": ok([]apicontract.Guild{apiGuild("10", "Guild")})},
			file:    "guild.schema.json", def: "guildList"},
		"guild create": {argv: []string{"guild", "create", "--name", "Guild", "--description", "for testing"},
			answers: map[string]answerFunc{"createGuild": created(apiGuild("10", "Guild"))},
			file:    "guild.schema.json", def: "guild"},
		"guild show": {argv: []string{"guild", "show", "10"},
			answers: map[string]answerFunc{"getGuild": ok(apiGuild("10", "Guild"))},
			file:    "guild.schema.json", def: "guild"},
		"guild update": {argv: []string{"guild", "update", "10", "--name", "Renamed", "--recording", "on"},
			answers: map[string]answerFunc{"updateGuild": ok(apiGuild("10", "Renamed"))},
			file:    "guild.schema.json", def: "guild"},
		"guild delete": {argv: []string{"guild", "delete", "10", "--yes"},
			answers: map[string]answerFunc{"deleteGuild": noContent()},
			file:    "common.schema.json", def: "done"},
		"guild transfer": {argv: []string{"guild", "transfer", "10", "2", "--yes"},
			answers: map[string]answerFunc{"transferGuildOwnership": ok(apiGuild("10", "Guild"))},
			file:    "guild.schema.json", def: "guild"},
		"guild audit-log": {argv: []string{"guild", "audit-log", "10", "--limit", "1", "--action", "guild.update"},
			answers: map[string]answerFunc{"listGuildAuditLog": ok([]apicontract.AuditLogEntry{apiAudit("50", "guild.update")})},
			file:    "guild.schema.json", def: "auditPage"},
		"guild recording-log": {argv: []string{"guild", "recording-log", "10"},
			answers: map[string]answerFunc{"listGuildMessageAudit": ok([]apicontract.MessageAuditEntry{apiRecorded("60", "create", "hi")})},
			file:    "guild.schema.json", def: "recordingPage"},

		"channel list": {argv: []string{"channel", "list", "10"},
			answers: map[string]answerFunc{"listGuildChannels": ok([]apicontract.Channel{apiChannel("20", "10", "general", 0)})},
			file:    "channel.schema.json", def: "channelList"},
		"channel create": {argv: []string{"channel", "create", "10", "--name", "general", "--parent", "19", "--topic", "chat"},
			answers: map[string]answerFunc{"createGuildChannel": created(apiChannel("20", "10", "general", 0))},
			file:    "channel.schema.json", def: "channel"},
		"channel update": {argv: []string{"channel", "update", "20", "--clear-topic", "--nsfw=false"},
			answers: map[string]answerFunc{"updateChannel": ok(apiChannel("20", "10", "general", 0))},
			file:    "channel.schema.json", def: "channel"},
		"channel delete": {argv: []string{"channel", "delete", "20", "--yes"},
			answers: map[string]answerFunc{"deleteChannel": noContent()},
			file:    "common.schema.json", def: "done"},

		"role list": {argv: []string{"role", "list", "10"},
			answers: map[string]answerFunc{"listGuildRoles": ok([]apicontract.Role{apiRole("10", "10", "@everyone", 0)})},
			file:    "role.schema.json", def: "roleList"},
		"role create": {argv: []string{"role", "create", "10", "--name", "mods", "--permissions", "8192", "--hoist"},
			answers: map[string]answerFunc{"createGuildRole": created(apiRole("30", "10", "mods", 1))},
			file:    "role.schema.json", def: "role"},
		"role update": {argv: []string{"role", "update", "10", "30", "--color", "255"},
			answers: map[string]answerFunc{"updateGuildRole": ok(apiRole("30", "10", "mods", 1))},
			file:    "role.schema.json", def: "role"},
		"role reorder": {argv: []string{"role", "reorder", "10", "--set", "30=2", "--set", "31=1"},
			answers: map[string]answerFunc{"reorderGuildRoles": ok([]apicontract.Role{apiRole("31", "10", "a", 1), apiRole("30", "10", "b", 2)})},
			file:    "role.schema.json", def: "roleList"},
		"role delete": {argv: []string{"role", "delete", "10", "30", "--yes"},
			answers: map[string]answerFunc{"deleteGuildRole": noContent()},
			file:    "common.schema.json", def: "done"},

		"member list": {argv: []string{"member", "list", "10", "--limit", "2"},
			answers: map[string]answerFunc{"listGuildMembers": ok([]apicontract.Member{apiMember("10", "1"), apiMember("10", "2", "30")})},
			file:    "member.schema.json", def: "memberPage"},
		"member update": {argv: []string{"member", "update", "10", "2", "--nickname", "Bee", "--mute"},
			answers: map[string]answerFunc{"updateGuildMember": ok(apiMember("10", "2"))},
			file:    "member.schema.json", def: "member"},
		"member remove": {argv: []string{"member", "remove", "10", "2", "--yes"},
			answers: map[string]answerFunc{"removeGuildMember": noContent()},
			file:    "common.schema.json", def: "done"},
		"member role add": {argv: []string{"member", "role", "add", "10", "2", "30"},
			answers: map[string]answerFunc{"addGuildMemberRole": ok(apiMember("10", "2", "30"))},
			file:    "member.schema.json", def: "member"},
		"member role remove": {argv: []string{"member", "role", "remove", "10", "2", "30"},
			answers: map[string]answerFunc{"removeGuildMemberRole": ok(apiMember("10", "2"))},
			file:    "member.schema.json", def: "member"},

		"overwrite set": {argv: []string{"overwrite", "set", "20", "30", "--type", "role", "--deny", "2048"},
			answers: map[string]answerFunc{"setChannelPermissionOverwrite": ok(apiOverwrite("20", "30", 0, "0", "2048"))},
			file:    "overwrite.schema.json", def: "overwrite"},
		"overwrite delete": {argv: []string{"overwrite", "delete", "20", "30", "--type", "member"},
			answers: map[string]answerFunc{"deleteChannelPermissionOverwrite": noContent()},
			file:    "common.schema.json", def: "done"},
	}
}

// leaves names every runnable verb in the tree: "guild list", "member role add".
func leaves(cmds []*cli.Command, prefix string) []string {
	var out []string
	for _, c := range cmds {
		name := strings.TrimSpace(prefix + " " + c.Name)
		if len(c.Commands) > 0 {
			out = append(out, leaves(c.Commands, name)...)
			continue
		}
		out = append(out, name)
	}
	return out
}

func TestEveryVerbHasACase(t *testing.T) {
	connect := func(context.Context) (daemonclient.Caller, func(), error) { return nil, nil, errors.New("unused") }
	tree := leaves(Commands(connect), "")
	sort.Strings(tree)
	cases := verbCases()
	for _, v := range tree {
		if _, ok := cases[v]; !ok {
			t.Errorf("the tree has `norite %s` and verbCases has no case for it", v)
		}
	}
	for name := range cases {
		if !contains(tree, name) {
			t.Errorf("verbCases has a case for `norite %s`, which the tree no longer has", name)
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// TestEveryVerbsJSONMatchesItsContract is the done-when's "--json output validates against
// contracts/cli-json/", for every verb, with each request it sends held to openapi.yaml on the way.
func TestEveryVerbsJSONMatchesItsContract(t *testing.T) {
	for name, tc := range verbCases() {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			for op, a := range tc.answers {
				f.on(op, a)
			}
			r := runVerb(t, f, "", append([]string{"--json"}, tc.argv...)...)
			require.NoError(t, r.err)
			require.NotEmpty(t, f.requests(), "the verb asked the instance nothing")
			matchesCLISchema(t, r.out, tc.file, tc.def)
		})
	}
}

// TestEveryVerbAlsoPrintsText runs each case without --json: the same result, drawn for a person.
func TestEveryVerbAlsoPrintsText(t *testing.T) {
	for name, tc := range verbCases() {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			for op, a := range tc.answers {
				f.on(op, a)
			}
			r := runVerb(t, f, "", tc.argv...)
			require.NoError(t, r.err)
			assert.NotEmpty(t, strings.TrimSpace(r.out))
			assert.False(t, json.Valid([]byte(r.out)) && strings.HasPrefix(strings.TrimSpace(r.out), "{"),
				"text, not JSON: %s", r.out)
		})
	}
}

// ---------- the done-when's journey, step by step ----------

func TestAGuildIsCreatedRenamedGivenARoleAndAChannelAnOverwriteWrittenAndItsLogRead(t *testing.T) {
	f := newFake(t).
		on("createGuild", created(apiGuild("10", "Guild"))).
		on("updateGuild", ok(apiGuild("10", "Renamed"))).
		on("createGuildRole", created(apiRole("30", "10", "mods", 1))).
		on("createGuildChannel", created(apiChannel("20", "10", "general", 0))).
		on("setChannelPermissionOverwrite", ok(apiOverwrite("20", "30", 0, "0", "2048"))).
		on("listGuildAuditLog", ok([]apicontract.AuditLogEntry{apiAudit("53", "overwrite.set"), apiAudit("52", "channel.create")}))

	for _, argv := range [][]string{
		{"guild", "create", "--name", "Guild"},
		{"guild", "update", "10", "--name", "Renamed"},
		{"role", "create", "10", "--name", "mods"},
		{"channel", "create", "10", "--name", "general"},
		{"overwrite", "set", "20", "30", "--type", "role", "--deny", "2048"},
		{"guild", "audit-log", "10"},
	} {
		require.NoError(t, runVerb(t, f, "", argv...).err, "%v", argv)
	}

	calls := f.requests()
	require.Len(t, calls, 6)
	assert.JSONEq(t, `{"name":"Guild"}`, string(calls[0].Body))
	assert.JSONEq(t, `{"name":"Renamed"}`, string(calls[1].Body), "only what was passed changes")
	assert.JSONEq(t, `{"name":"mods"}`, string(calls[2].Body))
	assert.JSONEq(t, `{"name":"general","type":0}`, string(calls[3].Body))
	assert.Equal(t, "/channels/20/permissions/30", calls[4].Path)
	assert.JSONEq(t, `{"type":0,"deny":"2048"}`, string(calls[4].Body))
	assert.Equal(t, "50", calls[5].Query.Get("limit"))
}

func TestRecordingIsSwitchedOnAndOffAndItsLogPaged(t *testing.T) {
	f := newFake(t).
		on("updateGuild", func(r request) (int, any) {
			var body map[string]bool
			_ = json.Unmarshal(r.Body, &body)
			g := apiGuild("10", "Guild")
			g.MessageAuditEnabled = body["message_audit_enabled"]
			return http.StatusOK, g
		}).
		on("listGuildMessageAudit", func(r request) (int, any) {
			if r.Query.Get("before") == "" {
				return http.StatusOK, []apicontract.MessageAuditEntry{apiRecorded("62", "edit", "b"), apiRecorded("61", "create", "a")}
			}
			return http.StatusOK, []apicontract.MessageAuditEntry{apiRecorded("60", "create", "first")}
		})

	on := runVerb(t, f, "", "--json", "guild", "update", "10", "--recording", "on")
	require.NoError(t, on.err)
	assert.Contains(t, on.out, `"recording": true`)
	off := runVerb(t, f, "", "--json", "guild", "update", "10", "--recording", "off")
	require.NoError(t, off.err)
	assert.Contains(t, off.out, `"recording": false`)

	first := runVerb(t, f, "", "--json", "guild", "recording-log", "10", "--limit", "2")
	require.NoError(t, first.err)
	var page struct {
		Items []map[string]any `json:"items"`
		Next  *string          `json:"next"`
	}
	require.NoError(t, json.Unmarshal([]byte(first.out), &page))
	require.NotNil(t, page.Next, "a full page says where the next one starts")
	assert.Equal(t, "61", *page.Next)

	second := runVerb(t, f, "", "--json", "guild", "recording-log", "10", "--limit", "2", "--before", *page.Next)
	require.NoError(t, second.err)
	require.NoError(t, json.Unmarshal([]byte(second.out), &page))
	assert.Nil(t, page.Next, "a short page is the last")

	calls := f.requests()
	assert.JSONEq(t, `{"message_audit_enabled":true}`, string(calls[0].Body))
	assert.JSONEq(t, `{"message_audit_enabled":false}`, string(calls[1].Body))
	assert.Equal(t, "61", calls[3].Query.Get("before"))
}

// TestANonMembersRefusalIsAnAnswerNotACrash is the done-when's last clause: the instance's 404 for a guild
// somebody is not in comes back as clierr.RefusedError, which main exits 4 for, without the crash prefix.
func TestANonMembersRefusalIsAnAnswerNotACrash(t *testing.T) {
	f := newFake(t).on("getGuild", func(request) (int, any) {
		return http.StatusNotFound, refusal("not_found", "not found")
	})
	r := runVerb(t, f, "", "guild", "show", "10")
	var refused *clierr.RefusedError
	require.ErrorAs(t, r.err, &refused)
	assert.Equal(t, http.StatusNotFound, refused.Status)
	assert.Equal(t, "guild show: not found (request req-1)", r.err.Error())
	assert.Empty(t, r.out)
}

func TestADestructiveVerbWithoutYesAndNoTerminalTouchesNothing(t *testing.T) {
	for _, argv := range [][]string{
		{"guild", "delete", "10"}, {"guild", "transfer", "10", "2"}, {"channel", "delete", "20"},
		{"role", "delete", "10", "30"}, {"member", "remove", "10", "2"},
	} {
		f := newFake(t)
		r := runVerb(t, f, "y\n", argv...)
		require.True(t, errors.Is(r.err, clierr.ErrNoTerminal), "%v: %v", argv, r.err)
		assert.Contains(t, r.err.Error(), "--yes")
		assert.Empty(t, f.requests(), "%v asked the instance something", argv)
	}
}

func TestAnUpdateWithNothingOrContradictionsIsAUsageError(t *testing.T) {
	for _, argv := range [][]string{
		{"guild", "update", "10"},
		{"guild", "update", "10", "--description", "x", "--clear-description"},
		{"guild", "update", "10", "--recording", "maybe"},
		{"channel", "update", "20"},
		{"channel", "update", "20", "--topic", "x", "--clear-topic"},
		{"role", "update", "10", "30"},
		{"member", "update", "10", "2"},
		{"guild", "create"},
		{"channel", "create", "10", "--name", "x", "--type", "forum"},
		{"role", "create", "10", "--name", "x", "--permissions", "all"},
		{"role", "reorder", "10"},
		{"role", "reorder", "10", "--set", "30=-1"},
		{"overwrite", "set", "20", "30"},
		{"guild", "audit-log", "10", "--actor", "me"},
		{"member", "list", "10", "--limit", "500"},
	} {
		f := newFake(t)
		r := runVerb(t, f, "", argv...)
		var usage *clierr.UsageError
		require.ErrorAs(t, r.err, &usage, "%v: %v", argv, r.err)
		assert.Empty(t, f.requests(), "%v", argv)
	}
}

// TestAHostileNameIsInertInTextAndExactInJSON: rule 19 in both presentations, on a name an instance chose.
func TestAHostileNameIsInertInTextAndExactInJSON(t *testing.T) {
	hostile := "Evil\x1b]0;owned\x07 \u202eesrever"
	f := newFake(t).on("listCurrentUserGuilds", ok([]apicontract.Guild{apiGuild("10", hostile)}))

	text := runVerb(t, f, "", "guild", "list")
	require.NoError(t, text.err)
	assert.NotContains(t, text.out, "\x1b")
	assert.NotContains(t, text.out, "\u202e")

	js := runVerb(t, f, "", "--json", "guild", "list")
	require.NoError(t, js.err)
	assert.NotContains(t, js.out, "\u202e")
	var back []map[string]any
	require.NoError(t, json.Unmarshal([]byte(js.out), &back))
	assert.Equal(t, hostile, back[0]["name"], "lossless")
}
