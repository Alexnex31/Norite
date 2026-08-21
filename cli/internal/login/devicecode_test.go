package login

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/credentials"
)

// M9's flow from this side: no listener, no port, no browser. The whole client is a pair of POSTs and a
// loop, so what these cover is what it prints, what it refuses to print, and how it waits.

// fakeDeviceInstance answers the two device endpoints. Real HTTP, like the rest of this package's fakes:
// the poll's answers are read out of an error envelope by code, and a mock would assert only that this
// test agrees with itself.
type fakeDeviceInstance struct {
	*fakeInstance

	mu sync.Mutex
	// pending is how many polls answer authorization_pending before one succeeds.
	pending int
	// slowDowns is how many answer slow_down first, which the client is supposed to widen its interval on.
	slowDowns int
	// finalError, when set, is the code every poll answers with instead of ever succeeding.
	finalError string
	// throttleFirst is how many polls answer 429 before the flow is allowed to proceed.
	throttleFirst int

	// userCode, verificationURI and interval override what the issuing endpoint returns.
	userCode        string
	verificationURI string
	interval        int
	expiresIn       int

	// polls counts every poll, and lastDeviceCode is what the last one presented.
	polls          int
	lastDeviceCode string
	// issued is what the issuing endpoint received.
	issued deviceCodeRequest
}

func newFakeDeviceInstance(t *testing.T) *fakeDeviceInstance {
	t.Helper()
	f := &fakeDeviceInstance{fakeInstance: newFakeInstance(t)}

	f.server.Config.Handler.(*http.ServeMux).HandleFunc("/api/v1/auth/device/code",
		func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			_ = json.NewDecoder(r.Body).Decode(&f.issued)

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "nod_" + strings.Repeat("a", 43),
				"user_code":        orDefault(f.userCode, "BCDF-GHJK"),
				"verification_uri": orDefault(f.verificationURI, f.server.URL+"/device"),
				"expires_in":       orZero(f.expiresIn, 1200),
				"interval":         orZero(f.interval, 5),
			})
		})

	f.server.Config.Handler.(*http.ServeMux).HandleFunc("/api/v1/auth/device/token",
		func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.polls++

			var body struct {
				DeviceCode string `json:"device_code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.lastDeviceCode = body.DeviceCode

			switch {
			case f.throttleFirst > 0:
				f.throttleFirst--
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"slow down"}}`))
			case f.finalError != "":
				writeDeviceError(w, f.finalError)
			case f.slowDowns > 0:
				f.slowDowns--
				writeDeviceError(w, "slow_down")
			case f.pending > 0:
				f.pending--
				writeDeviceError(w, "authorization_pending")
			default:
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token": "eyJ.access.token", "refresh_token": "nrt_stored",
					"token_type": "Bearer", "expires_at": "2026-01-01T00:00:00Z",
				})
			}
		})

	return f
}

func writeDeviceError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": "…", "request_id": "req-1"},
	})
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func orZero(v, fallback int) int {
	if v == 0 {
		return fallback
	}
	return v
}

// deviceRunner is testRunner with the poll loop's clock replaced, so a twenty-minute flow runs instantly.
func deviceRunner(t *testing.T, f *fakeDeviceInstance, opts Options) (*Runner, *credentials.Store, *bytes.Buffer) {
	t.Helper()
	runner, store, out := testRunner(t, f.fakeInstance, opts)

	var elapsed time.Duration
	var mu sync.Mutex
	runner.after = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		elapsed += d
		mu.Unlock()
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	start := time.Now()
	runner.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return start.Add(elapsed)
	}
	return runner, store, out
}

// ---------- the milestone's own criterion ----------

// M9's "done when", from the client's side: a login on a host with no browser displays a code instead of
// binding a listener, and finishes once somebody approves it elsewhere.
func TestDeviceCodeLoginStoresACredentialTheDaemonCanUse(t *testing.T) {
	f := newFakeDeviceInstance(t)
	f.pending = 2
	runner, store, out := deviceRunner(t, f, Options{DeviceCode: true})

	require.NoError(t, runner.Run(t.Context()))

	// What the instance was told, and what it was not: no email, no password, nothing typed here at all.
	assert.NotEmpty(t, f.issued.DeviceID, "the session is scoped to this installation")
	assert.Equal(t, "ada-laptop", f.issued.DeviceName)
	assert.Zero(t, f.logins, "a device sign-in must not touch the password endpoint")

	// What a person saw.
	assert.Contains(t, out.String(), "BCDF-GHJK")
	assert.Contains(t, out.String(), f.server.URL+"/device")

	// What a later daemon start will find.
	record, token, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "nrt_stored", token)
	assert.Equal(t, "ada", record.Username)
	assert.Equal(t, f.issued.DeviceID, record.DeviceID)
}

