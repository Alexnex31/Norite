package login

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/credentials"
)

// A stand-in instance. Real HTTP over a loopback listener rather than a mocked client: the request shape —
// the path, the JSON body, the Bearer header on the follow-up call — is exactly what this milestone has to
// get right, and a mock would assert only what the test already believed.
type fakeInstance struct {
	server *httptest.Server

	// lastLogin is what the login endpoint received.
	lastLogin loginRequest
	// bearer is the Authorization header the @me call carried.
	bearer string

	// loginStatus overrides the response, for the failure paths.
	loginStatus int
	loginBody   string
	// meStatus does the same for the identity call.
	meStatus int
}

func newFakeInstance(t *testing.T) *fakeInstance {
	t.Helper()
	f := &fakeInstance{}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&f.lastLogin)
		if f.loginStatus != 0 {
			w.WriteHeader(f.loginStatus)
			_, _ = w.Write([]byte(f.loginBody))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "eyJ.access.token", "refresh_token": "nrt_stored",
			"token_type": "Bearer", "expires_at": "2026-01-01T00:00:00Z",
		})
	})
	mux.HandleFunc("/api/v1/users/@me", func(w http.ResponseWriter, r *http.Request) {
		f.bearer = r.Header.Get("Authorization")
		if f.meStatus != 0 {
			w.WriteHeader(f.meStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "123456789", "username": "ada", "email": "ada@example.com",
		})
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// testRunner wires a Runner to a temp store and a fake instance, with every prompt answered in advance.
func testRunner(t *testing.T, f *fakeInstance, opts Options) (*Runner, *credentials.Store, *bytes.Buffer) {
	t.Helper()

	store, err := credentials.OpenIn(t.TempDir())
	require.NoError(t, err)
	out := &bytes.Buffer{}

	if opts.Instance == "" {
		opts.Instance = f.server.URL
	}
	return &Runner{
		Options:     opts,
		Store:       store,
		Out:         out,
		ReadLine:    func(string) (string, error) { return "ada@example.com", nil },
		ReadSecret:  func(string) (string, error) { return "a correct passphrase", nil },
		Interactive: true,
		Hostname:    func() (string, error) { return "ada-laptop", nil },
		NewDeviceID: credentials.NewDeviceID,
	}, store, out
}

// ---------- the milestone's own criterion ----------

// M7's "done when": a password login succeeds and leaves something the daemon can use next launch.
func TestLoginStoresACredentialTheDaemonCanUse(t *testing.T) {
	f := newFakeInstance(t)
	runner, store, out := testRunner(t, f, Options{})

	require.NoError(t, runner.Run(t.Context()))

	// What the instance was actually sent.
	assert.Equal(t, "ada@example.com", f.lastLogin.Email)
	assert.Equal(t, "a correct passphrase", f.lastLogin.Password)
	assert.NotEmpty(t, f.lastLogin.DeviceID, "a login with no device identity would share a refresh family")
	assert.Equal(t, "ada-laptop", f.lastLogin.DeviceName)
	assert.Equal(t, "Bearer eyJ.access.token", f.bearer)

	// What a later daemon start will find.
	record, token, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "nrt_stored", token)
	assert.Equal(t, f.server.URL, record.InstanceURL)
	assert.Equal(t, "ada", record.Username, "the username comes from the instance, not from what was typed")
	assert.Equal(t, "123456789", record.UserID)
	assert.Equal(t, f.lastLogin.DeviceID, record.DeviceID)

	assert.Contains(t, out.String(), "Signed in as ada")
}

// The access token is not stored. It expires in fifteen minutes, so it would be dead before any restart it
// might have survived — a second credential at rest buying nothing.
func TestOnlyTheRefreshTokenIsStored(t *testing.T) {
	f := newFakeInstance(t)
	runner, store, out := testRunner(t, f, Options{})
	require.NoError(t, runner.Run(t.Context()))

	_, token, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "nrt_stored", token)
	assert.NotContains(t, token, "eyJ.access.token")

	// ...and nothing secret was printed. This output goes to a terminal people paste into bug reports.
	assert.NotContains(t, out.String(), "nrt_stored")
	assert.NotContains(t, out.String(), "eyJ.access.token")
	assert.NotContains(t, out.String(), "a correct passphrase")
}

