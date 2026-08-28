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

	"github.com/Alexnex31/Norite/backend/internal/auth"
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
