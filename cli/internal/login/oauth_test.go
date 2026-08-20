package login

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/credentials"
)

// M8's half, driven end to end over real HTTP through the real listener.
//
// The "browser" is an http.Client that follows redirects, substituted for the launcher. So a run goes:
// Runner builds the authorize URL → the fake browser fetches it → the fake instance 302s to the loopback
// URI → the client follows it into the listener this package actually bound → the Runner wakes up and
// redeems. Nothing about that path is mocked, which is the point: the thing M8 has to get right is a chain
// of four hops, and a mock would assert only what the test already believed.

// oauthFake is a fake instance that speaks the loopback flow.
type oauthFake struct {
	*fakeInstance

	// sawChallenge and sawRedirect are what /authorize received.
	sawChallenge string
	sawRedirect  string
	// sawVerifier is what /exchange received.
	sawVerifier string
	// issuedCode is what the callback handed to the listener.
	issuedCode string

	// failWith, when set, makes the instance redirect with ?error= instead of a code.
	failWith string
	// exchangeStatus overrides the exchange response.
	exchangeStatus int
	// exchanges counts redemptions, so a test can assert nothing reached the instance.
	exchanges int
}

func newOAuthFake(t *testing.T) *oauthFake {
	t.Helper()
	f := &oauthFake{fakeInstance: newFakeInstance(t)}

	// The two endpoints M8 adds, mounted on the same server the password path already uses.
	f.server.Config.Handler.(*http.ServeMux).HandleFunc("/api/v1/auth/oauth/google/authorize",
		func(w http.ResponseWriter, r *http.Request) {
			f.sawChallenge = r.URL.Query().Get("flow_challenge")
			f.sawRedirect = r.URL.Query().Get("client_redirect_uri")

			// Stands in for the whole provider round trip: the real instance would send the browser to
			// Google and be redirected back before reaching this point.
			back, err := url.Parse(f.sawRedirect)
			if err != nil {
				http.Error(w, "bad redirect", http.StatusBadRequest)
				return
			}
			if f.failWith != "" {
				back.RawQuery = url.Values{"error": {f.failWith}}.Encode()
			} else {
				f.issuedCode = "noc_" + strings.Repeat("a", 43)
				back.RawQuery = url.Values{"code": {f.issuedCode}}.Encode()
			}
			http.Redirect(w, r, back.String(), http.StatusFound)
		})

	f.server.Config.Handler.(*http.ServeMux).HandleFunc("/api/v1/auth/oauth/exchange",
		func(w http.ResponseWriter, r *http.Request) {
			f.exchanges++
			var req oauthExchangeRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.sawVerifier = req.FlowVerifier
			f.lastLogin.DeviceID = req.DeviceID
			f.lastLogin.DeviceName = req.DeviceName

			if f.exchangeStatus != 0 {
				w.WriteHeader(f.exchangeStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "eyJ.access.token", "refresh_token": "nrt_stored",
				"token_type": "Bearer", "expires_at": "2026-01-01T00:00:00Z",
			})
		})

	return f
}

// oauthRunner wires a Runner for the browser flow, with a fake browser and an ephemeral port.
//
// Port 0 rather than the shipped list: several packages' tests run at once on a CI machine, and contending
// for a fixed port would make this suite flaky for a reason that has nothing to do with what it tests. The
// list itself is exercised by the fallback tests below, which bind their own.
func oauthRunner(t *testing.T, f *oauthFake, opts Options) (*Runner, *credentials.Store, *bytes.Buffer) {
	t.Helper()
	if opts.Provider == "" {
		opts.Provider = "google"
	}
	runner, store, out := testRunner(t, f.fakeInstance, opts)
	runner.loopbackPorts = []int{0}
	runner.openBrowser = fakeBrowser(t)
	return runner, store, out
}

// fakeBrowser fetches the URL the way a browser would, following the instance's redirect into the
// listener this process bound.
func fakeBrowser(t *testing.T) func(context.Context, string) error {
	t.Helper()
	return func(ctx context.Context, target string) error {
		go func() {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			if err != nil {
				return
			}
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}
}

// ---------- the milestone's own criterion ----------

// M8's "done when", client half: a browser sign-in leaves something the daemon can use next launch.
func TestOAuthLoginStoresACredentialTheDaemonCanUse(t *testing.T) {
	f := newOAuthFake(t)
	runner, store, out := oauthRunner(t, f, Options{})

	require.NoError(t, runner.Run(t.Context()))

	record, refresh, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "nrt_stored", refresh, "the daemon needs the refresh token, and only that")
	assert.Equal(t, f.server.URL, record.InstanceURL)
	assert.Equal(t, "ada", record.Username, "the name comes from the instance, not from anything typed")
	assert.Equal(t, "ada-laptop", record.DeviceName)
	assert.NotEmpty(t, record.DeviceID)

	// The same device identity the password path uses, so a person switching methods does not acquire a
	// second session (ADR 0011).
	assert.Equal(t, record.DeviceID, f.lastLogin.DeviceID)
	assert.Contains(t, out.String(), "Signed in as ada")
}

