// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package verbs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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
	"github.com/Alexnex31/Norite/cli/internal/output"
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

		"overwrite set": {argv: []string{"overwrite", "set", "20", "30", "--type", "role", "--allow", "0", "--deny", "2048"},
			answers: map[string]answerFunc{"setChannelPermissionOverwrite": ok(apiOverwrite("20", "30", 0, "0", "2048"))},
			file:    "overwrite.schema.json", def: "overwrite"},
		"overwrite delete": {argv: []string{"overwrite", "delete", "20", "30", "--type", "member", "--yes"},
			answers: map[string]answerFunc{"deleteChannelPermissionOverwrite": noContent()},
			file:    "common.schema.json", def: "done"},

		"message list": {argv: []string{"message", "list", "20", "--limit", "1"},
			answers: map[string]answerFunc{"listChannelMessages": ok([]apicontract.Message{apiMessage("30", "20", "hello")})},
			file:    "message.schema.json", def: "messagePage"},
		"message send": {argv: []string{"message", "send", "20", "--content", "hello", "--reply-to", "29"},
			answers: map[string]answerFunc{"sendMessage": created(apiMessage("30", "20", "hello"))},
			file:    "message.schema.json", def: "message"},
		"message edit": {argv: []string{"message", "edit", "20", "30", "--content", "hello, corrected"},
			answers: map[string]answerFunc{"updateMessage": ok(apiMessage("30", "20", "hello, corrected"))},
			file:    "message.schema.json", def: "message"},
		"message delete": {argv: []string{"message", "delete", "20", "30", "--yes"},
			answers: map[string]answerFunc{"deleteMessage": noContent()},
			file:    "common.schema.json", def: "done"},
		"message history": {argv: []string{"message", "history", "20", "30"},
			answers: map[string]answerFunc{"getMessageEditHistory": ok(apiHistory("30", "20", "before"))},
			file:    "message.schema.json", def: "history"},

		"report file": {argv: []string{"report", "file", "30", "--reason", "spam", "--detail", "look"},
			answers: map[string]answerFunc{"fileReport": created(apiReport("70", "open"))},
			file:    "report.schema.json", def: "report"},
		"report list": {argv: []string{"report", "list", "10", "--status", "open"},
			answers: map[string]answerFunc{"listGuildReports": ok([]apicontract.TriageReport{apiTriage("70")})},
			file:    "report.schema.json", def: "triagePage"},
		"report show": {argv: []string{"report", "show", "10", "70"},
			answers: map[string]answerFunc{"getGuildReport": ok(apiTriageDetail("70"))},
			file:    "report.schema.json", def: "triageDetail"},
		"report resolve": {argv: []string{"report", "resolve", "10", "70"},
			answers: map[string]answerFunc{"resolveGuildReport": ok(apiReport("70", "resolved"))},
			file:    "report.schema.json", def: "report"},
		"report dismiss": {argv: []string{"report", "dismiss", "10", "70"},
			answers: map[string]answerFunc{"resolveGuildReport": ok(apiReport("70", "dismissed"))},
			file:    "report.schema.json", def: "report"},

		"tag list": {argv: []string{"tag", "list", "10"},
			answers: map[string]answerFunc{"listGuildMessageTags": ok([]apicontract.MessageTag{apiTag("80", "todo", false)})},
			file:    "tag.schema.json", def: "tagList"},
		"tag create": {argv: []string{"tag", "create", "10", "--name", "todo", "--shared"},
			answers: map[string]answerFunc{"createMessageTag": created(apiTag("80", "todo", true))},
			file:    "tag.schema.json", def: "tag"},
		"tag delete": {argv: []string{"tag", "delete", "10", "80", "--yes"},
			answers: map[string]answerFunc{"deleteMessageTag": noContent()},
			file:    "common.schema.json", def: "done"},
		"tag apply": {argv: []string{"tag", "apply", "21", "30", "80"},
			answers: map[string]answerFunc{"applyMessageTag": noContent()},
			file:    "common.schema.json", def: "done"},
		"tag unapply": {argv: []string{"tag", "unapply", "21", "30", "80"},
			answers: map[string]answerFunc{"unapplyMessageTag": noContent()},
			file:    "common.schema.json", def: "done"},
		"tag on": {argv: []string{"tag", "on", "21", "30"},
			answers: map[string]answerFunc{"listMessageTags": ok([]apicontract.AppliedMessageTag{apiApplied("80", "todo")})},
			file:    "tag.schema.json", def: "appliedTagList"},
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
		{"overwrite", "set", "20", "30", "--type", "role", "--allow", "0", "--deny", "2048"},
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
	assert.JSONEq(t, `{"type":0,"allow":"0","deny":"2048"}`, string(calls[4].Body))
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
		{"overwrite", "delete", "20", "30", "--type", "role"},
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
		// Half an overwrite would blank the other half: a mute's deny, lifted by adding an allow.
		{"overwrite", "set", "20", "30", "--type", "role", "--allow", "1024"},
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

