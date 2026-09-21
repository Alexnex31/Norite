// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/guilds"
	"github.com/Alexnex31/Norite/backend/internal/messages"
)

// Two tests in this package claim to enumerate the guild route surface, and until M14 neither asked the
// router what that surface was. Both iterated a slice somebody typed.
//
// That is worse than it sounds for the anti-enumeration one: a missing entry means a mutating route was
// never checked for the property, on a test whose entire stated purpose is that this cannot happen. During
// M13 it needed routes adding twice — the overwrite pair, then the role reorder — and a person noticed
// both times. The audit-coverage one had drifted further: ten of the sixteen actions.
//
// # The walk is not the load-bearing part
//
// A walk that skips routes it has no case for is the hand-written table again wearing a mechanism's
// clothes. What makes this work is [requireCasesMatchTheSurface], which fails in *both* directions: a
// route with no case, and a case naming a route the router no longer serves. The second half matters as
// much — a stale case for a deleted route is a test asserting something about nothing, and it looks
// exactly like coverage.
//
// A route that genuinely should not be exercised declares itself exempt with a reason, which is a decision
// a reader can disagree with rather than an absence nobody can see.

// guildSurfaceRoutes returns every route the real router serves under /guilds or /channels, as
// "METHOD /pattern" with chi's path parameters intact.
//
// Built from newTestRouterWithAuth, which constructs the router with nil services and therefore needs no
// database — the route table is a property of the code, not of any instance.
func guildSurfaceRoutes(t *testing.T) []string {
	t.Helper()

	var out []string
	for op := range routedOperations(t, newTestRouterWithAuth(t)) {
		_, route, _ := strings.Cut(op, " ")
		// /reports is here because filing is top-level — the target vocabulary spans objects with no
		// guild — and a route outside these prefixes is one neither test below can see. M15 shipped the
		// message routes invisible to both for a version of this reason; the prefix list is the other
		// half of that lesson.
		if strings.HasPrefix(route, "/api/v1/guilds") ||
			strings.HasPrefix(route, "/api/v1/channels") ||
			strings.HasPrefix(route, "/api/v1/reports") {
			out = append(out, op)
		}
	}
	sort.Strings(out)

	// A surface that came back empty would make every test below vacuously pass, which is the failure mode
	// this whole file exists to remove.
	require.NotEmpty(t, out, "the walk found no guild routes; newRouter or the prefixes above changed")
	return out
}

// requireCasesMatchTheSurface fails when the walked routes and a test's cases disagree either way.
func requireCasesMatchTheSurface[T any](t *testing.T, routes []string, cases map[string]T) {
	t.Helper()

	for _, route := range routes {
		if _, ok := cases[route]; !ok {
			t.Errorf("the router serves %s and this test has no case for it — add one, or mark it "+
				"exempt with the reason it is not covered here", route)
		}
	}

	known := make(map[string]struct{}, len(routes))
	for _, route := range routes {
		known[route] = struct{}{}
	}
	for route := range cases {
		if _, ok := known[route]; !ok {
			t.Errorf("this test has a case for %s and the router serves no such route — a case for a "+
				"route that no longer exists asserts nothing and reads as coverage", route)
		}
	}
}

var routePathParam = regexp.MustCompile(`\{[a-z_]+\}`)

// fillRoute substitutes real ids into a chi pattern, failing on a parameter it has no value for.
//
// The failure is the point: a route introducing a new path parameter arrives here as a hard stop naming
// the parameter, rather than as a request to a literal "{thing_id}" that answers 404 for the wrong reason
// and passes an anti-enumeration assertion by accident.
func fillRoute(t *testing.T, pattern string, ids map[string]string) string {
	t.Helper()

	filled := routePathParam.ReplaceAllStringFunc(pattern, func(param string) string {
		value, ok := ids[param]
		require.Truef(t, ok, "route %s uses %s and this test has no value for it", pattern, param)
		return value
	})
	require.NotContains(t, filled, "{", "route %s was not fully substituted: %s", pattern, filled)
	return filled
}

