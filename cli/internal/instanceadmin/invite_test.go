package instanceadmin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/operatortoken"
)

// fakeInviteAPI answers the three invite endpoints and records what it was asked.
type fakeInviteAPI struct {
	server *httptest.Server
	bearer string
	method string
	path   string
	body   map[string]any
	// rawURI is what actually crossed the wire. r.URL.Path is the *decoded* form, so it shows a slash
	// whether the client escaped one or not — asserting on it would pass against an unescaped client.
	rawURI  string
	status  int
	reply   string
	deleted string
}

func newFakeInviteAPI(t *testing.T) *fakeInviteAPI {
	t.Helper()
	f := &fakeInviteAPI{status: http.StatusCreated}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.bearer = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.method, f.path, f.rawURI = r.Method, r.URL.Path, r.RequestURI
		f.body = nil
		_ = json.NewDecoder(r.Body).Decode(&f.body)

		if r.Method == http.MethodDelete {
			f.deleted = strings.TrimPrefix(r.URL.Path, "/api/v1/instance/invites/")
			w.WriteHeader(f.status)
			if f.reply != "" {
				_, _ = w.Write([]byte(f.reply))
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.reply))
	}))
	t.Cleanup(f.server.Close)
	return f
}

func inviteRunner(t *testing.T, f *fakeInviteAPI, jsonOut bool) (*Runner, *strings.Builder) {
	t.Helper()
	out := &strings.Builder{}
	return &Runner{
		Options: Options{ConfigPath: writeConfig(t, f.server.URL)},
		Out:     out,
		JSON:    jsonOut,
	}, out
}

// The credential every invite call carries is the same operator token bootstrap uses — this command
// cannot produce an administrator's access token, because the CLI does not hold one (ADR 0011).
func TestInviteCommandsAuthenticateAsTheOperator(t *testing.T) {
	f := newFakeInviteAPI(t)
	f.reply = `{"code":"BCDFGHJKMNPQRSTV","created_by":null,"max_uses":null,"uses":0,` +
		`"expires_at":null,"created_at":"2026-08-25T10:00:00Z"}`

	r, _ := inviteRunner(t, f, false)
	require.NoError(t, r.createInvite(context.Background(), 0, 0))

	require.NotEmpty(t, f.bearer)
	assert.NoError(t, operatortoken.Verify([]byte(testSigningKey), f.bearer, time.Now()))
}

// Neither flag means unlimited and permanent, and that has to reach the wire as *absent* fields rather
// than as zeros — the contract reads an omitted max_uses as unlimited and a zero as invalid.
func TestCreateWithNoLimitsSendsNoLimits(t *testing.T) {
	f := newFakeInviteAPI(t)
	f.reply = `{"code":"BCDFGHJKMNPQRSTV","created_by":null,"max_uses":null,"uses":0,` +
		`"expires_at":null,"created_at":"2026-08-25T10:00:00Z"}`

	r, out := inviteRunner(t, f, false)
	require.NoError(t, r.createInvite(context.Background(), 0, 0))

	assert.NotContains(t, f.body, "max_uses")
	assert.NotContains(t, f.body, "expires_in_seconds")
	assert.Contains(t, out.String(), "BCDFGHJKMNPQRSTV")
	assert.Contains(t, out.String(), "never expires")
}

// A Go duration is this client's convenience and has no business crossing the wire; the contract takes
// seconds.
func TestCreateSendsTheExpiryInSeconds(t *testing.T) {
	f := newFakeInviteAPI(t)
	f.reply = `{"code":"BCDFGHJKMNPQRSTV","created_by":null,"max_uses":3,"uses":0,` +
		`"expires_at":"2026-09-01T10:00:00Z","created_at":"2026-08-25T10:00:00Z"}`

	r, _ := inviteRunner(t, f, false)
	require.NoError(t, r.createInvite(context.Background(), 3, 48*time.Hour))

	assert.EqualValues(t, 3, f.body["max_uses"])
	assert.EqualValues(t, 172800, f.body["expires_in_seconds"])
}

// An empty list prints `[]`, never `null`. Go marshals a nil slice to null, so this is something the
// command has to do on purpose — and a caller iterating the result should not have to special-case it.
func TestAnEmptyListPrintsAnEmptyArray(t *testing.T) {
	f := newFakeInviteAPI(t)
	f.status = http.StatusOK
	f.reply = `[]`

	r, out := inviteRunner(t, f, true)
	require.NoError(t, r.listInvites(context.Background()))
	assert.Equal(t, "[]", strings.TrimSpace(out.String()))

	// And the human form says what to do rather than printing nothing.
	r2, out2 := inviteRunner(t, f, false)
	require.NoError(t, r2.listInvites(context.Background()))
	assert.Contains(t, out2.String(), "invite create")
}