func TestAReportIsFiledAndTriagedAndTheReporterIsNeverShown(t *testing.T) {
	f := newFake(t).
		on("fileReport", created(apiReport("70", "open"))).
		on("listGuildReports", ok([]apicontract.TriageReport{apiTriage("70")})).
		on("getGuildReport", ok(apiTriageDetail("70"))).
		on("resolveGuildReport", func(r request) (int, any) {
			var body struct{ Status string }
			_ = json.Unmarshal(r.Body, &body)
			return http.StatusOK, apiReport("70", body.Status)
		})

	require.NoError(t, runVerb(t, f, "", "report", "file", "30", "--reason", "harassment").err)
	for _, argv := range [][]string{
		{"report", "list", "10"}, {"report", "show", "10", "70"}, {"report", "resolve", "10", "70"},
	} {
		for _, asJSON := range []bool{true, false} {
			args := argv
			if asJSON {
				args = append([]string{"--json"}, argv...)
			}
			r := runVerb(t, f, "", args...)
			require.NoError(t, r.err, "%v", args)
			assert.NotContains(t, strings.ToLower(r.out), "reporter", "%v names a reporter", args)
		}
	}

	calls := f.requests()
	assert.JSONEq(t, `{"target_type":"message","target_id":"30","reason_category":"harassment"}`, string(calls[0].Body))
	assert.JSONEq(t, `{"status":"resolved"}`, string(calls[len(calls)-1].Body))

	dismissed := runVerb(t, f, "", "--json", "report", "dismiss", "10", "70")
	require.NoError(t, dismissed.err)
	assert.Contains(t, dismissed.out, `"status": "dismissed"`, "dismiss is resolve with the other outcome")
}

func TestABacklogIsReadPostedToEditedAndDeletedAndAMessagesVersionsRead(t *testing.T) {
	f := newFake(t).
		on("listChannelMessages", ok([]apicontract.Message{apiMessage("31", "20", "newer"), apiMessage("30", "20", "older")})).
		on("sendMessage", created(apiMessage("32", "20", "line one\nline two"))).
		on("updateMessage", ok(apiMessage("32", "20", "corrected"))).
		on("deleteMessage", noContent()).
		on("getMessageEditHistory", ok(apiHistory("32", "20", "line one\nline two")))

	page := runVerb(t, f, "", "--json", "message", "list", "20", "--limit", "2")
	require.NoError(t, page.err)
	assert.Contains(t, page.out, `"next": "30"`, "the oldest id, for --before")

	require.NoError(t, runVerb(t, f, "line one\nline two\n", "message", "send", "20", "--content", "-").err)
	require.NoError(t, runVerb(t, f, "", "message", "edit", "20", "32", "--content", "corrected").err)
	history := runVerb(t, f, "", "message", "history", "20", "32")
	require.NoError(t, history.err)
	assert.Contains(t, history.out, "line two")
	require.NoError(t, runVerb(t, f, "", "message", "delete", "20", "32", "--yes").err)

	calls := f.requests()
	assert.JSONEq(t, `{"content":"line one\nline two"}`, string(calls[1].Body), "read from stdin, the final newline dropped")
	assert.JSONEq(t, `{"content":"corrected"}`, string(calls[2].Body))
	assert.Equal(t, "DELETE", calls[4].Method)
}

// TestATagIsAppliedToAMessageInAnotherChannelAndRemoved: the tag is the guild's, the message is reached
// through its own channel — 21 here, where the tag was listed through the guild.
func TestATagIsAppliedToAMessageInAnotherChannelAndRemoved(t *testing.T) {
	f := newFake(t).
		on("createMessageTag", created(apiTag("80", "todo", true))).
		on("applyMessageTag", noContent()).
		on("unapplyMessageTag", noContent())

	require.NoError(t, runVerb(t, f, "", "tag", "create", "10", "--name", "todo", "--shared").err)
	applied := runVerb(t, f, "", "--json", "tag", "apply", "21", "30", "80")
	require.NoError(t, applied.err)
	assert.JSONEq(t, `{"action":"tag.apply","target":{"channel_id":"21","message_id":"30","tag_id":"80"}}`, applied.out)
	require.NoError(t, runVerb(t, f, "", "tag", "unapply", "21", "30", "80").err)

	calls := f.requests()
	assert.Equal(t, "/channels/21/messages/30/tags/80", calls[1].Path)
	assert.Equal(t, http.MethodPut, calls[1].Method)
	assert.Equal(t, http.MethodDelete, calls[2].Method)
}