// refusalCase is one route as the anti-enumeration test exercises it.
type refusalCase struct {
	// exempt, when non-empty, says why a non-member is not refused here.
	exempt string
	body   any
}

// TestEveryGuildRouteRefusesANonMember is the structural test, now derived from the router.
//
// Every route the guild handler serves is exercised with an actor who is in no guild, and must answer 404.
// A handler added later that assembles its own permission check instead of calling authorize fails here —
// which is the point, since authorize being the only path to a decision is a property no single-endpoint
// test can assert.
//
// 404 rather than 403 throughout: a non-member must not learn the guild exists. Guild ids are snowflakes,
// so a 404/403 split turns a list of plausible ids into a map of what exists on the instance.
//
// **Widened from non-GET to every method**, which is not merely thoroughness. The reads disclose the same
// thing the writes do — a channel list, a member list, an audit log — and the audit-log route added by this
// milestone would have been covered by this test the moment it mounted rather than by remembering to add
// it. The old name said "Mutating"; the reason for the limit was that mutations were where authorize was
// being forgotten, and that is an argument about likelihood, not about what the property covers.
func TestEveryGuildRouteRefusesANonMember(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)

	channel := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/channels", f.guildID),
		map[string]any{"name": "general", "type": 0}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, channel.Code, channel)

	role := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID),
		map[string]any{"name": "mods"}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, role.Code, role)
	roleID := role.field(t, "id")

	// A real message, so the message routes are exercised against something that exists. A fabricated id
	// would answer 404 for the wrong reason and pass the anti-enumeration assertion by accident — the
	// failure fillRoute's comment describes, one level up.
	message := f.api.call(http.MethodPost, "/api/v1/channels/"+channel.field(t, "id")+"/messages",
		map[string]any{"content": "hello"}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, message.Code, "seeding a message: %s", message)

	// A real report, so the triage routes are exercised against something that exists — same argument as
	// the message above. The owner reports their own message, which is pointless in production and is the
	// cheapest way to get a row here.
	report := f.api.call(http.MethodPost, "/api/v1/reports", map[string]any{
		"target_type": "message", "target_id": message.field(t, "id"), "reason_category": "spam",
	}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, report.Code, "seeding a report: %s", report)

	ids := map[string]string{
		"{guild_id}":     f.guildID,
		"{channel_id}":   channel.field(t, "id"),
		"{role_id}":      roleID,
		"{user_id}":      f.memberID,
		"{overwrite_id}": roleID,
		"{message_id}":   message.field(t, "id"),
		"{report_id}":    report.field(t, "id"),
	}

	cases := map[string]refusalCase{
		"POST /api/v1/guilds": {
			exempt: "creating a guild is the one mutation with no guild to be a non-member of; " +
				"anyone who can authenticate may call it, and TestAGuildIsBoundedInChannelsAndRoles " +
				"covers what bounds it",
		},

		// The message routes. A stranger must not learn a channel exists, and guildauth answers 404 for
		// a non-member before any message is loaded — so these assert the same property the guild routes
		// do, one level down.
		"GET /api/v1/channels/{channel_id}/messages":                 {},
		"POST /api/v1/channels/{channel_id}/messages":                {body: map[string]any{"content": "intruding"}},
		"PATCH /api/v1/channels/{channel_id}/messages/{message_id}":  {body: map[string]any{"content": "hijacked"}},
		"DELETE /api/v1/channels/{channel_id}/messages/{message_id}": {},
		// M16a. A stranger must be refused here exactly as they are on the backlog — the route authorizes
		// without the PermViewChannel fold, which lifts a *requirement* and must not lift the refusal.
		"GET /api/v1/channels/{channel_id}/messages/{message_id}/history": {},

		// The report routes. Filing is the interesting one: a stranger naming a real message id must be
		// refused exactly as though it did not exist, because whether that id names a message in a channel
		// they cannot see is what the channel filter withholds.
		"POST /api/v1/reports": {body: map[string]any{
			"target_type": "message", "target_id": message.field(t, "id"), "reason_category": "spam",
		}},
		"GET /api/v1/guilds/{guild_id}/reports":             {},
		"GET /api/v1/guilds/{guild_id}/reports/{report_id}": {},
		"POST /api/v1/guilds/{guild_id}/reports/{report_id}/resolve": {
			body: map[string]any{"status": "dismissed"},
		},

		"GET /api/v1/guilds/{guild_id}":                    {},
		"PATCH /api/v1/guilds/{guild_id}":                  {body: map[string]any{"name": "hijacked"}},
		"DELETE /api/v1/guilds/{guild_id}":                 {},
		"GET /api/v1/guilds/{guild_id}/channels":           {},
		"GET /api/v1/guilds/{guild_id}/roles":              {},
		"GET /api/v1/guilds/{guild_id}/members":            {},
		"GET /api/v1/guilds/{guild_id}/audit-log":          {},
		"POST /api/v1/guilds/{guild_id}/channels":          {body: map[string]any{"name": "x", "type": 0}},
		"POST /api/v1/guilds/{guild_id}/roles":             {body: map[string]any{"name": "x"}},
		"PATCH /api/v1/guilds/{guild_id}/roles":            {body: map[string]any{"roles": []map[string]any{{"id": roleID, "position": 1}}}},
		"PATCH /api/v1/guilds/{guild_id}/roles/{role_id}":  {body: map[string]any{"name": "hijacked"}},
		"DELETE /api/v1/guilds/{guild_id}/roles/{role_id}": {},
		"PATCH /api/v1/guilds/{guild_id}/members/{user_id}": {
			body: map[string]any{"nickname": "hijacked"},
		},
		"DELETE /api/v1/guilds/{guild_id}/members/{user_id}":                 {},
		"PUT /api/v1/guilds/{guild_id}/members/{user_id}/roles/{role_id}":    {},
		"DELETE /api/v1/guilds/{guild_id}/members/{user_id}/roles/{role_id}": {},
		"PATCH /api/v1/channels/{channel_id}":                                {body: map[string]any{"name": "hijacked"}},
		"DELETE /api/v1/channels/{channel_id}":                               {},
		"PUT /api/v1/channels/{channel_id}/permissions/{overwrite_id}":       {body: map[string]any{"type": 0, "allow": "0", "deny": "0"}},
		"DELETE /api/v1/channels/{channel_id}/permissions/{overwrite_id}":    {body: map[string]any{"type": 0}},
	}

	routes := guildSurfaceRoutes(t)
	requireCasesMatchTheSurface(t, routes, cases)

	for _, route := range routes {
		tc := cases[route]
		if tc.exempt != "" {
			continue
		}

		method, pattern, _ := strings.Cut(route, " ")
		t.Run(route, func(t *testing.T) {
			resp := f.api.call(method, fillRoute(t, pattern, ids), tc.body, withToken(f.strangerToken))
			require.Equal(t, http.StatusNotFound, resp.Code,
				"a non-member must be refused, and must not learn the guild exists: %s", resp)
		})
	}
}

