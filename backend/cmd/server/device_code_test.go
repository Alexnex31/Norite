package main

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/auth"
)

// M9's flow, driven through the assembled router — real middleware chain, real database, real rate-limit
// buckets. These cover the two JSON endpoints a headless client speaks to; the browser half it never sees
// is covered in device_page_test.go.

// The issuing response is a contract, and a client that cannot read it has nowhere to send anybody.
func TestIssuingADeviceCodeAnswersEverythingAClientNeeds(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)

	resp := api.call(http.MethodPost, "/api/v1/auth/device/code",
		map[string]string{"device_id": "device-a", "device_name": "archlinux"})
	require.Equal(t, http.StatusOK, resp.Code, resp)

	var body struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	resp.decode(&body)

	assert.True(t, strings.HasPrefix(body.DeviceCode, "nod_"), "device code: %q", body.DeviceCode)
	_, err := auth.ParseUserCode(body.UserCode)
	assert.NoError(t, err, "the user code must be one this instance accepts back: %q", body.UserCode)

	// Absolute and on this instance's own origin. A relative path would be unusable — the person opens it
	// on a different machine, which has no idea what it is relative to.
	assert.Equal(t, "https://chat.example.com/device", body.VerificationURI)

	assert.Equal(t, 5, body.Interval)
	assert.InDelta(t, 1200, body.ExpiresIn, 5, "twenty minutes, in seconds")
}

// The ordinary answer to almost every poll, and the shape a client loops on.
func TestPollingAnUnapprovedCodeIsAuthorizationPending(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)
	code := api.issueDeviceCode(t)

	resp := api.call(http.MethodPost, "/api/v1/auth/device/token",
		map[string]string{"device_code": code.DeviceCode})
	require.Equal(t, http.StatusBadRequest, resp.Code, resp)
	assert.Equal(t, "authorization_pending", resp.errorBody().Code)
}

// RFC 8628 §3.5's vocabulary, whole. A near-miss on any of these codes is a client that silently loops
// forever, because every device-flow client already branches on exactly these strings.
func TestThePollAnswersTheDocumentedVocabulary(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)

	for _, tc := range []struct{ why, code, want string }{
		{"a code this instance never issued", "nod_" + strings.Repeat("A", 43), "expired_token"},
		{"a value that is not a device code at all", "hello", "expired_token"},
		{"a user code presented as a device code", "BCDF-GHJK", "expired_token"},
	} {
		resp := api.call(http.MethodPost, "/api/v1/auth/device/token",
			map[string]string{"device_code": tc.code})
		require.Equal(t, http.StatusBadRequest, resp.Code, "%s: %s", tc.why, resp)
		assert.Equal(t, tc.want, resp.errorBody().Code, tc.why)
	}
}

// Rule 8. The device code is a credential and the user code decides who gets signed in, so neither may
// appear anywhere a log or an error body can carry it.
func TestNeitherCodeAppearsInAnErrorBody(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)
	code := api.issueDeviceCode(t)

	resp := api.call(http.MethodPost, "/api/v1/auth/device/token",
		map[string]string{"device_code": code.DeviceCode})
	assert.NotContains(t, resp.String(), code.DeviceCode)
	assert.NotContains(t, resp.String(), code.UserCode)
}

// The poll spends the code and starts a session, which rule 4 forbids a GET from doing — and a GET here
// would also put a credential in a request path, where the logger would keep it. Neither the path
// architecture.md sketched nor the verb it sketched exists.
func TestThePollIsNotReachableAsAGet(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)
	code := api.issueDeviceCode(t)

	resp := api.call(http.MethodGet, "/api/v1/auth/device/code/"+code.DeviceCode, nil)
	assert.Equal(t, http.StatusNotFound, resp.Code, resp)

	resp = api.call(http.MethodGet, "/api/v1/auth/device/token", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, resp.Code, resp)
	assert.Equal(t, "POST", resp.Header.Get("Allow"))
}

