package daemonproc

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/credentials"
)

// M7's "done when" is that the daemon uses the stored token on its next launch without re-prompting. These
// drive that directly: a credential on disk, a stand-in instance, and the refresh the daemon performs when
// it starts.

// fakeInstance stands in for the backend's refresh endpoint.
type fakeInstance struct {
	server *httptest.Server

	// received is the refresh token the daemon presented.
	received string
	// status and body override the response for the failure paths.
	status int
	body   string
	// calls counts requests, so a test can assert the daemon did not call at all.
	calls int
}

func newFakeInstance(t *testing.T) *fakeInstance {
	t.Helper()
	f := &fakeInstance{}

	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		require.Equal(t, "/api/v1/auth/refresh", r.URL.Path)

		var body struct {
			RefreshToken string `json:"refresh_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.received = body.RefreshToken

		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = io.WriteString(w, f.body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "eyJ.fresh.access", "refresh_token": "nrt_rotated",
			"token_type": "Bearer", "expires_at": "2030-01-01T00:00:00Z",
		})
	}))
	t.Cleanup(f.server.Close)
	return f
}

// storedSession writes a credential the way `norite login` would.
func storedSession(t *testing.T, instanceURL, refreshToken string) *credentials.Store {
	t.Helper()
	store, err := credentials.OpenIn(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.Save(credentials.Record{
		InstanceURL: instanceURL,
		UserID:      "123456789",
		Username:    "ada",
		DeviceID:    "dev_test",
		DeviceName:  "laptop",
	}, refreshToken))
	return store
}

// testLogger captures log output so a test can assert what an operator would actually read.
func testLogger(out io.Writer) zerolog.Logger {
	return zerolog.New(out).Level(zerolog.DebugLevel)
}

// ---------- the milestone's criterion ----------

func TestTheDaemonSignsInWithTheStoredCredential(t *testing.T) {
	f := newFakeInstance(t)
	store := storedSession(t, f.server.URL, "nrt_from_login")
	logs := &strings.Builder{}

	sess := establishSession(t.Context(), testLogger(logs), store, f.server.Client())

	require.NotNil(t, sess, "a stored credential must produce a session")
	assert.Equal(t, "nrt_from_login", f.received, "the daemon presents what the login stored")
	assert.Equal(t, "eyJ.fresh.access", sess.accessToken)
	assert.Equal(t, "ada", sess.record.Username)
	assert.Contains(t, logs.String(), "signed in with the stored credential")
}

// Refresh tokens rotate, and the instance detects reuse of a spent one (M4). So the new token has to
// replace the old one at the moment it is issued, or the next start presents something already retired.
func TestTheRotatedTokenReplacesTheStoredOne(t *testing.T) {
	f := newFakeInstance(t)
	store := storedSession(t, f.server.URL, "nrt_from_login")

	require.NotNil(t, establishSession(t.Context(), testLogger(io.Discard), store, f.server.Client()))

	_, stored, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "nrt_rotated", stored)

	// ...and a second start presents the rotated one rather than the original.
	require.NotNil(t, establishSession(t.Context(), testLogger(io.Discard), store, f.server.Client()))
	assert.Equal(t, "nrt_rotated", f.received)
}

// The access token stays in memory. Fifteen minutes is shorter than the interval between the restarts it
// would be persisted to survive, so writing it down adds a credential at rest and buys nothing.
func TestTheAccessTokenIsNeverWrittenDown(t *testing.T) {
	f := newFakeInstance(t)
	store := storedSession(t, f.server.URL, "nrt_from_login")

	require.NotNil(t, establishSession(t.Context(), testLogger(io.Discard), store, f.server.Client()))

	_, stored, err := store.Load()
	require.NoError(t, err)
	assert.NotContains(t, stored, "eyJ.fresh.access")
}

// ---------- failures the daemon must survive ----------

// A daemon that refuses to start because nobody has logged in cannot be installed before its first login,
// and `norite daemon install` deliberately runs before anything else (M3).
func TestNoStoredCredentialIsNotAFailure(t *testing.T) {
	f := newFakeInstance(t)
	store, err := credentials.OpenIn(t.TempDir())
	require.NoError(t, err)
	logs := &strings.Builder{}

	assert.Nil(t, establishSession(t.Context(), testLogger(logs), store, f.server.Client()))
	assert.Zero(t, f.calls, "with nothing stored there is nothing to present")
	assert.Contains(t, logs.String(), "norite login", "the log must say how to fix it")
}

// An unreachable instance is the same situation for a different reason: keep running and say so, rather
// than exit and let the service manager restart into the same failure.
func TestAnUnreachableInstanceLeavesTheDaemonRunning(t *testing.T) {
	store := storedSession(t, "http://127.0.0.1:1", "nrt_from_login")
	logs := &strings.Builder{}

	assert.Nil(t, establishSession(t.Context(), testLogger(logs), store, &http.Client{}))
	assert.Contains(t, logs.String(), "could not renew the stored session")
}

// A refused token is the ordinary outcome of a password reset, a logout elsewhere, or reuse detection. The
// log has to point at the fix without repeating the instance's deliberately uniform 401.
func TestARefusedTokenSaysWhatToDo(t *testing.T) {
	f := newFakeInstance(t)
	f.status = http.StatusUnauthorized
	f.body = `{"error":{"code":"unauthorized","message":"invalid or expired refresh token"}}`
	store := storedSession(t, f.server.URL, "nrt_stale")
	logs := &strings.Builder{}

	assert.Nil(t, establishSession(t.Context(), testLogger(logs), store, f.server.Client()))
	assert.Contains(t, logs.String(), "norite login` again")

	// The stored credential is left alone. It is already useless, but clearing it here would turn a
	// transient instance-side problem into a lost session the person has to notice and redo.
	_, stored, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "nrt_stale", stored)
}

// Half a pair would leave the next start holding a token the instance has already rotated away from, with
// no way back except a fresh login.
func TestAnIncompletePairIsRefusedAndNothingIsStored(t *testing.T) {
	f := newFakeInstance(t)
	f.status = http.StatusOK
	f.body = `{"access_token":"eyJ.a.b"}`
	store := storedSession(t, f.server.URL, "nrt_from_login")

	assert.Nil(t, establishSession(t.Context(), testLogger(io.Discard), store, f.server.Client()))

	_, stored, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "nrt_from_login", stored, "the original must survive a response that made no sense")
}

// ---------- rule 8 ----------

// The log is the one artifact from all of this that gets pasted into a bug report.
func TestNoTokenEverReachesTheLog(t *testing.T) {
	f := newFakeInstance(t)
	store := storedSession(t, f.server.URL, "nrt_from_login")
	logs := &strings.Builder{}

	require.NotNil(t, establishSession(t.Context(), testLogger(logs), store, f.server.Client()))

	for _, secret := range []string{"nrt_from_login", "nrt_rotated", "eyJ.fresh.access"} {
		assert.NotContains(t, logs.String(), secret, "%q must never be logged", secret)
	}
}

// ...including on the paths where an error carries the request that held it. url.Error renders the full
// URL, and a wrapped transport error is one library change away from rendering a body with it.
func TestAFailedRefreshLogsNoToken(t *testing.T) {
	store := storedSession(t, "http://127.0.0.1:1", "nrt_from_login")
	logs := &strings.Builder{}

	assert.Nil(t, establishSession(t.Context(), testLogger(logs), store, &http.Client{}))
	assert.NotContains(t, logs.String(), "nrt_from_login")
}