// auditCase is one route as the audit-coverage test exercises it.
//
// Ordered, not mapped, because the sequence is load-bearing: a role must exist before it can be assigned
// and must be assigned before it can be unassigned, and every delete has to come after everything that
// needs the thing it deletes. The map the coverage check wants is derived from the slice.
type auditCase struct {
	route  string
	action string
	body   any
	want   int
	// exempt, when non-empty, says why this route writes no entry the test can count.
	exempt string
}

// TestEveryGuildMutationWritesExactlyOneAuditEntry is rule 2 over the whole route surface.
//
// Every mutating route must write exactly one audit entry naming who did it, in the same transaction —
// the transactional half is TestTheAuditEntryAndTheMutationShareATransaction, this is the "exactly one,
// for every route" half.
//
// It iterated a hand-written table of ten until M14, against sixteen actions and twenty-one routes. The
// six it never reached were role.reorder, member.role_add, member.role_remove, overwrite.set,
// overwrite.delete and guild.delete — which is to say the whole of M13's surface, on the test whose job is
// to notice a mutation that forgot to write one. Now derived from the router, so a route added without a
// case fails rather than being quietly uncounted.
func TestEveryGuildMutationWritesExactlyOneAuditEntry(t *testing.T) {
	t.Parallel()

	f := newGuildFixture(t)
	guildID := mustID(t, f.guildID)

	channel := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/channels", f.guildID),
		map[string]any{"name": "general", "type": 0}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, channel.Code, channel)
	channelID := channel.field(t, "id")

	role := f.api.call(http.MethodPost, fmt.Sprintf("/api/v1/guilds/%s/roles", f.guildID),
		map[string]any{"name": "mods"}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, role.Code, role)
	roleID := role.field(t, "id")

	// A message and a report against it, so the one report route that *does* write an entry has something
	// to close. Both are the owner's own, which writes nothing itself — posting and filing are outside
	// rule 2's narrowed scope — so neither disturbs the counts below.
	message := f.api.call(http.MethodPost, "/api/v1/channels/"+channelID+"/messages",
		map[string]any{"content": "hello"}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, message.Code, "seeding a message: %s", message)

	report := f.api.call(http.MethodPost, "/api/v1/reports", map[string]any{
		"target_type": "message", "target_id": message.field(t, "id"), "reason_category": "spam",
	}, withToken(f.ownerToken))
	require.Equal(t, http.StatusCreated, report.Code, "seeding a report: %s", report)

	ids := map[string]string{
		"{guild_id}":     f.guildID,
		"{channel_id}":   channelID,
		"{role_id}":      roleID,
		"{user_id}":      f.memberID,
		"{overwrite_id}": roleID,
		"{message_id}":   message.field(t, "id"),
		"{report_id}":    report.field(t, "id"),
	}

	const readsWriteNothing = "a GET writes no audit entry — rule 4 keeps reads side-effect-free"

	// In dependency order. The three already performed above carry no request.
	sequence := []auditCase{
		{route: "POST /api/v1/guilds", action: "guild.create",
			exempt: "the fixture created this guild through this route; its entry is counted below"},
		{route: "POST /api/v1/guilds/{guild_id}/channels", action: "channel.create",
			exempt: "performed above, so the channel the later cases name exists"},
		{route: "POST /api/v1/guilds/{guild_id}/roles", action: "role.create",
			exempt: "performed above, so the role the later cases name exists"},

		// The message routes, and all four are exempt for a reason this milestone introduced rather than
		// for the usual one. Rule 2 was narrowed at M15 to guild-scoped *administrative* mutations, and
		// message content is outside it: a member posting, editing or deleting their own message
		// exercises authority over nobody, so three of these are mutations that deliberately write no
		// audit entry at all. This test's model — every mutating route writes exactly one — stops holding
		// here, and saying so out loud is better than the alternative of making them write entries to
		// satisfy a test.
		//
		// The one case that *does* write an entry is a moderator deleting somebody else's message, which
		// needs a second actor this fixture's sequence has no room for. It is asserted in the messages
		// package, where both actors are cheap to build, by
		// TestAModeratorDeletingSomebodyElsesMessageIsAudited — and its counterpart asserts that an author
		// deleting their own writes nothing, which is the property with no other home.
		{route: "GET /api/v1/channels/{channel_id}/messages", exempt: readsWriteNothing},
		{route: "GET /api/v1/channels/{channel_id}/messages/{message_id}/history",
			exempt: readsWriteNothing},
		{route: "POST /api/v1/channels/{channel_id}/messages",
			exempt: "sending is not administrative (rule 2, narrowed at M15) and writes no entry"},
		{route: "PATCH /api/v1/channels/{channel_id}/messages/{message_id}",
			exempt: "only an author may edit, which is authority over nobody; writes no entry"},
		{route: "DELETE /api/v1/channels/{channel_id}/messages/{message_id}",
			exempt: "an author deleting their own writes nothing; the moderator path that does is " +
				"asserted in the messages package, which can build two actors"},

		// The report routes. Filing is exempt for M15's reason rather than a new one: a member filing
		// exercises authority over nobody, so rule 2's narrowed wording does not reach it — and auditing
		// it would put an unbounded write to this table within reach of @everyone, which is the specific
		// hazard that narrowing was about.
		//
		// Closing one is the opposite: it is a decision taken about somebody else's report, by somebody
		// exercising a moderation permission, and it is the case this test is for.
		{route: "POST /api/v1/reports",
			exempt: "performed above; filing is not administrative (rule 2, narrowed at M15)"},
		{route: "GET /api/v1/guilds/{guild_id}/reports", exempt: readsWriteNothing},
		{route: "GET /api/v1/guilds/{guild_id}/reports/{report_id}", exempt: readsWriteNothing},

		{route: "GET /api/v1/guilds/{guild_id}", exempt: readsWriteNothing},
		{route: "GET /api/v1/guilds/{guild_id}/channels", exempt: readsWriteNothing},
		{route: "GET /api/v1/guilds/{guild_id}/roles", exempt: readsWriteNothing},
		{route: "GET /api/v1/guilds/{guild_id}/members", exempt: readsWriteNothing},
		{route: "GET /api/v1/guilds/{guild_id}/audit-log", exempt: readsWriteNothing},

		{route: "PATCH /api/v1/guilds/{guild_id}", action: "guild.update",
			body: map[string]any{"name": "Renamed"}, want: http.StatusOK},
		{route: "PATCH /api/v1/channels/{channel_id}", action: "channel.update",
			body: map[string]any{"name": "renamed"}, want: http.StatusOK},
		{route: "PATCH /api/v1/guilds/{guild_id}/roles/{role_id}", action: "role.update",
			body: map[string]any{"name": "renamed"}, want: http.StatusOK},
		{route: "PATCH /api/v1/guilds/{guild_id}/roles", action: "role.reorder",
			body: map[string]any{"roles": []map[string]any{{"id": roleID, "position": 1}}},
			want: http.StatusOK},

		// The one report route that writes an entry. `report.dismiss` is the verb its sibling would
		// produce; both are covered for shape in the reports package, and this asserts the "exactly one,
		// on this route" half the others cannot.
		{route: "POST /api/v1/guilds/{guild_id}/reports/{report_id}/resolve", action: "report.resolve",
			body: map[string]any{"status": "resolved"}, want: http.StatusOK},

		{route: "PUT /api/v1/channels/{channel_id}/permissions/{overwrite_id}", action: "overwrite.set",
			body: map[string]any{"type": 0, "allow": "0", "deny": "0"}, want: http.StatusOK},
		{route: "DELETE /api/v1/channels/{channel_id}/permissions/{overwrite_id}",
			action: "overwrite.delete", body: map[string]any{"type": 0}, want: http.StatusNoContent},

		{route: "PUT /api/v1/guilds/{guild_id}/members/{user_id}/roles/{role_id}",
			action: "member.role_add", want: http.StatusOK},
		{route: "DELETE /api/v1/guilds/{guild_id}/members/{user_id}/roles/{role_id}",
			action: "member.role_remove", want: http.StatusOK},

		{route: "PATCH /api/v1/guilds/{guild_id}/members/{user_id}", action: "member.update",
			body: map[string]any{"nickname": "Nick"}, want: http.StatusOK},
		{route: "DELETE /api/v1/guilds/{guild_id}/members/{user_id}", action: "member.remove",
			want: http.StatusNoContent},

		{route: "DELETE /api/v1/guilds/{guild_id}/roles/{role_id}", action: "role.delete",
			want: http.StatusNoContent},
		{route: "DELETE /api/v1/channels/{channel_id}", action: "channel.delete",
			want: http.StatusNoContent},

		{route: "DELETE /api/v1/guilds/{guild_id}", action: "guild.delete",
			exempt: "the entry is written in the transaction and removed by the cascade it records — " +
				"TestDeletingAGuildTakesItsAuditTrailWithIt asserts that state directly"},
	}

	cases := make(map[string]auditCase, len(sequence))
	for _, c := range sequence {
		require.NotContainsf(t, cases, c.route, "%s appears twice in the sequence", c.route)
		cases[c.route] = c
	}
	requireCasesMatchTheSurface(t, guildSurfaceRoutes(t), cases)

	for _, c := range sequence {
		if c.exempt != "" {
			continue
		}
		method, pattern, _ := strings.Cut(c.route, " ")
		resp := f.api.call(method, fillRoute(t, pattern, ids), c.body, withToken(f.ownerToken))
		require.Equalf(t, c.want, resp.Code, "%s: %s", c.route, resp)
	}

	// Counted at the end rather than after each call, because "exactly one" is a claim about the whole
	// run: a mutation that wrote a second entry for an *earlier* action would pass a check made before it.
	for _, c := range sequence {
		if c.action == "" || c.route == "DELETE /api/v1/guilds/{guild_id}" {
			continue
		}
		t.Run(c.action, func(t *testing.T) {
			var n int
			f.api.mustQueryRow(t,
				`SELECT count(*) FROM audit_log_entries WHERE guild_id = $1 AND action = $2`,
				[]any{guildID, c.action}, &n)
			require.Equalf(t, 1, n, "%s must write exactly one audit entry (rule 2)", c.action)

			var actor int64
			f.api.mustQueryRow(t,
				`SELECT actor_id FROM audit_log_entries WHERE guild_id = $1 AND action = $2`,
				[]any{guildID, c.action}, &actor)
			require.Equal(t, mustID(t, f.ownerID), actor, "the entry must name who did it")
		})
	}
}