// ---------- what reaches the terminal ----------

// Rule 8. The device code is what redeems for a token pair, so it is a credential and belongs nowhere a
// person or a log can see it — unlike the user code beside it, which exists to be read out loud.
func TestTheDeviceCodeNeverReachesTheTerminal(t *testing.T) {
	f := newFakeDeviceInstance(t)
	runner, _, out := deviceRunner(t, f, Options{DeviceCode: true})

	require.NoError(t, runner.Run(t.Context()))

	assert.NotContains(t, out.String(), f.lastDeviceCode)
	assert.NotContains(t, out.String(), "nod_")
	assert.Contains(t, out.String(), "BCDF-GHJK", "and the half that is meant to be read is shown")
}

// The strictest check in the flow, because this is a URL somebody is about to open and type a password
// into. An instance that answers with somewhere else is misconfigured or hostile, and printing it either
// way would make this command the delivery mechanism.
//
// A unit table rather than a run through the fake: the fake is reachable only over plaintext loopback, so
// the two cases that matter most — a downgrade from https, and a lookalike host — cannot be expressed
// against it at all.
func TestOnlyTheInstanceItselfIsAcceptedAsAVerificationURI(t *testing.T) {
	const instance = "https://chat.example.com"

	for _, tc := range []struct{ why, uri string }{
		{"another host entirely", "https://evil.example/device"},
		{"a host that merely starts the same way", "https://chat.example.com.evil.example/device"},
		{"a host the instance is a suffix of", "https://evilchat.example.com/device"},
		{"the same host downgraded to plaintext", "http://chat.example.com/device"},
		{"a different port on the same host", "https://chat.example.com:8443/device"},
		{"userinfo hiding the real host", "https://chat.example.com@evil.example/device"},
		{"a scheme that is not a browser navigation", "javascript:alert(1)"},
		{"a bare path with no origin of its own", "/device"},
		{"nothing at all", ""},

		// The right host with a query built to read as another one. url.Parse rejects ASCII controls and
		// stops there — the bidi overrides are multi-byte — and String() emits RawQuery verbatim, so
		// nothing before the sanitizer check would have noticed (rule 19).
		{"a bidi override in the query", "https://chat.example.com/device?x=\u202emoc.live"},
	} {
		got, err := checkVerificationURI(instance, tc.uri)
		assert.Error(t, err, "%s: %q must be refused", tc.why, tc.uri)
		assert.Empty(t, got, "%s: a refusal must return nothing printable", tc.why)
	}

	for _, uri := range []string{
		"https://chat.example.com/device",
		"https://chat.example.com/device?code=BCDFGHJK",
	} {
		got, err := checkVerificationURI(instance, uri)
		require.NoError(t, err, "%q is this instance and must be accepted", uri)
		assert.Equal(t, uri, got)
	}

	// The same override in the *path* is accepted, and that is not an oversight in the check above.
	// url.URL.String() percent-encodes a path, so what reaches the terminal is literal ASCII that reorders
	// nothing — the danger is the raw bytes, and by then there are none. Asserted rather than assumed,
	// because the difference between the path and the query here is one line of the standard library's
	// behavior and nothing in this file would otherwise record that it was checked.
	escaped, err := checkVerificationURI(instance, "https://chat.example.com/\u202emoc.live")
	require.NoError(t, err)
	assert.NotContains(t, escaped, "\u202e")
	assert.Contains(t, escaped, "%E2%80%AE")
}

// And end to end, so the refusal is one the flow actually reaches rather than one only a unit test sees.
func TestAVerificationURIElsewhereIsNeverPrinted(t *testing.T) {
	f := newFakeDeviceInstance(t)
	f.verificationURI = "http://evil.example/device"
	runner, _, out := deviceRunner(t, f, Options{DeviceCode: true})

	err := runner.Run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "somewhere other than itself")
	assert.NotContains(t, out.String(), "evil.example")
}