// The --json shape is a contract this repository owns (rule 15), so it is checked against the schema file
// rather than against whatever the code currently emits.
func TestTheJSONOutputMatchesItsSchema(t *testing.T) {
	f := newFakeInviteAPI(t)
	f.reply = `{"code":"BCDFGHJKMNPQRSTV","created_by":"12345","max_uses":3,"uses":1,` +
		`"expires_at":"2026-09-01T10:00:00Z","created_at":"2026-08-25T10:00:00Z"}`

	r, out := inviteRunner(t, f, true)
	require.NoError(t, r.createInvite(context.Background(), 3, time.Hour))

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out.String()), &got))

	// Every property the schema requires is present, including the nullable ones — the schema's rule is
	// that they are explicitly null rather than omitted, so a caller reads the same keys every time.
	for _, key := range requiredInviteKeys(t) {
		assert.Contains(t, got, key, "the schema requires %q", key)
	}
	assert.Len(t, got, len(requiredInviteKeys(t)), "no field may be printed that the schema does not declare")

	code, _ := got["code"].(string)
	assert.Regexp(t, regexp.MustCompile(schemaCodePattern(t)), code)
}

// A nullable field is printed as null rather than omitted, which is the convention the schema fixes.
func TestNullableFieldsArePrintedAsNull(t *testing.T) {
	f := newFakeInviteAPI(t)
	f.reply = `{"code":"BCDFGHJKMNPQRSTV","created_by":null,"max_uses":null,"uses":0,` +
		`"expires_at":null,"created_at":"2026-08-25T10:00:00Z"}`

	r, out := inviteRunner(t, f, true)
	require.NoError(t, r.createInvite(context.Background(), 0, 0))

	assert.Contains(t, out.String(), `"created_by": null`)
	assert.Contains(t, out.String(), `"max_uses": null`)
	assert.Contains(t, out.String(), `"expires_at": null`)
}

// A code with a slash in it would address a different endpoint entirely. It is this client's own argument
// rather than something the instance said, but it still has to be escaped.
func TestARevokedCodeIsEscapedIntoThePath(t *testing.T) {
	f := newFakeInviteAPI(t)
	f.status = http.StatusNoContent

	r, _ := inviteRunner(t, f, false)
	require.NoError(t, r.revokeInvite(context.Background(), "AAAA/../../healthz"))

	assert.Equal(t, "/api/v1/instance/invites/AAAA%2F..%2F..%2Fhealthz", f.rawURI,
		"a slash in the code must not escape the endpoint it addresses")

	// And it still reached the invite handler rather than some other route, which is the property the
	// escaping is actually for.
	assert.Equal(t, http.MethodDelete, f.method)
	assert.True(t, strings.HasPrefix(f.path, "/api/v1/instance/invites/"), "path: %s", f.path)
}

// Revoking something that was not there says so, and says how to find out what is.
func TestRevokingAnUnknownCodeSaysWhatToCheck(t *testing.T) {
	f := newFakeInviteAPI(t)
	f.status = http.StatusNotFound
	f.reply = `{"error":{"code":"not_found","message":"not found","request_id":"r1"}}`

	r, _ := inviteRunner(t, f, false)
	err := r.revokeInvite(context.Background(), "BCDFGHJKMNPQRSTV")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invite list")
}

// An expired invite says "expired" rather than showing a date in the past — that is the answer somebody is
// looking for when they ask why a code did not work.
func TestAnExpiredInviteIsDescribedAsExpired(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	assert.Equal(t, "expired", describeExpiry(inviteView{ExpiresAt: &past}))
	assert.Equal(t, "never expires", describeExpiry(inviteView{}))

	future := time.Now().Add(time.Hour)
	assert.Contains(t, describeExpiry(inviteView{ExpiresAt: &future}), "expires ")
}

// Use counts read the way a person asks about them, and an unlimited invite does not claim a ceiling.
func TestUsesAreDescribedForBothKinds(t *testing.T) {
	three := int32(3)
	assert.Equal(t, "1 of 3 used", describeUses(inviteView{Uses: 1, MaxUses: &three}))
	assert.Equal(t, "7 used", describeUses(inviteView{Uses: 7}))
}