// ---------- the binding this client must construct ----------

// The recipe in contracts/openapi.yaml, checked against what the instance actually received. The detail
// worth pinning is that the challenge hashes the whole prefixed string: get it wrong and everything works
// until the last request of the flow, which is the worst possible place to find out.
func TestTheCLISendsTheDocumentedFlowBinding(t *testing.T) {
	f := newOAuthFake(t)
	runner, _, _ := oauthRunner(t, f, Options{})
	require.NoError(t, runner.Run(t.Context()))

	require.NotEmpty(t, f.sawVerifier)
	assert.True(t, strings.HasPrefix(f.sawVerifier, "nof_"), "the verifier must carry its prefix")
	assert.Len(t, f.sawVerifier, 47, "the contract promises 47 characters")

	sum := sha256.Sum256([]byte(f.sawVerifier))
	assert.Equal(t, base64.RawURLEncoding.EncodeToString(sum[:]), f.sawChallenge,
		"the challenge must be the hash of the whole prefixed verifier")
	assert.Len(t, f.sawChallenge, 43, "the contract promises 43 characters")
}

// Rule 8. Neither half of the binding, nor the code, nor either token may reach the terminal — someone
// pasting a transcript into a bug report must not be pasting a credential.
func TestTheFlowVerifierAndCodeNeverReachTheTerminal(t *testing.T) {
	f := newOAuthFake(t)
	runner, _, out := oauthRunner(t, f, Options{})
	require.NoError(t, runner.Run(t.Context()))

	printed := out.String()
	require.NotEmpty(t, f.sawVerifier)
	require.NotEmpty(t, f.issuedCode)
	assert.NotContains(t, printed, f.sawVerifier, "the flow verifier is the secret half of the binding")
	assert.NotContains(t, printed, f.issuedCode, "the exchange code is a credential until it is spent")
	assert.NotContains(t, printed, "nrt_stored", "the refresh token")
	assert.NotContains(t, printed, "eyJ.access.token", "the access token")

	// The challenge, by contrast, is a hash and is meant to be visible — it is in the printed URL.
	assert.Contains(t, printed, f.sawChallenge)
}

// ---------- the listener ----------

// The one-line property that stops a sign-in listener appearing on the LAN. ":port" would bind 0.0.0.0,
// and the mistake is invisible on a developer's machine.
func TestTheListenerBindsLoopbackOnly(t *testing.T) {
	l, err := listenLoopback([]int{0})
	require.NoError(t, err)
	defer func() { _ = l.Close() }()

	addr, ok := l.listener.Addr().(*net.TCPAddr)
	require.True(t, ok)
	assert.True(t, addr.IP.IsLoopback(), "bound %s, which is reachable from the network", addr)
}

// The redirect sent to the instance names the port actually bound, not the one that was asked for. A bug
// here is invisible until a fallback happens, and then produces a sign-in that hangs forever.
func TestTheRedirectURISentIsTheOneBound(t *testing.T) {
	f := newOAuthFake(t)
	runner, _, _ := oauthRunner(t, f, Options{})
	require.NoError(t, runner.Run(t.Context()))

	require.NotEmpty(t, f.sawRedirect)
	sent, err := url.Parse(f.sawRedirect)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", sent.Hostname())
	assert.Equal(t, "/callback", sent.Path)
	assert.NotEqual(t, "0", sent.Port(), "the bound port must replace the placeholder")
}

// A busy port is walked past rather than diagnosed, and the flow completes on the next one.
func TestTheListenerFallsBackWhenThePrimaryPortIsTaken(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = blocker.Close() }()
	taken := blocker.Addr().(*net.TCPAddr).Port

	f := newOAuthFake(t)
	runner, _, _ := oauthRunner(t, f, Options{})
	runner.loopbackPorts = []int{taken, 0}

	require.NoError(t, runner.Run(t.Context()))

	sent, err := url.Parse(f.sawRedirect)
	require.NoError(t, err)
	assert.NotEqual(t, strconv.Itoa(taken), sent.Port(), "the occupied port must not have been used")
}