// Refused rather than sanitized, and the difference matters here: this value is printed as an instruction.
// A cleaned-up version of it is worse than none, because somebody would type what they were shown and be
// told it is wrong, with nothing on screen explaining why.
func TestAUserCodeThisClientCannotDisplayIsRefused(t *testing.T) {
	for _, tc := range []struct{ why, code string }{
		{"an escape sequence", "BCDF\x1b[2KGHJK"},
		{"a newline that could forge a line of output", "BCDF-GHJK\nApproved!"},
		{"lowercase, which this instance's contract does not issue", "bcdf-ghjk"},
		{"a value long enough to fill a screen", strings.Repeat("A", 200)},
	} {
		f := newFakeDeviceInstance(t)
		f.userCode = tc.code
		runner, _, out := deviceRunner(t, f, Options{DeviceCode: true})

		err := runner.Run(t.Context())
		require.Error(t, err, "%s: %q must be refused", tc.why, tc.code)
		assert.Contains(t, err.Error(), "cannot display", tc.why)
		assert.NotContains(t, out.String(), "\x1b", "%s: nothing that acts on a terminal is printed", tc.why)
	}
}

// A response missing half of what it promised is not this API — a captive portal or a proxy, most likely.
// Said here rather than as a confusing failure two steps later, which is the same guard the login and
// exchange calls carry.
func TestAnIncompleteIssuingResponseIsRefused(t *testing.T) {
	f := newFakeInstance(t)
	f.server.Config.Handler.(*http.ServeMux).HandleFunc("/api/v1/auth/device/code",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_code":"BCDF-GHJK","verification_uri":"x","expires_in":1,"interval":1}`))
		})

	runner, _, _ := testRunner(t, f, Options{DeviceCode: true})
	err := runner.Run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "incomplete sign-in code")
}

// ---------- the loop ----------

// The instance asks for room; the client gives it, additively, and keeps going. A client that ignored
// slow_down would be throttled off the instance for the rest of the flow.
func TestSlowDownWidensTheIntervalAndTheFlowStillCompletes(t *testing.T) {
	f := newFakeDeviceInstance(t)
	f.slowDowns = 3
	f.pending = 1
	runner, store, _ := deviceRunner(t, f, Options{DeviceCode: true})

	require.NoError(t, runner.Run(t.Context()))
	assert.Equal(t, 5, f.polls, "three slow_downs, one pending, one success")

	_, token, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "nrt_stored", token)
}

// Waiting happens before the first ask as well as between them. Nobody can have approved a code issued a
// millisecond ago, so an immediate poll spends a request on a certain "pending" — and sets the instance's
// idea of what counts as too fast.
func TestTheFirstPollWaitsForTheInterval(t *testing.T) {
	f := newFakeDeviceInstance(t)
	f.interval = 7
	runner, _, _ := deviceRunner(t, f, Options{DeviceCode: true})

	var waits []time.Duration
	runner.after = func(d time.Duration) <-chan time.Time {
		waits = append(waits, d)
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}

	require.NoError(t, runner.Run(t.Context()))
	require.NotEmpty(t, waits, "the client must not poll the instant it has a code")
	assert.Equal(t, 7*time.Second, waits[0], "and it must use the interval the instance published")
}

// A 429 is an instruction to wait, not a reason to throw away a sign-in. By the time a poll is throttled
// the person may already have approved it on their phone, and giving up there costs them a fresh code and
// a second trip to the other device.
func TestBeingRateLimitedBacksOffRatherThanGivingUp(t *testing.T) {
	f := newFakeDeviceInstance(t)
	f.throttleFirst = 2
	runner, store, _ := deviceRunner(t, f, Options{DeviceCode: true})

	require.NoError(t, runner.Run(t.Context()))

	_, token, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "nrt_stored", token, "the approved code must still be collected")
}

// A denial is not an expiry, and saying so is the point: somebody pressed Deny, and "try again" is the
// wrong advice for that.
func TestADeniedAuthorizationSaysItWasDenied(t *testing.T) {
	f := newFakeDeviceInstance(t)
	f.finalError = "access_denied"
	runner, store, _ := deviceRunner(t, f, Options{DeviceCode: true})

	err := runner.Run(t.Context())
	require.ErrorIs(t, err, ErrDeviceAccessDenied)

	_, _, loadErr := store.Load()
	assert.Error(t, loadErr, "a denied sign-in must leave nothing behind")
}