func TestAMessageMustHaveContentWithinTheLimit(t *testing.T) {
	for _, tc := range []struct {
		stdin string
		argv  []string
	}{
		{"", []string{"message", "send", "20"}},
		{"", []string{"message", "send", "20", "--content", "   "}},
		{"\n\n", []string{"message", "send", "20", "--content", "-"}},
		{"", []string{"message", "send", "20", "--content", strings.Repeat("日", maxContent+1)}},
		{"", []string{"report", "file", "30", "--reason", "rude"}},
	} {
		f := newFake(t)
		r := runVerb(t, f, tc.stdin, tc.argv...)
		var usage *clierr.UsageError
		require.ErrorAs(t, r.err, &usage, "%v", tc.argv)
		assert.Empty(t, f.requests())
	}

	// Counted in runes, as the instance counts them: 4,000 Japanese characters are 12,000 bytes and fit.
	f := newFake(t).on("sendMessage", created(apiMessage("30", "20", "x")))
	require.NoError(t, runVerb(t, f, "", "message", "send", "20", "--content", strings.Repeat("日", maxContent)).err)
}

// TestAUsageErrorNeedsNoDaemon: every flag check and every confirmation happens before the verb attaches,
// so a mistake is exit 2 on a machine with no daemon running, never exit 3 (M20 /code-review).
func TestAUsageErrorNeedsNoDaemon(t *testing.T) {
	noDaemon := func(context.Context) (daemonclient.Caller, func(), error) {
		return nil, nil, clierr.Unavailable("the daemon is not running")
	}
	for _, argv := range [][]string{
		{"message", "send", "1"},
		{"message", "list", "1", "--limit", "0"},
		{"guild", "update", "10"},
		{"guild", "audit-log", "10", "--actor", "me"},
		{"report", "file", "30", "--reason", "rude"},
		{"overwrite", "set", "20", "30"},
	} {
		root := &cli.Command{
			Name: "norite", Writer: io.Discard, ErrWriter: io.Discard, Reader: strings.NewReader(""),
			Flags:          []cli.Flag{&cli.BoolFlag{Name: "json"}},
			ExitErrHandler: func(context.Context, *cli.Command, error) {},
			Commands:       Commands(noDaemon),
		}
		err := root.Run(context.Background(), append([]string{"norite"}, argv...))
		var usage *clierr.UsageError
		assert.ErrorAs(t, err, &usage, "%v: %v", argv, err)
	}

	// The confirmation too: no terminal and no --yes is ErrNoTerminal, exit 2, before anything attaches.
	root := &cli.Command{
		Name: "norite", Writer: io.Discard, ErrWriter: io.Discard, Reader: strings.NewReader(""),
		Flags: []cli.Flag{&cli.BoolFlag{Name: "json"}}, ExitErrHandler: func(context.Context, *cli.Command, error) {},
		Commands: Commands(noDaemon),
	}
	err := root.Run(context.Background(), []string{"norite", "guild", "delete", "1"})
	assert.True(t, errors.Is(err, clierr.ErrNoTerminal), "got %v", err)
}

// TestTheConfirmationIsAskedOnStderr: stdout carries the result, which --json pipes into a parser.
func TestTheConfirmationIsAskedOnStderr(t *testing.T) {
	f := newFake(t).on("deleteGuild", noContent())
	connect := func(context.Context) (daemonclient.Caller, func(), error) { return f, func() {}, nil }
	var out, errOut bytes.Buffer
	root := &cli.Command{
		Name: "norite", Writer: &out, ErrWriter: &errOut, Reader: strings.NewReader("y\n"),
		Flags: []cli.Flag{&cli.BoolFlag{Name: "json"}}, ExitErrHandler: func(context.Context, *cli.Command, error) {},
		Commands: []*cli.Command{guildCommand(connect)},
	}
	// The env sees a non-terminal reader, so the question is forced the way a terminal would see it.
	root.Commands[0].Commands[4].Action = run(connect, func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
		e.interactive = true
		if err := confirm(cmd, e, "delete guild 10"); err != nil {
			return nil, err
		}
		return done{Action: "guild.delete", Target: map[string]string{"guild_id": "10"}}, nil
	})
	require.NoError(t, root.Run(context.Background(), []string{"norite", "--json", "guild", "delete", "10"}))
	assert.Contains(t, errOut.String(), "[y/N]")
	assert.NotContains(t, out.String(), "[y/N]")
	assert.True(t, json.Valid(out.Bytes()), "stdout is only the document: %s", out.String())
}