// ---------- device identity ----------

// A second login keeps the installation's device ID. A fresh one would strand the previous refresh-token
// family until it expired and add a session-list entry per login (ADR 0011).
func TestLoggingInAgainKeepsTheDeviceIdentity(t *testing.T) {
	f := newFakeInstance(t)
	runner, store, _ := testRunner(t, f, Options{})
	require.NoError(t, runner.Run(t.Context()))

	first, _, err := store.Load()
	require.NoError(t, err)

	require.NoError(t, runner.Run(t.Context()))
	second, _, err := store.Load()
	require.NoError(t, err)

	assert.Equal(t, first.DeviceID, second.DeviceID)
	assert.Equal(t, first.DeviceID, f.lastLogin.DeviceID)
}

func TestTheDeviceNameFallsBackToTheHostname(t *testing.T) {
	f := newFakeInstance(t)
	runner, _, _ := testRunner(t, f, Options{})
	require.NoError(t, runner.Run(t.Context()))
	assert.Equal(t, "ada-laptop", f.lastLogin.DeviceName)

	// A machine with no usable hostname is a cosmetic problem in a session list, not a reason to refuse a
	// login.
	runner.Options.DeviceName = ""
	runner.Hostname = func() (string, error) { return "", errors.New("no hostname") }
	fresh, err := credentials.OpenIn(t.TempDir())
	require.NoError(t, err)
	runner.Store = fresh
	require.NoError(t, runner.Run(t.Context()))
	assert.Equal(t, "this device", f.lastLogin.DeviceName)
}

// The backend caps device_name at 64. Trimming here means an over-long name is fixed before the password
// crosses the network, rather than rejected after it.
func TestAnOverLongDeviceNameIsTrimmedBeforeSending(t *testing.T) {
	f := newFakeInstance(t)
	runner, _, _ := testRunner(t, f, Options{DeviceName: strings.Repeat("x", 200)})
	require.NoError(t, runner.Run(t.Context()))
	assert.Len(t, []rune(f.lastLogin.DeviceName), maxDeviceName)
}

// ---------- where to log in ----------

// The instance from a previous login is remembered, so signing in again is one word rather than a URL
// someone has to keep.
func TestTheInstanceIsRememberedFromTheLastLogin(t *testing.T) {
	f := newFakeInstance(t)
	runner, store, _ := testRunner(t, f, Options{})
	require.NoError(t, runner.Run(t.Context()))

	runner.Options.Instance = ""
	require.NoError(t, runner.Run(t.Context()))

	record, _, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, f.server.URL, record.InstanceURL)
}

func TestWithNoInstanceAnywhereTheErrorSaysWhatToPass(t *testing.T) {
	f := newFakeInstance(t)
	runner, _, _ := testRunner(t, f, Options{})
	runner.Options.Instance = ""
	t.Setenv(instanceEnvVar, "")

	err := runner.Run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--instance")
}

func TestTheInstanceCanComeFromTheEnvironment(t *testing.T) {
	f := newFakeInstance(t)
	runner, store, _ := testRunner(t, f, Options{})
	runner.Options.Instance = ""
	t.Setenv(instanceEnvVar, f.server.URL)

	require.NoError(t, runner.Run(t.Context()))
	record, _, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, f.server.URL, record.InstanceURL)
}

// A plain-HTTP instance is a real self-hosted deployment, so it is permitted — but the warning comes before
// the password is asked for, because the point of it is to let someone stop.
func TestPlainHTTPIsWarnedAboutBeforeThePasswordIsAsked(t *testing.T) {
	f := newFakeInstance(t)
	runner, _, out := testRunner(t, f, Options{})

	asked := false
	runner.ReadSecret = func(string) (string, error) {
		asked = true
		assert.Contains(t, out.String(), "plain HTTP",
			"the warning must already be on screen by the time the password is asked for")
		return "a correct passphrase", nil
	}

	require.NoError(t, runner.Run(t.Context()))
	require.True(t, asked)
}

// ---------- refusals ----------