// TestTheMessageAuditVerbAgreesAcrossPackages pins the one string that is written in two places.
//
// `messages` writes `message.delete` and `guilds` validates an `action` filter against its own vocabulary,
// and neither package can import the other — `messages` must not import `guilds` (that is what the M15
// chokepoint extraction was for) and `guilds` importing `messages` to read one constant would recreate the
// coupling in the other direction. So the value is a literal on the guilds side, and this is what stops
// the two drifting.
//
// It lives here because cmd/server already imports both, for the same reason the route-surface tests do.
// Without it, renaming the constant in `messages` would leave the audit log refusing to filter on rows it
// is still storing — the failure M14 chose a refusal over an empty page to make loud.
func TestTheMessageAuditVerbAgreesAcrossPackages(t *testing.T) {
	t.Parallel()

	require.Contains(t, guilds.AuditActions(), messages.ActionMessageDelete,
		"the messages package writes %q and the guilds audit-log reader does not accept it as a filter; "+
			"a verb written to the table but missing from the vocabulary makes the reader refuse rows it "+
			"already holds", messages.ActionMessageDelete)
}

// TestTheTextChannelTypeAgreesAcrossPackages pins the second value written in two places.
//
// `messages.ChannelGuildText` decides which channels accept a message; `guilds.ChannelGuildText` is the
// vocabulary the contract and the channel-creation path use. Neither package may import the other, for
// the reason the audit verb above is a literal, so the value is duplicated and this is what stops it
// drifting.
//
// Getting it wrong is silent in the dangerous direction. If `guilds` ever renumbers the vocabulary — the
// hazard its own comment warns about for permission bits — a stale 0 here would either refuse every text
// channel, which is loud, or start accepting whichever type took position 0, which is not.
func TestTheTextChannelTypeAgreesAcrossPackages(t *testing.T) {
	t.Parallel()

	require.Equal(t, guilds.ChannelGuildText, messages.ChannelGuildText,
		"messages gates sending on its own copy of the text channel type; if the two disagree, sending "+
			"is either refused everywhere or allowed into a channel type no client renders")
}