// The roadmap's explicit criterion: only once every registered port is exhausted does it fail, and the
// error says which were tried and what to do. Naming them matters — "no port was free" leaves somebody
// with nothing to look for.
func TestEveryPortTakenSaysWhichOnesAndWhatToDo(t *testing.T) {
	var ports []int
	for range 2 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer func() { _ = l.Close() }()
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}

	f := newOAuthFake(t)
	runner, _, _ := oauthRunner(t, f, Options{})
	runner.loopbackPorts = ports

	err := runner.Run(t.Context())
	require.Error(t, err)
	for _, port := range ports {
		assert.Contains(t, err.Error(), strconv.Itoa(port), "the error must name every port it tried")
	}
	assert.Contains(t, err.Error(), "password", "and offer the way out that does not need a port")
	assert.Zero(t, f.exchanges, "nothing may have been redeemed")
}

// Only the first callback counts. A refresh, a prefetch, or something local being curious must not replace
// a result already in hand.
func TestOnlyTheFirstCallbackCounts(t *testing.T) {
	l, err := listenLoopback([]int{0})
	require.NoError(t, err)
	defer func() { _ = l.Close() }()

	good := "noc_" + strings.Repeat("a", 43)
	for range 3 {
		resp, err := http.Get(l.redirectURI() + "?code=" + good)
		require.NoError(t, err)
		_ = resp.Body.Close()
	}

	res, err := l.wait(t.Context())
	require.NoError(t, err)
	assert.Equal(t, good, res.code)
	assert.Empty(t, l.result, "a second callback must not have queued another result")
}

// A code that is not shaped like one the instance issues is refused locally and never sent upstream. Cheap,
// and it means a local process cannot make this CLI relay arbitrary bytes in an authenticated request.
func TestAMalformedCodeAtTheListenerNeverReachesTheInstance(t *testing.T) {
	l, err := listenLoopback([]int{0})
	require.NoError(t, err)
	defer func() { _ = l.Close() }()

	resp, err := http.Get(l.redirectURI() + "?code=" + url.QueryEscape("../../etc/passwd"))
	require.NoError(t, err)
	_ = resp.Body.Close()

	res, err := l.wait(t.Context())
	require.NoError(t, err)
	assert.Empty(t, res.code)
	assert.Equal(t, "malformed_code", res.failure)
}

// Anything other than the callback path is a 404, so a stray local request cannot be taken for a result.
func TestTheListenerAnswersOnlyItsOwnPath(t *testing.T) {
	l, err := listenLoopback([]int{0})
	require.NoError(t, err)
	defer func() { _ = l.Close() }()

	resp, err := http.Get("http://" + l.listener.Addr().String() + "/")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Empty(t, l.result, "a 404 must not have produced a result")
}

// ---------- rule 19 at the new boundary ----------

// The listener's query is the one input in this program that arrives on a socket any local process can
// write to. An escape sequence in it must not reach the terminal.
func TestAnErrorFromTheLoopbackCallbackCannotActOnTheTerminal(t *testing.T) {
	l, err := listenLoopback([]int{0})
	require.NoError(t, err)
	defer func() { _ = l.Close() }()

	hostile := "\x1b[31mrate limited\x1b[2K\rSigned in as admin\x1b]0;pwned\a"
	resp, err := http.Get(l.redirectURI() + "?error=" + url.QueryEscape(hostile))
	require.NoError(t, err)
	_ = resp.Body.Close()

	res, err := l.wait(t.Context())
	require.NoError(t, err)
	require.Empty(t, res.code)

	// What survives is inert, and what is printed from it is inert too.
	assertInertOnATerminal(t, res.failure)
	assertInertOnATerminal(t, oauthFailure(res.failure).Error())
	assert.NotContains(t, res.failure, "Signed in", "no attacker-chosen words may survive")
}

// The vocabulary is closed: a known code keeps its identity, and everything else is reduced to something
// with a known shape rather than merely sanitized.
func TestTheFailureVocabularyIsClosed(t *testing.T) {
	assert.Equal(t, "access_denied", reduceFailure("access_denied"))
	assert.Equal(t, "email_unverified", reduceFailure("email_unverified"))
	assert.Equal(t, "no_result", reduceFailure(""))

	for _, hostile := range []string{
		"ACCESS DENIED; rm -rf /",
		"<script>alert(1)</script>",
		strings.Repeat("x", 500),
		"../../../etc/passwd",
	} {
		got := reduceFailure(hostile)
		assert.LessOrEqual(t, len(got), 64, "%q must be bounded", hostile)
		for _, r := range got {
			assert.True(t, r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_',
				"%q produced %q, which contains %q", hostile, got, r)
		}
	}
}

// ---------- what the instance says went wrong ----------

