package login

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

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
	// meUsername overrides the name the instance claims the account has. Empty means "ada".
	meUsername string
	// logins counts calls to the login endpoint, so a test can assert nothing reached the instance.
	logins int

	// twoFactor makes the login answer 202 with a challenge instead of a pair, as an account with a
	// second factor does.
	twoFactor bool
	// acceptCode is the only code the verify endpoint accepts. Empty means "accept any non-empty code".
	acceptCode string
	// lastVerify is what the verify endpoint received, and verifies counts the attempts.
	lastVerify twoFactorVerifyRequest
	verifies   int
}

func newFakeInstance(t *testing.T) *fakeInstance {
	t.Helper()
	f := &fakeInstance{}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		f.logins++
		_ = json.NewDecoder(r.Body).Decode(&f.lastLogin)
		if f.loginStatus != 0 {
			w.WriteHeader(f.loginStatus)
			_, _ = w.Write([]byte(f.loginBody))
			return
		}
		if f.twoFactor {
			// The shape the backend actually sends: a challenge, an expiry, and no token of any kind.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"two_factor_required": true, "challenge": "eyJ.challenge.token",
				"expires_at": "2026-01-01T00:05:00Z",
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "eyJ.access.token", "refresh_token": "nrt_stored",
			"token_type": "Bearer", "expires_at": "2026-01-01T00:00:00Z",
		})
	})
	mux.HandleFunc("/api/v1/auth/2fa/verify", func(w http.ResponseWriter, r *http.Request) {
		f.verifies++
		_ = json.NewDecoder(r.Body).Decode(&f.lastVerify)
		if f.acceptCode != "" && f.lastVerify.Code != f.acceptCode {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"that code is not valid"}}`))
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
		username := f.meUsername
		if username == "" {
			username = "ada"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "123456789", "username": username, "email": "ada@example.com",
		})
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// testRunner wires a Runner to a temp store and a fake instance, with every prompt answered in advance.
func testRunner(t *testing.T, f *fakeInstance, opts Options) (*Runner, *credentials.Store, *bytes.Buffer) {
	t.Helper()

	store, err := credentials.OpenLocalForTest(t.TempDir())
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

		// Pinned here rather than left to the machine, and it is the default every test in this package
		// starts from — one that wants the other answer says so.
		//
		// From M9 a provider sign-in checks whether a browser can be reached and takes the device-code
		// path when it cannot, so an unpinned suite quietly tests a different flow on a runner with no
		// DISPLAY than on a desktop. That is not hypothetical: it turned fourteen of M8's tests red in CI
		// while every one of them passed locally.
		browserReachable: func() bool { return true },
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

// A logout must not cost the installation its identity. It used to: the ID lived in the record file, which
// a logout removes, so the next login minted a new one — a second entry in the account's session list,
// beside a refresh family still live, since logging out locally revokes nothing on the instance.
func TestTheDeviceIdentitySurvivesALogout(t *testing.T) {
	f := newFakeInstance(t)
	runner, store, _ := testRunner(t, f, Options{})

	require.NoError(t, runner.Run(t.Context()))
	before := f.lastLogin.DeviceID
	require.NotEmpty(t, before)

	require.NoError(t, store.Clear())
	require.NoError(t, runner.Run(t.Context()))

	assert.Equal(t, before, f.lastLogin.DeviceID)
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
	fresh, err := credentials.OpenLocalForTest(t.TempDir())
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

// ---------- reading a line ----------

// This started as fmt.Fscanln, which was wrong twice over: it fails with "unexpected newline" on an empty
// line, so pressing Enter at the prompt produced a scanner error instead of "an email address is required";
// and it stops at the first space, so a mistyped address silently became its first word.
func TestTheLineReaderHandlesWhatPeopleActuallyType(t *testing.T) {
	for name, tc := range map[string]struct {
		input   string
		want    string
		wantErr bool
	}{
		"an ordinary answer":     {input: "ada@example.com\n", want: "ada@example.com"},
		"surrounding space":      {input: "  ada@example.com  \n", want: "ada@example.com"},
		"just pressing enter":    {input: "\n", want: ""},
		"a space in the middle":  {input: "ada example\n", want: "ada example"},
		"no trailing newline":    {input: "ada@example.com", want: "ada@example.com"},
		"stdin closed with none": {input: "", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			read := lineReader(strings.NewReader(tc.input), io.Discard)
			got, err := read("Email: ")
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// An empty line reaches the flow as an empty answer, so the flow's own message is what a person sees.
func TestPressingEnterAtTheEmailPromptSaysWhatIsMissing(t *testing.T) {
	f := newFakeInstance(t)
	runner, _, _ := testRunner(t, f, Options{})
	runner.ReadLine = lineReader(strings.NewReader("\n"), io.Discard)

	err := runner.Run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "email address is required")
	assert.Zero(t, f.lastLogin.Email, "nothing may reach the instance")
}

// A missing email and a missing password are the same class of problem — an input a script did not supply —
// and must be reported the same way, so a caller can tell them apart from wrong credentials by exit code
// alone. They were not: the email case exited 1 with the "norite:" prefix while the password case exited 2
// without it.
func TestEveryUnanswerableQuestionReportsTheSameWay(t *testing.T) {
	for name, setup := range map[string]func(*Runner){
		"no email":    func(r *Runner) { r.Options.Email = "" },
		"no password": func(r *Runner) { r.Options.Email = "ada@example.com" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeInstance(t)
			runner, _, _ := testRunner(t, f, Options{})
			runner.Interactive = false
			t.Setenv(passwordEnvVar, "")
			setup(runner)

			err := runner.Run(t.Context())
			require.ErrorIs(t, err, ErrNoTerminal, "main matches on this to choose exit code 2")
			assert.Contains(t, err.Error(), "terminal")
		})
	}
}

// ...and each still says what would actually have answered it, since the two are fixed differently.
func TestEachUnanswerableQuestionSaysWhatWouldAnswerIt(t *testing.T) {
	f := newFakeInstance(t)

	runner, _, _ := testRunner(t, f, Options{})
	runner.Interactive = false
	t.Setenv(passwordEnvVar, "")
	err := runner.Run(t.Context())
	assert.Contains(t, err.Error(), "--email")

	runner.Options.Email = "ada@example.com"
	err = runner.Run(t.Context())
	assert.Contains(t, err.Error(), passwordEnvVar)
}

// ---------- the stored instance URL is not trusted ----------

// LoadRecord does not validate, so nothing re-checks a record that was hand-edited or written by an older
// build — and this value decides where the password is POSTed. `https://user:pass@evil.example` is exactly
// the shape ParseInstanceURL refuses from a flag or the environment, and it used to be handed back from the
// store verbatim.
func TestAStoredInstanceURLIsRevalidatedBeforeThePasswordIsSent(t *testing.T) {
	f := newFakeInstance(t)
	dir := t.TempDir()
	store, err := credentials.OpenLocalForTest(dir)
	require.NoError(t, err)

	runner, _, _ := testRunner(t, f, Options{})
	runner.Store = store
	require.NoError(t, runner.Run(t.Context()))
	require.Equal(t, 1, f.logins)

	// Hand-edit the record, the way a person or an older build could. "account.json" is the record file;
	// if that ever changes this test fails loudly, which is the right outcome.
	poisoned := `{"instance_url":"https://ada:hunter2@evil.example.com","user_id":"1","username":"ada",` +
		`"device_id":"dev_x","device_name":"laptop"}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "account.json"), []byte(poisoned), 0o600))

	runner.Options.Instance = ""
	t.Setenv(instanceEnvVar, "")
	err = runner.Run(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "username or password")
	assert.Equal(t, 1, f.logins, "no password may leave the machine for an unvalidated host")
}

// An unreadable record fails the login at the first step, with the way out, rather than at whichever
// question happened to read it second.
func TestAnUnreadableRecordFailsTheLoginWithTheWayOut(t *testing.T) {
	f := newFakeInstance(t)
	dir := t.TempDir()
	store, err := credentials.OpenLocalForTest(dir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "account.json"), []byte("{not json"), 0o600))

	runner, _, _ := testRunner(t, f, Options{})
	runner.Store = store

	err = runner.Run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "norite logout")
	assert.Zero(t, f.logins, "nothing may reach the instance")
}

// ---------- nothing an instance says may act on the terminal ----------

// A terminal executes what it is printed, and `--instance` is a URL somebody handed the person running the
// command. `ESC [ 2 K CR` erases the line it was written on and rewrites it, so a name chosen by whatever
// answered can replace what the CLI just said about it (CLAUDE.md rule 19).
func TestAUsernameFromTheInstanceCannotActOnTheTerminal(t *testing.T) {
	f := newFakeInstance(t)
	f.meUsername = "\x1b[2K\rada\x1b[31m (verified admin)\x1b]0;retitled\x07"

	runner, store, out := testRunner(t, f, Options{})
	require.NoError(t, runner.Run(t.Context()))

	assertInertOnATerminal(t, out.String())
	assert.Contains(t, out.String(), "ada", "sanitizing must not destroy the name itself")

	// ...and the stored copy is clean too, because it is cleaned as it enters rather than as it is printed:
	// `norite logout` prints it back later, and the daemon logs it at every start.
	record, _, err := store.Load()
	require.NoError(t, err)
	assertInertOnATerminal(t, record.Username)
}

// The most direct route from a stranger's server to a terminal: this message is printed verbatim behind a
// "norite:" prefix. A 401 is replaced by the CLI's own wording, so an instance that wants to be heard has
// to answer with something else.
func TestAnErrorFromTheInstanceCannotActOnTheTerminal(t *testing.T) {
	f := newFakeInstance(t)
	f.loginStatus = http.StatusTooManyRequests
	f.loginBody = `{"error":{"code":"rate_limited","message":"slow down\u001b[2K\rSigned in as admin"}}`

	runner, _, _ := testRunner(t, f, Options{})
	err := runner.Run(t.Context())

	require.Error(t, err)
	assertInertOnATerminal(t, err.Error())
	assert.Contains(t, err.Error(), "slow down")
}

// A response that is not this API's error shape at all — a proxy, a captive portal — is reported by status,
// and the reason phrase in that status is the server's text like everything else.
func TestAnUnrecognizedResponseIsReportedWithoutItsEscapeSequences(t *testing.T) {
	f := newFakeInstance(t)
	f.loginStatus = http.StatusBadGateway
	f.loginBody = "<html>not an API</html>"

	runner, _, _ := testRunner(t, f, Options{})
	err := runner.Run(t.Context())

	require.Error(t, err)
	assertInertOnATerminal(t, err.Error())
	assert.Contains(t, err.Error(), "not a Norite API response")
}

// A name that reorders what follows it is the same lie as one that erases the line, reached by a different
// mechanism — and a terminal that implements bidi will happily print it.
func TestAUsernameCannotReorderWhatIsPrinted(t *testing.T) {
	f := newFakeInstance(t)
	f.meUsername = "ada\u202enimda"

	runner, _, out := testRunner(t, f, Options{})
	require.NoError(t, runner.Run(t.Context()))

	assertInertOnATerminal(t, out.String())
}

// A megabyte of "username" would push everything the command said off the screen, which is what an
// erase-line sequence is for. The cut is marked, so a name shortened to `ada` cannot read as `ada`.
func TestAnEnormousUsernameIsCutAndMarked(t *testing.T) {
	f := newFakeInstance(t)
	f.meUsername = strings.Repeat("a", 4000)

	runner, store, out := testRunner(t, f, Options{})
	require.NoError(t, runner.Run(t.Context()))

	record, _, err := store.Load()
	require.NoError(t, err)
	assert.Less(t, len([]rune(record.Username)), 200, "an unbounded name reaches the record and the screen")
	assert.True(t, strings.HasSuffix(record.Username, "…"), "a silent cut renders as a different name")
	assert.Less(t, len(out.String()), 1000)
}

// assertInertOnATerminal fails if s carries anything a terminal would act on rather than print.
// Spelled out from the Unicode tables rather than by calling termsafe, so that this asserts the property
// the login path needs rather than agreeing with whatever the sanitizer currently does.
func assertInertOnATerminal(t *testing.T, s string) {
	t.Helper()
	for i, r := range s {
		switch {
		case r == '\n', r == '\t':
			// The whole output is checked at once in places, and it has lines — and Block, which every
			// error passes through on its way to stderr, keeps tabs as well.
		case unicode.Is(unicode.Cc, r):
			t.Errorf("%q carries %U at byte %d, which a terminal would act on", s, r, i)
		case unicode.Is(unicode.Bidi_Control, r) && r != '\u061c' && r != '\u200e' && r != '\u200f':
			t.Errorf("%q carries %U at byte %d, which reorders what is printed", s, r, i)
		}
	}
	if !utf8.ValidString(s) {
		t.Errorf("%q is not valid UTF-8, so a terminal decoding bytes sees whatever byte was sent", s)
	}
}

// ---------- M11a's follow-up: a sign-in that owes a second factor ----------

// The gap this closes: the backend answered 202 and the CLI decoded it into an empty token pair, reporting
// `the instance returned an incomplete token pair` — a message meaning "this is not a Norite API", which
// blamed the instance for the client's own missing step.
func TestALoginThatOwesASecondFactorPromptsForACode(t *testing.T) {
	f := newFakeInstance(t)
	f.twoFactor = true
	f.acceptCode = "123456"

	runner, store, out := testRunner(t, f, Options{})
	runner.ReadLine = answers("ada@example.com", "123456")

	require.NoError(t, runner.Run(t.Context()))

	assert.Equal(t, "eyJ.challenge.token", f.lastVerify.Challenge,
		"the challenge must be carried back unchanged")
	assert.Equal(t, "123456", f.lastVerify.Code)

	// And the session is the one the second call returned, stored as any other.
	record, err := store.LoadRecord()
	require.NoError(t, err)
	assert.Equal(t, "ada", record.Username)
	assert.Contains(t, out.String(), "second factor")
}

// A recovery code is typed at the same prompt, because the instance accepts both at one endpoint and
// sending somebody to a different command at the moment they have lost their phone is the one time it
// must not happen.
func TestARecoveryCodeIsAcceptedAtTheSamePrompt(t *testing.T) {
	f := newFakeInstance(t)
	f.twoFactor = true
	f.acceptCode = "BCDFGHJKMN"

	runner, _, _ := testRunner(t, f, Options{})
	runner.ReadLine = answers("ada@example.com", "BCDFGHJKMN")

	require.NoError(t, runner.Run(t.Context()))
	assert.Equal(t, "BCDFGHJKMN", f.lastVerify.Code)
}

// A mistyped code is the ordinary mistake — the number rolls over while somebody is reading it — so it is
// worth a second prompt rather than a re-run that asks for the password again.
func TestAWrongCodeIsAskedForAgain(t *testing.T) {
	f := newFakeInstance(t)
	f.twoFactor = true
	f.acceptCode = "654321"

	runner, store, _ := testRunner(t, f, Options{})
	runner.ReadLine = answers("ada@example.com", "000000", "654321")

	require.NoError(t, runner.Run(t.Context()))
	assert.Equal(t, 2, f.verifies, "the first code was refused and the second accepted")
	assert.Equal(t, 1, f.logins, "and the password was not sent again")

	_, err := store.LoadRecord()
	require.NoError(t, err)
}

// Bounded, though. Every attempt spends the instance's auth rate limit, and a prompt that never gives up
// turns a challenge nobody can answer into a loop nobody can read their way out of.
func TestTheCodePromptGivesUpEventually(t *testing.T) {
	f := newFakeInstance(t)
	f.twoFactor = true
	f.acceptCode = "654321"

	runner, store, _ := testRunner(t, f, Options{})
	runner.ReadLine = answers("ada@example.com", "000000", "111111", "222222", "654321")

	err := runner.Run(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBadFactorCode)
	assert.Equal(t, maxFactorAttempts, f.verifies, "it must stop rather than keep asking")

	_, loadErr := store.LoadRecord()
	assert.ErrorIs(t, loadErr, credentials.ErrNoCredential, "a refused sign-in stores nothing")
}

// Nothing to type into means saying so, with the flow that needs no terminal — never blocking on input
// that will never arrive, and never a bare failure that reads as the instance's fault.
func TestACodeCannotBeAskedForWithoutATerminal(t *testing.T) {
	f := newFakeInstance(t)
	f.twoFactor = true

	runner, _, _ := testRunner(t, f, Options{Email: "ada@example.com"})
	runner.Interactive = false
	t.Setenv(passwordEnvVar, "a correct passphrase")

	err := runner.Run(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoTerminal)
	assert.Contains(t, err.Error(), "--device-code", "the way out has to be named")
	assert.Zero(t, f.verifies, "and nothing was guessed at")
}

// A 202 carrying no challenge is not a sign-in the CLI can complete, and must not be reported as one.
func TestAChallengelessAcceptedResponseIsRefused(t *testing.T) {
	f := newFakeInstance(t)
	f.loginStatus = http.StatusAccepted
	f.loginBody = `{"two_factor_required":true}`

	runner, _, _ := testRunner(t, f, Options{})
	err := runner.Run(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no challenge")
	assert.Zero(t, f.verifies)
}

// answers replies to successive prompts in order, failing the test if one more is asked for than planned.
//
// The prompts are ordered rather than keyed on their text: the email prompt and the code prompt are two
// reads from the same reader, and a test that matched on wording would keep passing if the two were
// swapped.
func answers(replies ...string) func(string) (string, error) {
	i := 0
	return func(string) (string, error) {
		if i >= len(replies) {
			return "", fmt.Errorf("unexpected prompt %d: only %d answers were planned", i+1, len(replies))
		}
		reply := replies[i]
		i++
		return reply, nil
	}
}
