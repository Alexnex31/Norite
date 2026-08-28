package daemonproc

import (
	"cmp"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/credentials"
)

// M7's "done when" is that the daemon uses the stored token on its next launch without re-prompting. These
// drive that directly: a credential on disk, a stand-in instance, and the refresh the daemon performs when
// it starts.

// fakeInstance stands in for the backend's refresh endpoint.
//
// The fields the handler writes are guarded, because the handler runs on httptest's goroutine and the
// assertions read from the test's. For a request that gets a response the HTTP round trip orders the two,
// but the logoutBroken case answers by hijacking and closing the connection — the ordering is then a TCP
// close observed as EOF, which is real but invisible to `go test -race`. `just test` does not currently
// pass -race, so this is not what makes the suite green; it is what stops a future -race run reporting a
// race in the harness rather than in the code.
type fakeInstance struct {
	mu     sync.Mutex
	server *httptest.Server

	// received is the refresh token the daemon presented.
	received string
	// status and body override the response for the failure paths.
	status int
	body   string
	// calls counts refresh requests, so a test can assert the daemon did not call at all.
	calls int
	// handedBack is the token presented to /auth/logout, which is how a test sees whether the daemon gave
	// back the credential it could not keep. Empty means it did not.
	handedBack string
	// logoutStatus overrides the revoke response, for the path where the instance refuses it.
	logoutStatus int
	// logoutBroken drops the connection instead of answering the revoke, which is what a transport
	// failure looks like to the client. Closing the whole server from inside a handler cannot be used for
	// this: Close waits for the in-flight request, so it deadlocks — and it would break the refresh too,
	// which has to succeed for the token being handed back to exist at all.
	logoutBroken bool
	// beforeRefresh runs inside the handler, which is the one place a test can act while the daemon is
	// between reading the record and writing it back.
	beforeRefresh func()
}