// A declined consent is reported as such, in this program's words rather than the instance's, and nothing
// is stored.
func TestADeclinedSignInIsReportedAndStoresNothing(t *testing.T) {
	f := newOAuthFake(t)
	f.failWith = "access_denied"
	runner, store, _ := oauthRunner(t, f, Options{})

	err := runner.Run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "canceled at the provider")
	assert.Zero(t, f.exchanges, "a failure must not be redeemed")

	_, err = store.LoadRecord()
	assert.ErrorIs(t, err, credentials.ErrNoCredential, "nothing may be stored on a failed sign-in")
}

// A refused code says what to do rather than repeating a status line.
func TestARefusedCodeSaysToTryAgain(t *testing.T) {
	f := newOAuthFake(t)
	f.exchangeStatus = http.StatusUnauthorized
	runner, _, _ := oauthRunner(t, f, Options{})

	err := runner.Run(t.Context())
	assert.ErrorIs(t, err, ErrOAuthCodeRefused)
}

// ---------- the browser ----------

// A browser that will not open is not a failure: the URL is printed and the flow still completes when the
// callback arrives. This is the SSH-with-a-forwarded-port case, and the wrong-default-browser case.
func TestABrowserThatWillNotOpenPrintsTheURLAndKeepsWaiting(t *testing.T) {
	f := newOAuthFake(t)
	runner, store, out := oauthRunner(t, f, Options{})

	browser := fakeBrowser(t)
	runner.openBrowser = func(ctx context.Context, target string) error {
		// Fails, and then something opens the link anyway — a person copying it out of the terminal.
		_ = browser(ctx, target)
		return assert.AnError
	}

	require.NoError(t, runner.Run(t.Context()))
	assert.Contains(t, out.String(), "Could not open a browser")
	assert.Contains(t, out.String(), "/api/v1/auth/oauth/google/authorize")

	_, _, err := store.Load()
	assert.NoError(t, err, "the sign-in must still have completed")
}

// --no-browser launches nothing at all and says where to go.
func TestNoBrowserPrintsTheURLAndDoesNotLaunch(t *testing.T) {
	f := newOAuthFake(t)
	runner, _, out := oauthRunner(t, f, Options{NoBrowser: true})

	launched := false
	browser := fakeBrowser(t)
	runner.openBrowser = func(ctx context.Context, target string) error {
		launched = true
		return browser(ctx, target)
	}

	// Nothing will open the link, so the wait has to be cut short rather than run its full length.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	err := runner.Run(ctx)

	require.Error(t, err, "nothing completed the flow, so it must not have succeeded")
	assert.False(t, launched, "--no-browser must not launch anything")
	assert.Contains(t, out.String(), "Open this in a browser")
}

// The wait ends promptly when its context does. Not about signals — cmd/app exits on those without
// unwinding — but about being callable from a test today and the TUI's login screen later.
func TestAnInterruptedWaitStopsPromptly(t *testing.T) {
	l, err := listenLoopback([]int{0})
	require.NoError(t, err)
	defer func() { _ = l.Close() }()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	start := time.Now()
	_, err = l.wait(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), time.Second, "the wait must not have run to its full timeout")
}

// ---------- what is said before anything happens ----------

// The plain-HTTP warning reaches the terminal before the browser opens, for the same reason it precedes a
// password prompt: the point of a warning is to let somebody stop.
func TestPlainHTTPIsWarnedAboutBeforeTheBrowserOpens(t *testing.T) {
	f := newOAuthFake(t)
	runner, _, out := oauthRunner(t, f, Options{})

	var warnedFirst bool
	browser := fakeBrowser(t)
	runner.openBrowser = func(ctx context.Context, target string) error {
		warnedFirst = strings.Contains(out.String(), "plain HTTP")
		return browser(ctx, target)
	}

	require.NoError(t, runner.Run(t.Context()))
	assert.True(t, warnedFirst, "the warning must already have been printed when the browser opened")
	assert.Contains(t, out.String(), "unencrypted")
}

// A misspelled provider fails at the terminal, before a port is bound or a request made — not as a 404
// page in a browser after somebody has already been sent there.
func TestAnUnknownProviderFailsBeforeAnythingIsBound(t *testing.T) {
	f := newOAuthFake(t)
	runner, _, _ := oauthRunner(t, f, Options{Provider: "gooogle"})

	launched := false
	runner.openBrowser = func(context.Context, string) error { launched = true; return nil }

	err := runner.Run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gooogle")
	assert.Contains(t, err.Error(), "google", "the error must name what this client does know")
	assert.False(t, launched, "nothing may have been opened")
	assert.Zero(t, f.exchanges)
}
