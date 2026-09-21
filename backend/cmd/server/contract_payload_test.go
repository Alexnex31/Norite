// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/Alexnex31/Norite/backend/internal/apicontract"
	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/guilds"
)

// The half of CLAUDE.md rule 6 that contract_test.go does not cover.
//
// That file checks the *set* of routes against contracts/openapi.yaml and says so: payload drift waits for
// oapi-codegen at M12. Waiting turned out to cost something. Validating real responses against the
// document by hand found two payload defects that had been committed and reviewed:
//
//   - MintedApiToken was unsatisfiable. It composed over ApiToken with allOf, and allOf branches validate
//     independently, so ApiToken's `additionalProperties: false` rejected the `value` the second branch
//     required. Every successful mint failed its own schema — of the one credential this API cannot
//     reissue.
//   - Eight error codes the backend emits were absent from the Error enum, including authorization_pending
//     and slow_down, which a device-flow client branches on every few seconds.
//
// Both are the same shape: a document nothing ever compared with a response. These tests are narrow on
// purpose — no JSON Schema library, just "what the document declares" against "what the server sent" —
// and they cover the two places where being wrong is expensive.

// contractSchemas returns components.schemas from the contract document.
func contractSchemas(t *testing.T) map[string]map[string]any {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "openapi.yaml"))
	require.NoError(t, err)

	var doc struct {
		Components struct {
			Schemas map[string]map[string]any `yaml:"schemas"`
		} `yaml:"components"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	require.NotEmpty(t, doc.Components.Schemas, "the contract declares no schemas")
	return doc.Components.Schemas
}

func declaredProperties(t *testing.T, schema map[string]any) (all, required []string) {
	t.Helper()

	props, ok := schema["properties"].(map[string]any)
	require.True(t, ok, "schema has no properties block")
	for name := range props {
		all = append(all, name)
	}
	for _, r := range schema["required"].([]any) {
		required = append(required, r.(string))
	}
	sort.Strings(all)
	sort.Strings(required)
	return all, required
}

// A minted token's response must be exactly what the contract says it is.
//
// Exactly, in both directions: an undeclared key is a client that silently drops it — and the key at risk
// here is `value`, which exists in this one response and nowhere else, ever. A declared key the server
// does not send is a client that generates a required field it will never receive.
func TestTheMintedTokenResponseMatchesTheContract(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	acct := a.newAccount("ada", "ada@example.com", "laptop")

	res := a.call(http.MethodPost, "/api/v1/auth/tokens",
		map[string]any{"name": "status bot", "scopes": []string{"identify"}},
		withToken(acct.Tokens.AccessToken))
	require.Equal(t, http.StatusCreated, res.Code, res)

	var body map[string]any
	require.NoError(t, json.Unmarshal(res.Body, &body))

	var got []string
	for k := range body {
		got = append(got, k)
	}
	sort.Strings(got)

	declared, required := declaredProperties(t, contractSchemas(t)["MintedApiToken"])

	assert.Equal(t, declared, got,
		"POST /auth/tokens sent %v; MintedApiToken declares %v", got, declared)
	assert.Contains(t, required, "value",
		"the whole point of this response is the credential; the contract must require it")
	assert.Contains(t, got, "value")
}

// Every error code the server can produce must be one the contract names.
//
// Driven through real requests rather than by grepping for string literals: a code that no route can
// actually emit does not need documenting, and a grep would list those too. The cost is that this covers
// the codes exercised below and not every code in the source — which is why the list is deliberately wide,
// and why a new one belongs here at the same time as it belongs in the enum.
func TestEveryErrorCodeTheServerSendsIsInTheContract(t *testing.T) {
	documented := documentedErrorCodes(t)
	seen := map[string]string{}

	record := func(what string, res *response) {
		require.GreaterOrEqual(t, res.Code, 400, "%s should have been refused: %s", what, res)
		code := res.errorBody().Code
		require.NotEmpty(t, code, "%s produced an error with no code", what)
		seen[code] = what
	}

	// One instance for almost everything. A second newAPI in the same test would collide with the first
	// on the throwaway database dbtest names after the test, so anything needing different configuration
	// gets a subtest — which is also what gives it its own name, and therefore its own database.
	a := newAPI(t, auth.RegistrationInvite)

	record("a body missing required fields",
		a.call(http.MethodPost, "/api/v1/auth/login", map[string]any{}))
	record("credentials that match nothing",
		a.call(http.MethodPost, "/api/v1/auth/login",
			map[string]any{"email": "nobody@example.com", "password": testPassword, "device_id": "d"}))
	record("an unknown path", a.call(http.MethodGet, "/api/v1/no-such-thing", nil))
	record("the wrong method on a real route",
		a.call(http.MethodDelete, "/api/v1/auth/login", nil))
	record("no invite on a gated instance",
		a.call(http.MethodPost, "/api/v1/auth/register",
			map[string]any{"username": "ada", "email": "ada@example.com", "password": testPassword}))
	record("an invite that does not exist",
		a.call(http.MethodPost, "/api/v1/auth/register",
			map[string]any{"username": "ada", "email": "ada@example.com", "password": testPassword,
				"invite_code": "BCDFGHJKMNPQRSTV"}))
	record("an unauthenticated read", a.call(http.MethodGet, "/api/v1/users/@me", nil))
	record("polling a device code that does not exist",
		a.call(http.MethodPost, "/api/v1/auth/device/token", map[string]any{"device_code": "nod_nothing"}))
	record("exchanging an invented oauth code",
		a.call(http.MethodPost, "/api/v1/auth/oauth/exchange",
			map[string]any{"code": "noc_nothing", "flow_verifier": "nof_x", "device_id": "d"}))

	// The mailer is consulted per request, so an instance can be made relay-less in place rather than
	// rebuilt. This is the 503 whose message the platform used to replace with "internal server error".
	a.mail.disabled = true
	record("reset on an instance with no relay",
		a.call(http.MethodPost, "/api/v1/auth/password/reset/request",
			map[string]any{"email": "ada@example.com"}))
	a.mail.disabled = false

	t.Run("an instance that does not know its own address", func(t *testing.T) {
		bare := newAPIWithBaseURL(t, auth.RegistrationOpen, &captureMailer{}, nil, "")
		res := bare.call(http.MethodPost, "/api/v1/auth/device/code", map[string]any{"device_id": "d"})
		require.GreaterOrEqual(t, res.Code, 400, "%s", res)
		seen[res.errorBody().Code] = "starting a device flow with no public base URL"
	})

	for code, what := range seen {
		assert.True(t, documented[code],
			"%q (from %s) is not in the Error enum in contracts/openapi.yaml — rule 6 wants it there in "+
				"the same commit as the code that emits it", code, what)
	}
	t.Logf("checked %d distinct error codes against the contract", len(seen))
}

// documentedErrorCodes reads the Error schema's enum out of the contract.
func documentedErrorCodes(t *testing.T) map[string]bool {
	t.Helper()

	schema := contractSchemas(t)["Error"]
	errProp := schema["properties"].(map[string]any)["error"].(map[string]any)
	codeProp := errProp["properties"].(map[string]any)["code"].(map[string]any)

	out := map[string]bool{}
	for _, c := range codeProp["enum"].([]any) {
		out[c.(string)] = true
	}
	require.NotEmpty(t, out, "the contract's Error schema declares no codes")
	return out
}

// A validation failure must name the field the caller sent, not the Go field this server stores it in.
//
// `field "DeviceID" failed the "required" requirement` names an identifier that appears nowhere in
// contracts/openapi.yaml, and sits two lines away from the decoder's own errors, which quote the wire name
// (`unknown field "admin"`). A person has to guess the mapping and a generated client cannot.
func TestValidationErrorsNameTheFieldTheCallerSent(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)

	for _, tc := range []struct {
		what   string
		path   string
		body   any
		wire   string
		goName string
	}{
		{"login with no device_id", "/api/v1/auth/login",
			map[string]any{"email": "ada@example.com", "password": testPassword}, "device_id", "DeviceID"},
		{"register with no username", "/api/v1/auth/register",
			map[string]any{"email": "ada@example.com", "password": testPassword}, "username", "Username"},
		{"an over-long display name", "/api/v1/auth/register",
			map[string]any{"username": "ada", "email": "ada@example.com", "password": testPassword,
				"display_name": strings.Repeat("x", 200)}, "display_name", "DisplayName"},
		{"a device name past its limit", "/api/v1/auth/device/code",
			map[string]any{"device_id": "d", "device_name": strings.Repeat("x", 100)},
			"device_name", "DeviceName"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			res := a.call(http.MethodPost, tc.path, tc.body)
			require.Equal(t, http.StatusBadRequest, res.Code, res)

			message := res.errorBody().Message
			assert.Contains(t, message, tc.wire, "the message must name the field as sent")
			assert.NotContains(t, message, tc.goName, "the message must not leak the Go field name")
		})
	}
}

// A 503 this server raises on purpose must say which feature is switched off.
//
// The blanket "internal server error" that used to replace every 5xx message reported an instance that was
// merely unconfigured as a broken one — and both of these handlers had already written the sentence that
// would have said what to do. What must still never cross the wire is the underlying error, which is why
// the generic string is keyed on an *absent* message rather than on the status.
func TestADeliberateOutageSaysWhichFeatureIsOff(t *testing.T) {
	t.Run("no mail relay", func(t *testing.T) {
		a := newAPIWithoutMail(t, auth.RegistrationOpen)
		res := a.call(http.MethodPost, "/api/v1/auth/password/reset/request",
			map[string]any{"email": "ada@example.com"})

		require.Equal(t, http.StatusServiceUnavailable, res.Code, res)
		body := res.errorBody()
		assert.Equal(t, "reset_unavailable", body.Code)
		assert.Contains(t, body.Message, "email relay",
			"a person reading this must learn the instance is unconfigured, not broken")
		assert.NotEqual(t, "internal server error", body.Message)
	})

	t.Run("no public base url", func(t *testing.T) {
		a := newAPIWithBaseURL(t, auth.RegistrationOpen, &captureMailer{}, nil, "")
		res := a.call(http.MethodPost, "/api/v1/auth/device/code", map[string]any{"device_id": "d"})

		require.Equal(t, http.StatusServiceUnavailable, res.Code, res)
		body := res.errorBody()
		assert.Equal(t, "device_flow_unavailable", body.Code)
		assert.Contains(t, body.Message, "public base URL")
		assert.NotEqual(t, "internal server error", body.Message)
	})
}

// M11's two new response shapes, against what the document declares.
//
// Same reasoning as the minted-token test above, and the reason that one exists: contract_test.go compares
// the set of routes and never a payload, so a field the server sends and the schema omits — or the reverse
// — is invisible to it. Session in particular has a field that is easy to get wrong in a way no functional
// test would notice: first_seen must be the device family's start, and a schema that did not require it
// would let a client codegen it away.
func TestTheSessionListingMatchesTheContract(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	acct := a.newAccount("ada", "ada@example.com", "laptop")

	res := a.call(http.MethodGet, "/api/v1/users/@me/sessions", nil, withToken(acct.Tokens.AccessToken))
	require.Equal(t, http.StatusOK, res.Code, res)

	var listing []map[string]any
	require.NoError(t, json.Unmarshal(res.Body, &listing))
	require.Len(t, listing, 1, "one device is signed in")

	var got []string
	for k := range listing[0] {
		got = append(got, k)
	}
	sort.Strings(got)

	declared, required := declaredProperties(t, contractSchemas(t)["Session"])
	assert.Equal(t, declared, got, "GET /users/@me/sessions sent %v; Session declares %v", got, declared)
	assert.Equal(t, declared, required,
		"every field of a Session is always present — a nullable one is explicitly null, never omitted")
}

// And an empty listing is an array, not null.
//
// Go marshals a nil slice to `null`, so this is something the handler has to do on purpose — the same
// convention contracts/cli-json/README.md fixes for the CLI, and the same trap.
func TestAnEmptySessionListingIsAnArray(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	acct := a.newAccount("ada", "ada@example.com", "laptop")

	// Sign the only device out, then read the listing with the access token it still holds — which stays
	// valid for its full life, since access tokens are not checked against session state (§17.10).
	res := a.call(http.MethodPost, "/api/v1/auth/logout/all", nil, withToken(acct.Tokens.AccessToken))
	require.Equal(t, http.StatusOK, res.Code, res)
	sessions := a.call(http.MethodDelete, "/api/v1/users/@me/sessions/"+currentSessionID(t, a, acct.Tokens.AccessToken),
		nil, withToken(acct.Tokens.AccessToken))
	require.Equal(t, http.StatusNoContent, sessions.Code, sessions)

	got := a.call(http.MethodGet, "/api/v1/users/@me/sessions", nil, withToken(acct.Tokens.AccessToken))
	require.Equal(t, http.StatusOK, got.Code, got)
	assert.Equal(t, "[]", strings.TrimSpace(got.String()), "an empty listing is [], never null")
}

func currentSessionID(t *testing.T, a *api, accessToken string) string {
	t.Helper()
	for _, s := range listSessions(t, a, accessToken) {
		if s.Current {
			return s.ID
		}
	}
	t.Fatal("no current session in the listing")
	return ""
}

// The source offer AGPL-3.0 section 13 obliges this instance to make.
//
// Checked against the contract the same way the session listing is, because the failure this catches is a
// field quietly renamed or dropped: nothing else in the codebase reads this payload, so a client written
// against the contract is the only thing that would notice, and it would notice in production.
func TestTheSourceOfferMatchesTheContract(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)

	res := a.call(http.MethodGet, "/api/v1/meta", nil)
	require.Equal(t, http.StatusOK, res.Code, res)

	var body map[string]any
	require.NoError(t, json.Unmarshal(res.Body, &body))

	var got []string
	for k := range body {
		got = append(got, k)
	}
	sort.Strings(got)

	declared, required := declaredProperties(t, contractSchemas(t)["InstanceMeta"])
	assert.Equal(t, declared, got, "GET /meta sent %v; InstanceMeta declares %v", got, declared)
	assert.Equal(t, declared, required, "every field of the offer is always present; none is nullable")

	assert.Equal(t, "AGPL-3.0-or-later", body["license"],
		"the identifier is a constant — a fork that genuinely relicenses edits meta.License deliberately")
	assert.NotEmpty(t, body["source_url"], "an empty offer is not an offer")
}

// Section 13 owes the offer to anyone interacting with the instance over a network, so a credential must
// not be part of reaching it. Removing `security: []` from the contract, or mounting this route under
// /instance, would break exactly this and nothing else — no other test here sends no Authorization header
// and expects a 200.
func TestTheSourceOfferNeedsNoCredential(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)

	anonymous := a.call(http.MethodGet, "/api/v1/meta", nil)
	require.Equal(t, http.StatusOK, anonymous.Code, "the section 13 offer must not require a credential")

	// A garbage credential must not change the answer either: a client that happens to attach a stale
	// token is still owed the offer, so the route must not be behind a verifier that rejects.
	rejected := a.call(http.MethodGet, "/api/v1/meta", nil, withToken("nrt_not_a_real_token"))
	require.Equal(t, http.StatusOK, rejected.Code, "a bad credential must not turn the offer into a 401")
	assert.JSONEq(t, string(anonymous.Body), string(rejected.Body), "the offer is the same either way")
}

// TestTheScopeVocabularyMatchesTheContract and its audit-action sibling close the third contract gap.
//
// Neither existing contract test looks at an enum. contract_test.go compares the route *set*; the tests
// above this one validate response payloads, which catches an enum only if some exercised response
// happens to carry the value. So `messages.read`/`messages.write` and `message.delete` were added to the
// Go vocabularies at M15 and to no enum, and both gates stayed green — a client generated from the
// contract could not ask for the scopes the four message routes are gated on, and could not decode an
// audit page containing a moderator deletion. Found by a code review reading the YAML by hand, which is
// precisely the thing rule 6 exists to make unnecessary.
//
// Asserted against `internal/apicontract`, which is generated *from* the YAML and checked for staleness
// by `just contract-check` — so this compares the Go vocabulary to the document, transitively, without
// parsing YAML a second time. It lives here because this package is the only one importing `auth`,
// `guilds` and `apicontract` together, for the reason the route-surface tests live here.
func TestTheScopeVocabularyMatchesTheContract(t *testing.T) {
	t.Parallel()

	for _, s := range auth.AllScopes {
		require.Truef(t, apicontract.Scope(s).Valid(),
			"auth.AllScopes carries %q and the contract's Scope enum does not; a token minted with it "+
				"cannot be requested by any client generated from contracts/openapi.yaml", s)
	}

	// And the other direction, so an enum value the server would reject cannot sit in the contract
	// advertising a capability that does not exist.
	for _, s := range []apicontract.Scope{
		apicontract.Identify, apicontract.GuildsRead, apicontract.GuildsWrite,
		apicontract.GuildsAudit, apicontract.MessagesRead, apicontract.MessagesWrite,
		apicontract.ReportsWrite, apicontract.ReportsModerate,
	} {
		require.Truef(t, auth.ValidScope(auth.Scope(s)),
			"the contract offers scope %q and the server rejects it", s)
	}
}

// TestEveryAuditActionIsInTheContract pins the verb vocabulary the audit-log reader returns and filters on.
//
// One direction only, deliberately: the Go list is what the server writes and accepts, so a verb missing
// from the enum is the failure that matters. A value in the enum that Go does not write yet is how a
// reserved verb would legitimately look.
func TestEveryAuditActionIsInTheContract(t *testing.T) {
	t.Parallel()

	actions := guilds.AuditActions()
	require.NotEmpty(t, actions, "the vocabulary came back empty, which would make this vacuous")

	for _, a := range actions {
		require.Truef(t, apicontract.AuditLogAction(a).Valid(),
			"guilds.AuditActions() carries %q and the contract's AuditLogAction enum does not; the reader "+
				"accepts it as an ?action filter and returns entries carrying it, so a generated client "+
				"can neither filter on it nor decode a page containing one", a)
	}
}

// TestTheModerationResponsesMatchTheContract pins the three report shapes and M16a's edit-history pair
// against the document.
//
// Three rather than one because they are written out flat rather than composed with `allOf` — which is
// itself a decision this file's own history argues for: MintedApiToken composed that way and shipped a
// schema no successful response could satisfy, because `allOf` branches validate independently and a
// branch carrying `additionalProperties: false` rejects what its sibling adds.
//
// The assertion that matters most is the one about absence. `reporter_id` is deliberately not on any of
// these, and a contract that declared it would be advertising a field the server will never send — which
// is the direction a generated client turns into a required property it waits forever for.
//
// M16a added the edit-history pair here rather than to a test of its own, because the fixture is the same
// four objects and the surfaces are the same kind of thing: a moderator reading content. That is why the
// name says moderation rather than reports — it was TestTheReportResponsesMatchTheContract, and a name
// that stops describing what a test covers is the one M15 renamed AuthorizeChannelForRead over.
func TestTheModerationResponsesMatchTheContract(t *testing.T) {
	a := newAPI(t, auth.RegistrationOpen)
	schemas := contractSchemas(t)

	owner := a.newAccount("owner", "owner@example.com", "laptop")
	guild := a.call(http.MethodPost, "/api/v1/guilds", map[string]any{"name": "Guild"},
		withToken(owner.Tokens.AccessToken))
	require.Equal(t, http.StatusCreated, guild.Code, guild)
	guildID := guild.field(t, "id")

	channel := a.call(http.MethodPost, "/api/v1/guilds/"+guildID+"/channels",
		map[string]any{"name": "general", "type": 0}, withToken(owner.Tokens.AccessToken))
	require.Equal(t, http.StatusCreated, channel.Code, channel)

	message := a.call(http.MethodPost, "/api/v1/channels/"+channel.field(t, "id")+"/messages",
		map[string]any{"content": "hello"}, withToken(owner.Tokens.AccessToken))
	require.Equal(t, http.StatusCreated, message.Code, message)

	filed := a.call(http.MethodPost, "/api/v1/reports", map[string]any{
		"target_type": "message", "target_id": message.field(t, "id"), "reason_category": "spam",
		"detail": "please look at this",
	}, withToken(owner.Tokens.AccessToken))
	require.Equal(t, http.StatusCreated, filed.Code, filed)
	reportID := filed.field(t, "id")

	page := a.call(http.MethodGet, "/api/v1/guilds/"+guildID+"/reports", nil,
		withToken(owner.Tokens.AccessToken))
	require.Equal(t, http.StatusOK, page.Code, page)

	detail := a.call(http.MethodGet, "/api/v1/guilds/"+guildID+"/reports/"+reportID, nil,
		withToken(owner.Tokens.AccessToken))
	require.Equal(t, http.StatusOK, detail.Code, detail)

	// M16a. An edit first, or the history comes back with an empty `versions` and the nested schema is
	// never exercised — which is this test's own lesson about an enum only being seen if some response
	// happens to carry the value.
	channelID := channel.field(t, "id")
	messageID := message.field(t, "id")
	edited := a.call(http.MethodPatch, "/api/v1/channels/"+channelID+"/messages/"+messageID,
		map[string]any{"content": "hello, corrected"}, withToken(owner.Tokens.AccessToken))
	require.Equal(t, http.StatusOK, edited.Code, edited)

	history := a.call(http.MethodGet,
		"/api/v1/channels/"+channelID+"/messages/"+messageID+"/history", nil,
		withToken(owner.Tokens.AccessToken))
	require.Equal(t, http.StatusOK, history.Code, history)

	var historyBody struct {
		Versions []map[string]any `json:"versions"`
	}
	require.NoError(t, json.Unmarshal(history.Body, &historyBody))
	require.Len(t, historyBody.Versions, 1, "one edit, so one prior version to validate the item schema")

	var pageBody []map[string]any
	require.NoError(t, json.Unmarshal(page.Body, &pageBody))
	require.Len(t, pageBody, 1)

	for _, tc := range []struct {
		what   string
		schema string
		body   []byte
		object map[string]any
	}{
		{what: "POST /reports", schema: "Report", body: filed.Body},
		{what: "GET a guild's reports", schema: "TriageReport", object: pageBody[0]},
		{what: "GET one report", schema: "TriageReportDetail", body: detail.Body},
		{what: "GET a message's edit history", schema: "MessageEditHistory", body: history.Body},
		{what: "one prior version", schema: "MessageEditVersion", object: historyBody.Versions[0]},
	} {
		t.Run(tc.schema, func(t *testing.T) {
			object := tc.object
			if object == nil {
				require.NoError(t, json.Unmarshal(tc.body, &object))
			}

			var got []string
			for k := range object {
				got = append(got, k)
			}
			sort.Strings(got)

			declared, required := declaredProperties(t, schemas[tc.schema])
			assert.Equal(t, declared, got,
				"%s sent %v; %s declares %v", tc.what, got, tc.schema, declared)
			assert.Equal(t, declared, required,
				"%s: every field is always present on these responses, so the contract should require "+
					"all of them — a nullable field is still sent, as null", tc.schema)

			assert.NotContains(t, got, "reporter_id",
				"%s names the reporter; a guild moderator is never told who filed", tc.what)
		})
	}
}