// An instance with no public base URL cannot tell a client where to send anybody, so it says so instead of
// issuing a code whose verification_uri is empty and failing in the client minutes later. The same shape
// the reset endpoint uses when there is no mail relay.
func TestWithoutAPublicBaseURLTheFlowSaysItIsUnavailable(t *testing.T) {
	api := newAPIWithoutPublicBaseURL(t)

	resp := api.call(http.MethodPost, "/api/v1/auth/device/code",
		map[string]string{"device_id": "device-a"})
	require.Equal(t, http.StatusServiceUnavailable, resp.Code, resp)
	assert.Equal(t, "device_flow_unavailable", resp.errorBody().Code)
}

// device_id is required, because the session that eventually comes out of this flow is scoped to it and
// there is no later point at which a client could supply one — the browser that approves has no idea.
func TestIssuingWithoutADeviceIDIsRefused(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)

	resp := api.call(http.MethodPost, "/api/v1/auth/device/code", map[string]string{})
	assert.Equal(t, http.StatusBadRequest, resp.Code, resp)
}

// The poll gets its own bucket because polling is repetitive by design and the auth bucket exists to stop
// repetition. A client at the documented interval spends twelve requests a minute, which would be most of
// that bucket on its own.
func TestPollingDoesNotExhaustTheAuthBucket(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)
	code := api.issueDeviceCode(t)

	for range 25 {
		resp := api.call(http.MethodPost, "/api/v1/auth/device/token",
			map[string]string{"device_code": code.DeviceCode}, fromIP("198.51.100.7"))
		require.NotEqual(t, http.StatusTooManyRequests, resp.Code,
			"a client at its documented interval must not be throttled off the instance")
	}

	// And the bucket it shares with nothing else has not been spent by that, so a password login from the
	// same address still works.
	resp := api.call(http.MethodPost, "/api/v1/auth/login",
		map[string]string{"email": "nobody@example.com", "password": "wrong-password-here", "device_id": "d"},
		fromIP("198.51.100.7"))
	assert.NotEqual(t, http.StatusTooManyRequests, resp.Code, resp)
}

// The CLI at M9 and the SPA at Phase O implement against contracts/openapi.yaml, not against this
// package's Go constants, so a recipe that is wrong in the document survives a green suite. This is the
// device flow's sibling of TestTheDocumentedLoopbackRedirectRecipeWorks: everything it asserts is read out
// of the document and compared against what the endpoint actually does.
func TestTheDocumentedDeviceCodeRecipeWorks(t *testing.T) {
	doc := readContract(t)
	api := newAPIWithoutMail(t, auth.RegistrationOpen)
	issued := api.issueDeviceCode(t)

	// The user-code alphabet, as a client would read it: a real YAML field, not prose.
	pattern := regexp.MustCompile(`pattern: "(\^\[BCDF[^"]+)"`).FindStringSubmatch(doc)
	require.Len(t, pattern, 2, "the contract must publish a user_code pattern")
	assert.Regexp(t, pattern[1], issued.UserCode,
		"the issued code does not match the pattern the contract publishes")

	// The two numbers a client sizes its loop from. Written into the prose because a client cannot import
	// a Go constant, which makes them exactly the kind of thing that drifts silently.
	assert.Contains(t, doc, "1200 on this instance", "the documented lifetime must be findable")
	assert.Contains(t, doc, "5 on this instance", "the documented interval must be findable")
	assert.InDelta(t, 1200, issued.ExpiresIn, 5, "the contract says 1200 seconds")
	assert.Equal(t, 5, issued.Interval, "the contract says 5 seconds")

	// And the device-code prefix, which is what a client shape-checks before sending anything upstream.
	assert.Contains(t, doc, `pattern: "^nod_[A-Za-z0-9_-]{43}$"`)
	assert.Regexp(t, `^nod_[A-Za-z0-9_-]{43}$`, issued.DeviceCode)
}