func TestAnExpiredCodeSaysToStartAgain(t *testing.T) {
	f := newFakeDeviceInstance(t)
	f.finalError = "expired_token"
	runner, _, _ := deviceRunner(t, f, Options{DeviceCode: true})

	err := runner.Run(t.Context())
	require.ErrorIs(t, err, ErrDeviceCodeExpired)
	assert.Contains(t, err.Error(), "norite login")
}

// The client gives up on its own rather than polling an instance that has gone away forever.
func TestTheClientStopsWhenTheCodeWouldHaveExpired(t *testing.T) {
	f := newFakeDeviceInstance(t)
	f.pending = 1_000_000
	f.expiresIn = 120
	f.interval = 5
	runner, _, _ := deviceRunner(t, f, Options{DeviceCode: true})

	err := runner.Run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expired before it was approved")
	assert.Less(t, f.polls, 40, "it must stop rather than poll the instance forever")
}

// Cancellable, which is what makes it usable from a terminal and testable without waiting.
func TestAnInterruptedDevicePollStopsPromptly(t *testing.T) {
	f := newFakeDeviceInstance(t)
	f.pending = 1_000_000
	runner, _, _ := deviceRunner(t, f, Options{DeviceCode: true})

	ctx, cancel := context.WithCancel(t.Context())
	runner.after = func(time.Duration) <-chan time.Time {
		cancel()
		return make(chan time.Time) // never fires; only the cancellation can end this
	}

	err := runner.Run(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// An instance that has not been told its own address says so, and the message names what still works —
// this is an operator's configuration problem and not the fault of whoever ran the command.
func TestAnInstanceWithoutTheFlowConfiguredSaysWhatElseWorks(t *testing.T) {
	f := newFakeInstance(t)
	f.server.Config.Handler.(*http.ServeMux).HandleFunc("/api/v1/auth/device/code",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"device_flow_unavailable","message":"…"}}`))
		})

	runner, _, _ := testRunner(t, f, Options{DeviceCode: true})
	err := runner.Run(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not set up for code sign-in")
	assert.Contains(t, err.Error(), "password")
}

// ---------- choosing the flow ----------

// The detection only ever redirects a sign-in that was already going to need a browser. A password login
// over SSH is what somebody typed, and stays what they typed.
func TestTheFallbackOnlyAppliesToProviderSignIns(t *testing.T) {
	f := newFakeDeviceInstance(t)
	runner, _, out := deviceRunner(t, f, Options{})
	runner.browserReachable = func() bool { return false }

	require.NoError(t, runner.Run(t.Context()))
	assert.Equal(t, 1, f.logins, "a bare login must still be a password login")
	assert.Zero(t, f.polls)
	assert.NotContains(t, out.String(), "No browser is reachable")
}

// And when it does apply, it says so. A degradation nobody is told about is one discovered later, at a
// worse time — ADR 0025's rule for the keyring, applied to this.
func TestFallingBackToTheDeviceCodeIsNeverSilent(t *testing.T) {
	f := newFakeDeviceInstance(t)
	runner, store, out := deviceRunner(t, f, Options{Provider: "github"})
	runner.browserReachable = func() bool { return false }

	require.NoError(t, runner.Run(t.Context()))

	assert.Contains(t, out.String(), "No browser is reachable")
	assert.Contains(t, out.String(), "GitHub", "and it names what it was asked to open")
	assert.Contains(t, out.String(), "BCDF-GHJK")

	_, token, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "nrt_stored", token)
}

// --no-browser is a deliberate "print the link, I will open it myself", which works over SSH with a
// forwarded port. Overriding it would take away the one flow it exists to provide.
func TestNoBrowserIsNotOverriddenByTheDetection(t *testing.T) {
	f := newFakeDeviceInstance(t)
	runner, _, _ := deviceRunner(t, f, Options{Provider: "github", NoBrowser: true, Instance: f.server.URL})
	runner.browserReachable = func() bool { return false }
	runner.loopbackPorts = []int{0}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := runner.Run(ctx)

	require.Error(t, err)
	assert.Zero(t, f.polls, "--no-browser must not become a device-code sign-in")
}