func TestWrongCredentialsAreReportedAsTheInstancePutsThem(t *testing.T) {
	f := newFakeInstance(t)
	f.loginStatus = http.StatusUnauthorized
	f.loginBody = `{"error":{"code":"unauthorized","message":"invalid email or password","request_id":"x"}}`

	runner, store, _ := testRunner(t, f, Options{})
	err := runner.Run(t.Context())
	require.ErrorIs(t, err, ErrBadCredentials)

	// A failed login must not disturb whatever was already stored.
	_, _, loadErr := store.Load()
	assert.ErrorIs(t, loadErr, credentials.ErrNoCredential)
}

// Without a terminal and without the environment variable there is nowhere to get a password. Blocking
// forever, or treating EOF as an empty password, are both worse than saying so.
func TestWithNoTerminalAndNoEnvironmentPasswordItSaysWhatToDo(t *testing.T) {
	f := newFakeInstance(t)
	runner, _, _ := testRunner(t, f, Options{Email: "ada@example.com"})
	runner.Interactive = false
	t.Setenv(passwordEnvVar, "")

	err := runner.Run(t.Context())
	require.ErrorIs(t, err, ErrNoTerminal)
	assert.Contains(t, err.Error(), passwordEnvVar)
}

// The scripted path: a password from the environment, never a flag — a flag value is visible in the process
// list to every other user on the machine.
func TestAPasswordCanComeFromTheEnvironment(t *testing.T) {
	f := newFakeInstance(t)
	runner, _, _ := testRunner(t, f, Options{Email: "ada@example.com"})
	runner.Interactive = false
	runner.ReadSecret = func(string) (string, error) {
		t.Fatal("the terminal must not be consulted when the environment supplies a password")
		return "", nil
	}
	t.Setenv(passwordEnvVar, "from the environment")

	require.NoError(t, runner.Run(t.Context()))
	assert.Equal(t, "from the environment", f.lastLogin.Password)
}

// An empty password is refused here rather than sent: the instance would answer with the same deliberately
// vague 401 it gives a wrong one, which reads as "your password is wrong" to someone who pressed Enter too
// early.
func TestAnEmptyPasswordIsRefusedWithoutAskingTheInstance(t *testing.T) {
	f := newFakeInstance(t)
	runner, _, _ := testRunner(t, f, Options{Email: "ada@example.com"})
	runner.ReadSecret = func(string) (string, error) { return "", nil }

	require.Error(t, runner.Run(t.Context()))
	assert.Empty(t, f.lastLogin.Email, "nothing may reach the instance")
}

// A URL that is not a Norite instance — a proxy, a captive portal — answers something this cannot parse.
// Saying so beats a JSON decode error nobody can act on.
func TestANonNoriteResponseIsReportedAsSuch(t *testing.T) {
	f := newFakeInstance(t)
	f.loginStatus = http.StatusBadGateway
	f.loginBody = "<html><body>502 Bad Gateway</body></html>"

	runner, _, _ := testRunner(t, f, Options{})
	err := runner.Run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a Norite API response")
	assert.NotContains(t, err.Error(), "<html>", "a proxy's HTML must not be pasted onto the terminal")
}

// The identity call is a nicety. Losing it must not lose the session, which is already valid.
func TestAFailedIdentityLookupStillCompletesTheLogin(t *testing.T) {
	f := newFakeInstance(t)
	f.meStatus = http.StatusInternalServerError

	runner, store, _ := testRunner(t, f, Options{})
	require.NoError(t, runner.Run(t.Context()))

	record, token, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "nrt_stored", token)
	assert.Equal(t, "ada@example.com", record.Username, "falls back to the address that was typed")
}

// An incomplete 200 means this is not the API it claims to be. Better said here than as a confusing failure
// when the daemon later tries to use half a pair.
func TestAnIncompleteTokenPairIsRejected(t *testing.T) {
	f := newFakeInstance(t)
	f.loginStatus = http.StatusOK
	f.loginBody = `{"access_token":"eyJ.a.b","token_type":"Bearer"}`

	runner, _, _ := testRunner(t, f, Options{})
	err := runner.Run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "incomplete token pair")
}