func newFakeInstance(t *testing.T) *fakeInstance {
	t.Helper()
	f := &fakeInstance{}

	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RefreshToken string `json:"refresh_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		// The daemon hands back a token it obtained and could not keep (M11). Handled here rather than in
		// a second stub, so a test can see the refresh and the revocation in the order they happened.
		if r.URL.Path == "/api/v1/auth/logout" {
			f.mu.Lock()
			f.handedBack = body.RefreshToken
			f.mu.Unlock()
			if f.logoutBroken {
				conn, _, err := w.(http.Hijacker).Hijack()
				require.NoError(t, err)
				_ = conn.Close()
				return
			}
			w.WriteHeader(cmp.Or(f.logoutStatus, http.StatusNoContent))
			return
		}

		f.mu.Lock()
		f.calls++
		f.received = body.RefreshToken
		f.mu.Unlock()
		require.Equal(t, "/api/v1/auth/refresh", r.URL.Path)

		if f.beforeRefresh != nil {
			f.beforeRefresh()
		}

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

// handedBackToken reports the token presented to /auth/logout, or empty if none was.
func (f *fakeInstance) handedBackToken() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.handedBack
}

// receivedToken reports the refresh token the daemon presented, and how many times it asked.
func (f *fakeInstance) receivedToken() (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.received, f.calls
}

// storedSession writes a credential the way `norite login` would.
func storedSession(t *testing.T, instanceURL, refreshToken string) *credentials.Store {
	t.Helper()
	return storedSessionIn(t, t.TempDir(), instanceURL, refreshToken)
}

// storedSessionIn is storedSession for a test that needs the directory too — one record shape rather than
// two, so a field added to credentials.Record (M7 added SecretBackend) is added in one place.
func storedSessionIn(t *testing.T, dir, instanceURL, refreshToken string) *credentials.Store {
	t.Helper()
	store, err := credentials.OpenLocalForTest(dir)
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
	presented, _ := f.receivedToken()
	assert.Equal(t, "nrt_from_login", presented, "the daemon presents what the login stored")
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
	presented, _ := f.receivedToken()
	assert.Equal(t, "nrt_rotated", presented)
}

// A login or a logout that lands while the refresh is in flight owns the store, and the daemon must leave
// it alone. Save would not have: reading the record before a network round trip and writing it back after
// is a read-modify-write across a released lock, so a stale record would delete the token the login had
// just stored and put the old instance back in account.json.
func TestALoginDuringTheRefreshIsNotUndone(t *testing.T) {
	f := newFakeInstance(t)
	store := storedSession(t, f.server.URL, "nrt_from_login")
	logs := &strings.Builder{}

	// What `norite login` to another instance would leave behind, mid-flight.
	f.beforeRefresh = func() {
		require.NoError(t, store.Save(credentials.Record{
			InstanceURL: "https://other.example.com",
			UserID:      "987654321",
			Username:    "grace",
			DeviceID:    "dev_test",
			DeviceName:  "laptop",
		}, "nrt_the_login_just_stored"))
	}

	sess := establishSession(t.Context(), testLogger(logs), store, f.server.Client())
	assert.Nil(t, sess, "the session renewed is not the one this machine holds any more")

	record, token, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "https://other.example.com", record.InstanceURL, "the login's record must survive")
	assert.Equal(t, "nrt_the_login_just_stored", token, "the login's token must survive")
	assert.Contains(t, logs.String(), "leaving it alone")
}

// A store that refuses the write-back still holds the token that was just spent. Presenting a rotated token
// is what M4 reads as theft, and it revokes every session on this device family — for a full disk. Clearing
// costs one `norite login` and takes nothing else down with it.
func TestASpentTokenIsClearedRatherThanLeftToLookStolen(t *testing.T) {
	f := newFakeInstance(t)
	dir := t.TempDir()
	store := storedSessionIn(t, dir, f.server.URL, "nrt_from_login")

	// The state directory goes read-only, so the write-back cannot land.
	if runtime.GOOS == "windows" {
		t.Skip("Unix directory modes do not describe Windows ACLs")
	}
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	logs := &strings.Builder{}
	sess := establishSession(t.Context(), testLogger(logs), store, f.server.Client())
	assert.Nil(t, sess)
	assert.Contains(t, logs.String(), "could not be stored")
}

// A lock the daemon could not take says nothing about what is on disk, and the likeliest holder of it is a
// `norite login` writing a fresh credential — a keyring prompt can hold it past the five-second wait. This
// used to fall into the "the write was refused, so the store holds a spent token" branch and clear it, which
// logged the person out of the session they had just been told they were signed in to.
func TestAnUnreadableStoreDoesNotCostTheCredential(t *testing.T) {
	f := newFakeInstance(t)
	dir := t.TempDir()
	store := storedSessionIn(t, dir, f.server.URL, "nrt_from_login")

	// Held for longer than the daemon is willing to wait, the way another process would.
	held := flock.New(filepath.Join(dir, "credentials.lock"))
	f.beforeRefresh = func() { require.NoError(t, held.Lock()) }
	t.Cleanup(func() { _ = held.Unlock() })

	logs := &strings.Builder{}
	assert.Nil(t, establishSession(t.Context(), testLogger(logs), store, f.server.Client()))
	require.NoError(t, held.Unlock())

	record, token, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, f.server.URL, record.InstanceURL, "the credential must still be there")
	assert.Equal(t, "nrt_from_login", token)
	assert.Contains(t, logs.String(), "may now be spent")

	// And the token this refresh obtained is handed back rather than dropped. It is the third of the three
	// branches that discard one, and the only one M11 shipped without the hand-back: what forbids clearing
	// here is that the stored credential may be somebody else's fresh one, which says nothing about a token
	// minted to this process moments ago that has never been written anywhere for anyone else to hold.
	assert.Equal(t, "nrt_rotated", f.handedBackToken(),
		"a renewed token no one can hold must be revoked, not left live for its full TTL")
}

// A writer that is merely finishing is waited out by the lock itself — that is what its five-second bound
// is for — so the write-back lands without any retry loop above it. The test store waits 100ms, so the
// hold here is proportionally brief in the same way a real one is against five seconds.
func TestARenewalWaitsOutABrieflyHeldLock(t *testing.T) {
	f := newFakeInstance(t)
	dir := t.TempDir()
	store := storedSessionIn(t, dir, f.server.URL, "nrt_from_login")

	held := flock.New(filepath.Join(dir, "credentials.lock"))
	f.beforeRefresh = func() {
		require.NoError(t, held.Lock())
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = held.Unlock()
		}()
	}

	require.NotNil(t, establishSession(t.Context(), testLogger(io.Discard), store, f.server.Client()))

	_, token, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "nrt_rotated", token, "a lock held briefly must not cost the renewed token")
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
	store, err := credentials.OpenLocalForTest(t.TempDir())
	require.NoError(t, err)
	logs := &strings.Builder{}

	assert.Nil(t, establishSession(t.Context(), testLogger(logs), store, f.server.Client()))
	_, calls := f.receivedToken()
	assert.Zero(t, calls, "with nothing stored there is nothing to present")
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

// The token a colliding login makes unkeepable is handed back, not abandoned (M11).
//
// Before this, the daemon dropped it and the instance kept it live for the full refresh TTL — thirty days
// of a valid credential in nobody's hands. The code said handing it back needed M11's primitive and M19's
// gateway connection; neither was true. POST /auth/logout has revoked exactly the session a presented
// refresh token belongs to since M4, and the client to call it with is the one that just refreshed.
func TestADroppedTokenIsHandedBack(t *testing.T) {
	f := newFakeInstance(t)
	store := storedSession(t, f.server.URL, "nrt_from_login")
	logs := &strings.Builder{}

	f.beforeRefresh = func() {
		require.NoError(t, store.Save(credentials.Record{
			InstanceURL: "https://other.example.com",
			UserID:      "987654321",
			Username:    "grace",
			DeviceID:    "dev_test",
			DeviceName:  "laptop",
		}, "nrt_the_login_just_stored"))
	}

	sess := establishSession(t.Context(), testLogger(logs), store, f.server.Client())
	require.Nil(t, sess)

	assert.Equal(t, "nrt_rotated", f.handedBackToken(),
		"the token the daemon obtained and could not keep must be revoked, not left live")
	assert.Contains(t, logs.String(), "revoked the renewed credential")

	// And it revoked the right one. The login's token is what this machine is holding now; handing *that*
	// back would sign somebody out of the session they had just created.
	record, token, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "nrt_the_login_just_stored", token, "the login's token must be untouched")
	assert.Equal(t, "https://other.example.com", record.InstanceURL)
	assert.NotEqual(t, token, f.handedBackToken())
}

// It goes to the instance the dropped token came from, not the one on disk now.
//
// A colliding `norite login` may have pointed the store at an entirely different instance — which is
// exactly what TestALoginDuringTheRefreshIsNotUndone sets up. Reading the URL back off disk at this point
// would present one instance's refresh token to another.
func TestADroppedTokenGoesBackToItsOwnInstance(t *testing.T) {
	f := newFakeInstance(t)
	other := newFakeInstance(t)
	store := storedSession(t, f.server.URL, "nrt_from_login")

	f.beforeRefresh = func() {
		require.NoError(t, store.Save(credentials.Record{
			InstanceURL: other.server.URL,
			UserID:      "987654321",
			Username:    "grace",
			DeviceID:    "dev_test",
			DeviceName:  "laptop",
		}, "nrt_the_login_just_stored"))
	}

	establishSession(t.Context(), testLogger(io.Discard), store, f.server.Client())

	assert.Equal(t, "nrt_rotated", f.handedBackToken(), "the issuing instance is the one told to revoke it")
	assert.Empty(t, other.handedBackToken(), "the instance the login switched to never saw this token")
}

// A hand-back that fails is logged and survived. The daemon is starting, and a token it could not revoke
// leaves precisely the situation that existed before this was implemented — refusing to start over it
// would turn a tidy-up into an outage.
func TestAFailedHandBackDoesNotStopTheDaemon(t *testing.T) {
	for name, arrange := range map[string]func(*fakeInstance){
		"the instance refuses it":       func(f *fakeInstance) { f.logoutStatus = http.StatusInternalServerError },
		"the connection drops under it": func(f *fakeInstance) { f.logoutBroken = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeInstance(t)
			store := storedSession(t, f.server.URL, "nrt_from_login")
			logs := &strings.Builder{}

			// Arranged before the refresh runs, not inside the handler: the refresh has to succeed for
			// there to be a token to hand back, and only the revocation may fail.
			arrange(f)
			f.beforeRefresh = func() {
				require.NoError(t, store.Save(credentials.Record{
					InstanceURL: "https://other.example.com", UserID: "987654321",
					Username: "grace", DeviceID: "dev_test", DeviceName: "laptop",
				}, "nrt_the_login_just_stored"))
			}

			// Reaching this line at all is the assertion: establishSession must return rather than fail.
			sess := establishSession(t.Context(), testLogger(logs), store, f.server.Client())
			assert.Nil(t, sess)
			assert.Contains(t, logs.String(), "leaving it alone", "the collision is still reported")

			_, token, err := store.Load()
			require.NoError(t, err)
			assert.Equal(t, "nrt_the_login_just_stored", token, "and the login's credential is intact")
		})
	}
}

// Rule 8, on the path M11 added: a revocation request carries a refresh token, and none of it may reach
// the log — not on success, not when the instance refuses, not when it cannot be reached.
func TestHandingBackATokenLogsNoToken(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusInternalServerError} {
		f := newFakeInstance(t)
		f.logoutStatus = status
		store := storedSession(t, f.server.URL, "nrt_from_login")
		logs := &strings.Builder{}

		f.beforeRefresh = func() {
			require.NoError(t, store.Save(credentials.Record{
				InstanceURL: "https://other.example.com", UserID: "987654321",
				Username: "grace", DeviceID: "dev_test", DeviceName: "laptop",
			}, "nrt_the_login_just_stored"))
		}

		establishSession(t.Context(), testLogger(logs), store, f.server.Client())

		for _, secret := range []string{"nrt_rotated", "nrt_from_login", "nrt_the_login_just_stored"} {
			assert.NotContains(t, logs.String(), secret, "status %d leaked %s", status, secret)
		}
	}
}