// The poll's four codes are a vocabulary two sides maintain by hand — the contract, and every client that
// branches on it — with nothing holding them together. A code the server can send and the document does
// not name is one no client will handle; a code the document names and the server never sends is one every
// client writes dead wording for.
//
// So the server's half is *produced* rather than listed here. A third copy of the list inside this test
// would agree with itself forever.
func TestTheDocumentedPollVocabularyIsTheOneTheServerSends(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)
	code := api.issueDeviceCode(t)

	var sent []string
	poll := func() string {
		resp := api.call(http.MethodPost, "/api/v1/auth/device/token",
			map[string]string{"device_code": code.DeviceCode})
		require.Equal(t, http.StatusBadRequest, resp.Code, resp)
		return resp.errorBody().Code
	}

	sent = append(sent, poll()) // nobody has approved it
	sent = append(sent, poll()) // and that was inside the interval

	_, err := api.pool.Exec(t.Context(), `UPDATE device_codes SET denied_at = now(), last_polled_at = NULL`)
	require.NoError(t, err)
	sent = append(sent, poll())

	resp := api.call(http.MethodPost, "/api/v1/auth/device/token",
		map[string]string{"device_code": "nod_" + strings.Repeat("A", 43)})
	require.Equal(t, http.StatusBadRequest, resp.Code, resp)
	sent = append(sent, resp.errorBody().Code)

	assert.Equal(t, []string{"access_denied", "authorization_pending", "expired_token", "slow_down"},
		uniqueSorted(sent), "these four are what a poll can answer with")
	assert.Equal(t, uniqueSorted(sent), documentedPollCodes(t),
		"the contract must name every code the poll answers with, and only those")
}

// documentedPollCodes reads the vocabulary out of the poll endpoint's own section of the contract.
func documentedPollCodes(t *testing.T) []string {
	t.Helper()
	doc := readContract(t)

	start := strings.Index(doc, "  /auth/device/token:")
	require.Positive(t, start, "the poll endpoint must be documented")
	section := doc[start:]
	section = section[:strings.Index(section, "\n  /auth/tokens:")]

	// Anchored below the summary line, so a word that happens to appear in prose elsewhere in the document
	// cannot be swept up — the mistake M8's vocabulary test made and had to correct.
	var found []string
	for _, m := range regexp.MustCompile("`([a-z_]{4,})`").FindAllStringSubmatch(section, -1) {
		if strings.Contains(m[1], "_") || m[1] == "slow" {
			found = append(found, m[1])
		}
	}
	return uniqueSorted(found)
}

func readContract(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "openapi.yaml"))
	require.NoError(t, err)
	return string(raw)
}

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// Issuing a code mints a database row from an unauthenticated request, which is exactly the shape the
// stricter bucket exists for — /authorize's own comment says so. It shares the /auth/device prefix with the
// poll and deliberately not the poll's bucket, because a bucket permissive enough for twelve requests a
// minute is six times looser than this half should be.
func TestIssuingACodeCarriesTheStricterBucket(t *testing.T) {
	api := newAPIWithoutMail(t, auth.RegistrationOpen)

	throttled := false
	for range 30 {
		resp := api.call(http.MethodPost, "/api/v1/auth/device/code",
			map[string]string{"device_id": "waiting-device"}, fromIP("198.51.100.9"))
		if resp.Code == http.StatusTooManyRequests {
			throttled = true
			break
		}
	}
	assert.True(t, throttled, "issuing must be bounded by the same limit as every other row-minting route")

	// And the poll is not, from the same address: the two are counted apart, which is the point of the
	// split. This is what stops a throttled flood of issuance also killing a sign-in already in flight.
	resp := api.call(http.MethodPost, "/api/v1/auth/device/token",
		map[string]string{"device_code": "nod_" + strings.Repeat("A", 43)}, fromIP("198.51.100.9"))
	assert.NotEqual(t, http.StatusTooManyRequests, resp.Code, resp)
}

// issuedCode is what a client gets back from /auth/device/code.
type issuedCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

func (a *api) issueDeviceCode(t *testing.T) issuedCode {
	t.Helper()
	resp := a.call(http.MethodPost, "/api/v1/auth/device/code",
		map[string]string{"device_id": "waiting-device", "device_name": "archlinux"})
	require.Equal(t, http.StatusOK, resp.Code, resp)

	var out issuedCode
	resp.decode(&out)
	return out
}
