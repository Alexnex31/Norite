package login

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
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

// And it fails the same way on a machine with no browser, which is where it stopped doing so.
//
// The check used to live inside the OAuth path. That was good enough while there was one path; from M9 a
// headless machine takes the device-code path instead, which ignores --provider entirely — so
// `--provider gooogle` became a working sign-in that never mentioned the typo, on exactly the machines
// where somebody is least able to see what happened. Where a mistake is reported must not depend on what
// machine it was made on.
func TestAnUnknownProviderFailsWhereverItIsTyped(t *testing.T) {
	for _, reachable := range []bool{true, false} {
		f := newOAuthFake(t)
		runner, store, _ := oauthRunner(t, f, Options{Provider: "gooogle"})
		runner.browserReachable = func() bool { return reachable }

		err := runner.Run(t.Context())
		require.Error(t, err, "browser reachable: %v", reachable)
		assert.Contains(t, err.Error(), "gooogle", "browser reachable: %v", reachable)

		_, _, loadErr := store.Load()
		assert.Error(t, loadErr, "a typo must not sign anybody in: browser reachable %v", reachable)
	}
}

// ---------- findings from review ----------

// A scripted run on a machine where no browser can open must fail at once rather than binding a socket and
// waiting fifteen minutes for somebody who is not there. The ErrNoTerminal messages used to offer
// --provider as the way out of "you are not at a terminal", which it is not: it needs a browser, not a TTY.
func TestANonInteractiveRunWithNoBrowserFailsAtOnce(t *testing.T) {
	f := newOAuthFake(t)
	runner, _, _ := oauthRunner(t, f, Options{})
	runner.Interactive = false
	runner.openBrowser = func(context.Context, string) error { return assert.AnError }

	start := time.Now()
	err := runner.Run(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no terminal")
	assert.Less(t, time.Since(start), 5*time.Second, "it must not have waited out the listener timeout")
	assert.Zero(t, f.exchanges)
}

// The same run with a browser that *does* open is a real case — a script launched from a desktop session —
// and must still work. Only the combination is hopeless.
func TestANonInteractiveRunWithAWorkingBrowserStillSucceeds(t *testing.T) {
	f := newOAuthFake(t)
	runner, store, _ := oauthRunner(t, f, Options{})
	runner.Interactive = false

	require.NoError(t, runner.Run(t.Context()))
	_, _, err := store.Load()
	assert.NoError(t, err)
}

// --no-browser is a deliberate choice to read the link yourself, so the absence of a terminal must not
// turn it into the fail-fast case above: the output may well be going somewhere a person is watching.
//
// Asserted by canceling rather than by completing the flow — what matters is that it *waits*, and the
// difference between "waited and was interrupted" and "refused immediately" is the whole property.
func TestNoBrowserIsNotOverriddenByANonInteractiveSession(t *testing.T) {
	f := newOAuthFake(t)
	runner, _, out := oauthRunner(t, f, Options{NoBrowser: true})
	runner.Interactive = false
	runner.openBrowser = func(context.Context, string) error {
		t.Error("--no-browser must not launch anything")
		return nil
	}

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	err := runner.Run(ctx)

	require.ErrorIs(t, err, context.DeadlineExceeded, "it must have been waiting, not refusing")
	assert.Contains(t, out.String(), "Open this in a browser")
}

// An address claimed between the callback and the sign-up form ends the flow in the browser, so a waiting
// client is told rather than left to time out — and told something true, not "server_error".
func TestASignupThatCannotCompleteReachesTheListener(t *testing.T) {
	f := newOAuthFake(t)
	f.failWith = "email_taken"
	runner, _, _ := oauthRunner(t, f, Options{})

	err := runner.Run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already registered")
	assert.NotContains(t, err.Error(), "server_error")
}

// ---------- the two vocabularies cannot drift apart ----------

// contractPath is contracts/openapi.yaml, which is where the failure codes are actually specified.
//
// The same arrangement instanceinit uses for contracts/instance-config.toml: the backend proves it emits
// these, this side proves the CLI has something to say about each. The two live in separate Go modules and
// share no types, so a hand-maintained list on each side is exactly the shape that drifts — and the drift
// is silent, because an unrecognized code still reduces cleanly and just prints as itself.
const contractPath = "../../../contracts/openapi.yaml"

// documentedFailureCodes pulls the enumerated codes out of the callback's 302 description.
func documentedFailureCodes(t *testing.T) []string {
	t.Helper()

	body, err := os.ReadFile(contractPath)
	require.NoError(t, err)

	// The sentence that enumerates them, anchored on both ends so nothing else in the document is picked
	// up. The opening anchor is the end of the paragraph *above* the list rather than its start: that
	// paragraph says there is deliberately no `error_description`, and a looser anchor swept that word up
	// as a code — which this test then correctly reported as missing wording.
	_, after, found := strings.Cut(string(body), "Write your own wording from these.")
	require.True(t, found, "the contract no longer introduces the vocabulary where this test expects")
	block, _, found := strings.Cut(after, "Treat an")
	require.True(t, found, "the vocabulary sentence no longer ends where this test expects")

	var codes []string
	for _, m := range regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(block, -1) {
		codes = append(codes, m[1])
	}
	require.NotEmpty(t, codes, "no codes found in:\n%s", block)
	return codes
}

// Every code the instance can send has wording of this client's own. Without this, a code added on the
// backend arrives here, falls through to the generic branch, and is shown to a person as the raw
// identifier — which is precisely what sending codes instead of sentences was supposed to avoid.
func TestEveryDocumentedFailureCodeHasWording(t *testing.T) {
	for _, code := range documentedFailureCodes(t) {
		if code == "server_error" {
			// The one deliberate exception, asserted rather than skipped silently: it means "something
			// went wrong that the vocabulary cannot describe", so the generic branch *is* the right
			// answer and the instance's own word for it is worth showing.
			assert.Contains(t, oauthFailure(code).Error(), code)
			continue
		}

		msg := oauthFailure(code).Error()
		assert.NotContains(t, msg, "could not complete the sign-in (",
			"%q reached the generic branch: the contract documents it and this client has no wording", code)
		assert.NotContains(t, msg, code,
			"%q is shown to a person verbatim rather than explained", code)
	}
}

// And the reverse: a code this client explains must be one the instance can actually send, or a local one
// the listener itself produces. A branch for something nobody emits is dead code that reads as coverage.
func TestTheCLIExplainsNoCodeNobodySends(t *testing.T) {
	documented := documentedFailureCodes(t)

	// Produced by the listener rather than by the instance — see loopback.go's read and reduceFailure.
	local := []string{"malformed_code", "no_result"}

	source, err := os.ReadFile("oauth.go")
	require.NoError(t, err)
	handled := regexp.MustCompile(`case "([a-z_]+)":`).FindAllStringSubmatch(string(source), -1)
	require.NotEmpty(t, handled)

	for _, m := range handled {
		code := m[1]
		assert.True(t, slices.Contains(documented, code) || slices.Contains(local, code),
			"%q is explained here but the contract does not list it and the listener does not produce it",
			code)
	}
}

// The listener's own reduction keeps every documented code intact. A code that survived the wire only to
// be stripped here would take the same generic branch as an attack payload.
func TestReduceFailureKeepsEveryDocumentedCode(t *testing.T) {
	for _, code := range documentedFailureCodes(t) {
		assert.Equal(t, code, reduceFailure(code), "%q must survive reduction unchanged", code)
	}
}

// The browser is where the person is looking, so the page it lands on has to agree with what happened.
// Serving "Signed in" after a canceled sign-in is a lie the terminal then contradicts — and closing the
// tab believing it worked is the obvious next move. Found by running the flow, not by reading it.
func TestTheCallbackPageSaysWhatActuallyHappened(t *testing.T) {
	for _, tc := range []struct {
		why, query, wants, forbids string
	}{
		{"a delivered code", "?code=noc_" + strings.Repeat("a", 43), "Signed in", "not completed"},
		{"a declined sign-in", "?error=access_denied", "not completed", "Signed in"},
		{"a code this instance did not issue", "?code=garbage", "not completed", "Signed in"},
		{"nothing at all", "", "not completed", "Signed in"},
	} {
		l, err := listenLoopback([]int{0})
		require.NoError(t, err)

		resp, err := http.Get(l.redirectURI() + tc.query)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, err)

		assert.Contains(t, string(body), tc.wants, "%s: the page must say so", tc.why)
		assert.NotContains(t, string(body), tc.forbids, "%s: the page must not claim otherwise", tc.why)
		_ = l.Close()
	}
}

// The failure page carries no part of the query that produced it. The error code is the one value on this
// path a hostile local process can influence, and the terminal already reports it in better words.
func TestTheFailurePageInterpolatesNothing(t *testing.T) {
	l, err := listenLoopback([]int{0})
	require.NoError(t, err)
	defer func() { _ = l.Close() }()

	resp, err := http.Get(l.redirectURI() + "?error=" + url.QueryEscape("<script>alert(1)</script>"))
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, err)

	assert.NotContains(t, string(body), "script>alert")
	assert.Equal(t, loopbackFailedPage, string(body), "the page is a constant and must stay one")
}
