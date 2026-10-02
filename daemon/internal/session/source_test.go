// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
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

// M7's criterion was that the daemon uses the stored token on its next launch without re-prompting; M19's
// is that it stays signed in while it runs, on a clock that may be hours wrong. These drive both: a
// credential on disk, a stand-in instance whose clock is the truth, and a daemon whose clock is not.

// ---------- a clock the tests own ----------

// fakeClock is the true time. The stand-in instance reads it directly; the daemon reads it through
// skewedClock, which is how a machine whose clock is wrong sees it.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, fakeWaiter{at: c.now.Add(d), ch: ch})
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at.After(c.now) {
			kept = append(kept, w)
			continue
		}
		w.ch <- c.now
	}
	c.waiters = kept
}

// waitingWithin reports whether something is waiting on a deadline no further than d ahead.
func (c *fakeClock) waitingWithin(d time.Duration) func() bool {
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, w := range c.waiters {
			if !w.at.After(c.now.Add(d)) {
				return true
			}
		}
		return false
	}
}

// skewedClock is the daemon's view of fakeClock on a machine whose clock is off by skew.
type skewedClock struct {
	*fakeClock
	skew time.Duration
}

func (c skewedClock) Now() time.Time { return c.fakeClock.Now().Add(c.skew) }

// ---------- a stand-in instance ----------

// fakeInstance stands in for the backend's refresh and logout endpoints.
//
// Every refresh issues a new pair, numbered, so a test can tell which renewal a token came from: refresh
// tokens nrt_rotated_N, access tokens eyJ.access.N. Its clock is the fake one, which is the truth — the
// expiry it issues and the Date header it sends are both read from it, as the real instance's are.
type fakeInstance struct {
	mu     sync.Mutex
	server *httptest.Server
	clock  *fakeClock

	// presented is every refresh token the daemon presented, in order.
	presented []string
	// status and body override the response for the failure paths.
	status int
	body   string
	// ttl is the access token's life. Zero means fifteen minutes, the real one.
	ttl time.Duration
	// handedBack is the token presented to /auth/logout. Empty means none was.
	handedBack string
	// logoutStatus overrides the revoke response; logoutBroken drops the connection instead.
	logoutStatus int
	logoutBroken bool
	// beforeRefresh runs inside the handler, the one place a test can act while the daemon is between
	// presenting a token and writing back its successor. Runs once and is then cleared.
	beforeRefresh func()
}

func newFakeInstance(t *testing.T, clock *fakeClock) *fakeInstance {
	t.Helper()
	f := &fakeInstance{clock: clock}

	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RefreshToken string `json:"refresh_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		if r.URL.Path == "/api/v1/auth/logout" {
			f.mu.Lock()
			f.handedBack = body.RefreshToken
			broken, status := f.logoutBroken, f.logoutStatus
			f.mu.Unlock()
			if broken {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
				return
			}
			w.WriteHeader(cmp.Or(status, http.StatusNoContent))
			return
		}
		if r.URL.Path != "/api/v1/auth/refresh" {
			http.NotFound(w, r)
			return
		}

		f.mu.Lock()
		f.presented = append(f.presented, body.RefreshToken)
		n := len(f.presented)
		before := f.beforeRefresh
		f.beforeRefresh = nil
		status, respBody, ttl := f.status, f.body, cmp.Or(f.ttl, 15*time.Minute)
		f.mu.Unlock()

		if before != nil {
			before()
		}

		now := f.clock.Now()
		w.Header().Set("Date", now.Format(http.TimeFormat))
		if status != 0 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, respBody)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fmt.Sprintf("eyJ.access.%d", n), "refresh_token": fmt.Sprintf("nrt_rotated_%d", n),
			"token_type": "Bearer", "expires_at": now.Add(ttl).Format(time.RFC3339Nano),
		})
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeInstance) set(fn func(f *fakeInstance)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeInstance) handedBackToken() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.handedBack
}

// refreshes reports every refresh token presented, in order.
func (f *fakeInstance) refreshes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.presented...)
}

// ---------- harness ----------

// storedSessionIn writes a credential the way `norite login` would.
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

// loginTo is what `norite login` to another account leaves in the store.
func loginTo(t *testing.T, store *credentials.Store, instanceURL, refreshToken string) {
	t.Helper()
	require.NoError(t, store.Save(credentials.Record{
		InstanceURL: instanceURL,
		UserID:      "987654321",
		Username:    "grace",
		DeviceID:    "dev_test",
		DeviceName:  "laptop",
	}, refreshToken))
}

// syncBuffer is a log sink the daemon's goroutine writes while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type harness struct {
	t      *testing.T
	clock  *fakeClock
	f      *fakeInstance
	store  *credentials.Store
	dir    string
	logs   *syncBuffer
	src    *Source
	cancel context.CancelFunc
	done   chan struct{}
}

type harnessOpt func(*harness, *Options)

// withSkew puts the daemon on a machine whose clock is off by skew.
func withSkew(skew time.Duration) harnessOpt {
	return func(h *harness, o *Options) { o.Clock = skewedClock{fakeClock: h.clock, skew: skew} }
}

// newHarness starts a Source against a stand-in instance and a stored credential. Call start to run it,
// after arranging anything the first refresh must see.
func newHarness(t *testing.T, opts ...harnessOpt) *harness {
	t.Helper()
	h := &harness{t: t, clock: newFakeClock(), dir: t.TempDir(), logs: &syncBuffer{}}
	h.f = newFakeInstance(t, h.clock)
	h.store = storedSessionIn(t, h.dir, h.f.server.URL, "nrt_from_login")

	o := Options{
		Store: h.store, HTTP: h.f.server.Client(), Clock: h.clock,
		Log:      zerolog.New(h.logs).Level(zerolog.DebugLevel),
		RetryMin: time.Second, RetryMax: 4 * time.Second,
	}
	for _, opt := range opts {
		opt(h, &o)
	}
	h.src = New(o)
	return h
}

func (h *harness) start() {
	ctx, cancel := context.WithCancel(h.t.Context())
	h.cancel, h.done = cancel, make(chan struct{})
	go func() { defer close(h.done); h.src.Run(ctx) }()
	h.t.Cleanup(h.stop)
}

func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		h.t.Error("Run did not return after its context was canceled")
	}
	h.cancel = nil
}

// current waits briefly, in real time, for a usable credential.
func (h *harness) current() (Credential, error) {
	ctx, cancel := context.WithTimeout(h.t.Context(), 2*time.Second)
	defer cancel()
	return h.src.Current(ctx)
}

// tryCurrent asks for a credential without waiting for one.
func (h *harness) tryCurrent() (Credential, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	c, err := h.src.Current(ctx)
	return c, err == nil
}

// hasToken reports whether Current is handing out accessToken.
func (h *harness) hasToken(accessToken string) func() bool {
	return func() bool { c, ok := h.tryCurrent(); return ok && c.AccessToken == accessToken }
}

// settle waits until Run has nothing due: it is live and its next refresh lies ahead on its own estimate
// of the instance's clock. A test that advances the clock while a refresh is in flight dates the instance's
// answer a step after the daemon sent the request, which measures the test's timing rather than the
// daemon's — so a test counting refreshes settles between steps.
//
// It asks for a token while it waits, as a gateway reconnecting would. Run arms its timer after computing
// the delay, and a step taken in between arms it a step late — nanoseconds in real time, a minute here — so
// without the asking a due refresh could wait for a step that is not coming. Asking is the production path
// for exactly that: Current nudges a refresh that is due.
func (h *harness) settle() {
	h.t.Helper()
	require.Eventually(h.t, func() bool {
		h.tryCurrent()
		h.src.mu.Lock()
		defer h.src.mu.Unlock()
		return h.src.phase == phaseLive && h.src.refreshAt.After(h.src.server.now())
	}, 2*time.Second, time.Millisecond, "the session did not settle")
}

// noCredential asserts Current has nothing to give.
func (h *harness) noCredential() {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.t.Context(), 150*time.Millisecond)
	defer cancel()
	_, err := h.src.Current(ctx)
	require.ErrorIs(h.t, err, context.DeadlineExceeded, "there must be no usable credential")
}

// advanceUntil moves the fake clock forward a step at a time until cond holds. Steps rather than one jump,
// because Run arms its timers between steps — a jump past several deadlines at once fires only the timers
// that already existed.
func (h *harness) advanceUntil(step time.Duration, limit time.Duration, cond func() bool) {
	h.t.Helper()
	for moved := time.Duration(0); ; moved += step {
		if waitBriefly(cond) {
			return
		}
		if moved >= limit {
			h.t.Fatalf("condition not met after advancing the clock %v", limit)
		}
		h.clock.Advance(step)
	}
}

// waitBriefly polls cond in real time, for long enough that Run has acted on whatever just happened.
func waitBriefly(cond func() bool) bool {
	deadline := time.Now().Add(25 * time.Millisecond)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

func (h *harness) stored() string {
	h.t.Helper()
	_, token, err := h.store.Load()
	require.NoError(h.t, err)
	return token
}

// ---------- signing in: M7's criterion ----------

func TestTheDaemonSignsInWithTheStoredCredential(t *testing.T) {
	h := newHarness(t)
	h.start()

	c, err := h.current()
	require.NoError(t, err)
	assert.Equal(t, []string{"nrt_from_login"}, h.f.refreshes(), "the daemon presents what the login stored")
	assert.Equal(t, "eyJ.access.1", c.AccessToken)
	assert.Equal(t, "ada", c.Username)
	assert.Equal(t, h.f.server.URL, c.InstanceURL)
	assert.Equal(t, uint64(1), c.Generation)
	assert.Contains(t, h.logs.String(), "signed in with the stored credential")
}

// Refresh tokens rotate, and the instance detects reuse of a spent one (M4). So the new token has to
// replace the old one at the moment it is issued, or the next start presents something already retired.
func TestTheRotatedTokenReplacesTheStoredOne(t *testing.T) {
	h := newHarness(t)
	h.start()
	_, err := h.current()
	require.NoError(t, err)
	assert.Equal(t, "nrt_rotated_1", h.stored())

	// ...and a second start presents the rotated one rather than the original.
	h.stop()
	again := New(Options{Store: h.store, HTTP: h.f.server.Client(), Clock: h.clock, Log: zerolog.Nop()})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go again.Run(ctx)
	_, err = again.Current(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"nrt_from_login", "nrt_rotated_1"}, h.f.refreshes())
}

// The access token stays in memory. Fifteen minutes is shorter than the interval between the restarts it
// would be persisted to survive, so writing it down adds a credential at rest and buys nothing.
func TestTheAccessTokenIsNeverWrittenDown(t *testing.T) {
	h := newHarness(t)
	h.start()
	_, err := h.current()
	require.NoError(t, err)

	entries, err := os.ReadDir(h.dir)
	require.NoError(t, err)
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(h.dir, e.Name()))
		require.NoError(t, err)
		assert.NotContains(t, string(data), "eyJ.access", "%s holds an access token", e.Name())
	}
}

// ---------- staying signed in: M19 ----------

// The milestone's reason for existing on this side: a connection that outlives the access token needs a
// fresh one for its next reconnect, and a refresh token unused for thirty days expires.
func TestTheSessionIsRenewedBeforeItExpires(t *testing.T) {
	h := newHarness(t)
	h.start()
	first, err := h.current()
	require.NoError(t, err)

	// A quarter of a fifteen-minute life before expiry: not at ten minutes, by twelve.
	h.clock.Advance(10 * time.Minute)
	assert.False(t, waitBriefly(func() bool { return len(h.f.refreshes()) > 1 }), "renewed too early")
	h.advanceUntil(30*time.Second, 2*time.Minute, h.hasToken("eyJ.access.2"))

	second, err := h.current()
	require.NoError(t, err)
	assert.NotEqual(t, first.AccessToken, second.AccessToken)
	assert.Equal(t, first.Generation, second.Generation, "a renewal is the same sign-in")
	assert.Equal(t, "nrt_rotated_2", h.stored(), "and each renewal is written back")
	assert.Equal(t, []string{"nrt_from_login", "nrt_rotated_1"}, h.f.refreshes(),
		"each renewal presents the token the previous one issued")
}

// The done-when: a skewed system clock causes no spurious auth failures. Both directions fail differently
// without the server clock, and both are asserted.
//
// Behind: the instance's expiry looks hours away, so the daemon would hand out a token long expired, and
// the gateway would answer 4004 on every reconnect. Ahead: every token looks expired the moment it
// arrives, so the daemon would refresh in a loop — rotating the account's refresh token as fast as the
// throttle allows, for ever.
func TestASkewedClockCausesNoSpuriousAuthFailures(t *testing.T) {
	for name, skew := range map[string]time.Duration{"six hours behind": -6 * time.Hour,
		"six hours ahead": 6 * time.Hour} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, withSkew(skew))
			h.start()
			_, err := h.current()
			require.NoError(t, err)

			// Twenty-four hours of true time, a minute at a time, asking for a token at every step the way a
			// gateway reconnecting would. Every token handed out must be live by the instance's clock.
			for range 24 * 60 {
				h.clock.Advance(time.Minute)
				h.settle()
				c, err := h.current()
				require.NoError(t, err)
				require.Truef(t, c.ExpiresAt.After(h.clock.Now()),
					"handed out a token that expired at %v; the instance's time is %v", c.ExpiresAt, h.clock.Now())
			}

			// About one renewal per 11¼ minutes. A loop would be hundreds; a daemon that never renewed would
			// have failed the assertion above.
			n := len(h.f.refreshes())
			assert.LessOrEqual(t, n, 24*60/11+2, "refreshed %d times in a day", n)
			assert.GreaterOrEqual(t, n, 24*60/12, "refreshed %d times in a day", n)
		})
	}
}

// After a laptop sleeps, the monotonic clock has stopped and the estimate of the server's clock is behind by
// the nap. The next HELLO corrects it before the token is checked for IDENTIFY.
func TestAHelloAfterSuspendCorrectsTheEstimate(t *testing.T) {
	h := newHarness(t)
	h.start()
	_, err := h.current()
	require.NoError(t, err)

	// What suspend looks like: the true clock moves an hour; the daemon's monotonic clock does not, which
	// here is a clock that stood still. Simulated by skewing the instance's view forward instead.
	h.src.ObserveServerTime(h.clock.Now().Add(time.Hour))

	_, ok := h.tryCurrent()
	assert.False(t, ok, "a token expired by the instance's clock must not be handed to IDENTIFY")
	h.advanceUntil(time.Second, 15*time.Second, h.hasToken("eyJ.access.2"))
}

// ---------- what the gateway reports ----------

// 4004: the instance refused the access token. Renew it, once.
func TestARejectedTokenIsRenewed(t *testing.T) {
	h := newHarness(t)
	h.start()
	c, err := h.current()
	require.NoError(t, err)

	h.src.Rejected(c.AccessToken)
	_, ok := h.tryCurrent()
	assert.False(t, ok, "a refused token is not handed out again")
	h.advanceUntil(time.Second, 15*time.Second, h.hasToken("eyJ.access.2"))

	// A refusal of a token already replaced is stale news, and costs nothing.
	h.src.Rejected(c.AccessToken)
	assert.False(t, waitBriefly(func() bool { return len(h.f.refreshes()) > 2 }))
}

// However often something asks, refreshes are spaced: a gateway refusing every token must not become a loop
// rotating the account's refresh token as fast as the network allows.
func TestRefreshesAreNeverCloserThanTheFloor(t *testing.T) {
	h := newHarness(t)
	h.start()
	_, err := h.current()
	require.NoError(t, err)

	for range 50 {
		if c, ok := h.tryCurrent(); ok {
			h.src.Rejected(c.AccessToken)
		}
		h.clock.Advance(time.Second)
		time.Sleep(5 * time.Millisecond)
	}
	// Fifty seconds of true time, a refusal every second: one refresh per ten seconds at most.
	n := len(h.f.refreshes())
	assert.LessOrEqual(t, n, 1+50/10+1, "refreshed %d times in fifty seconds", n)
}

// 4011 on a running daemon with a store nobody changed: the sign-in is over. Checked by presenting the
// token, which the instance refuses, and then nothing more is presented until somebody signs in.
func TestARevocationWithNoNewLoginSignsOut(t *testing.T) {
	h := newHarness(t)
	h.start()
	_, err := h.current()
	require.NoError(t, err)

	h.f.set(func(f *fakeInstance) { f.status, f.body = http.StatusUnauthorized, `{}` })
	h.src.Revoked()
	h.advanceUntil(time.Second, 15*time.Second, func() bool { return len(h.f.refreshes()) == 2 })
	h.noCredential()
	assert.Contains(t, h.logs.String(), "norite login` again")

	h.clock.Advance(time.Hour)
	assert.Len(t, h.f.refreshes(), 2, "a refused token is not presented again")
}

// 4011 because `norite login` on this machine superseded the daemon's sign-in: the store now holds the
// login's credential, and the daemon carries on with it. This is how a login reaches a running daemon at
// M19, with no socket and no watcher.
func TestARevocationAfterALoginAdoptsTheNewCredential(t *testing.T) {
	h := newHarness(t)
	h.start()
	first, err := h.current()
	require.NoError(t, err)

	loginTo(t, h.store, h.f.server.URL, "nrt_new_login")
	h.src.Revoked()

	h.advanceUntil(time.Second, 15*time.Second, func() bool {
		c, ok := h.tryCurrent()
		return ok && c.Generation != first.Generation
	})
	c, err := h.current()
	require.NoError(t, err)
	assert.Equal(t, "grace", c.Username)
	assert.Equal(t, "nrt_new_login", h.f.refreshes()[1], "the login's token is the one presented next")
	assert.Equal(t, "nrt_rotated_1", h.f.handedBackToken(),
		"what the daemon held belonged to a sign-in that is over, and is handed back")
}

// The same login as it actually arrives. The instance closes 4011 the moment the login's sign-in commits,
// before `norite login` has had its answer, so the daemon hears of the revocation while the store still
// holds what it knew and sets out to settle it by refreshing — which the floor holds back when the last
// renewal was recent, as it is after another login a moment before. The login is written in that wait. Waiting
// it out and then presenting the superseded token cost a refusal and up to ten seconds (M19 manual pass). Past
// the floor the refresh goes at once, loses the race to the login's write and costs one refusal, which is how a
// revocation that is not a login is told apart from one that is.
func TestALoginWrittenAfterItsRevocationIsAdoptedWithoutPresentingTheOldToken(t *testing.T) {
	h := newHarness(t)
	h.start()
	first, err := h.current()
	require.NoError(t, err)

	h.src.Revoked()
	require.Eventually(t, h.clock.waitingWithin(minRefreshGap), time.Second, time.Millisecond,
		"the refresh that settles the revocation is held back by the floor")

	loginTo(t, h.store, h.f.server.URL, "nrt_new_login")
	h.src.Reload() // what the watch does once the login has written the store

	h.advanceUntil(time.Second, 15*time.Second, func() bool {
		c, ok := h.tryCurrent()
		return ok && c.Generation != first.Generation
	})
	assert.Equal(t, []string{"nrt_from_login", "nrt_new_login"}, h.f.refreshes(),
		"the superseded token is never presented")
}

// ---------- the store ----------

// A login or a logout that lands while a refresh is in flight owns the store, and the daemon must leave it
// alone: reading the record before a network round trip and writing it back after is a read-modify-write
// across a released lock. Once the login has finished, the daemon signs in with what it stored.
func TestALoginDuringTheRefreshIsAdoptedNotUndone(t *testing.T) {
	h := newHarness(t)
	other := newFakeInstance(t, h.clock)
	h.f.set(func(f *fakeInstance) {
		f.beforeRefresh = func() { loginTo(t, h.store, other.server.URL, "nrt_the_login_just_stored") }
	})
	h.start()

	h.advanceUntil(time.Second, 15*time.Second, func() bool { return len(other.refreshes()) == 1 })
	c, err := h.current()
	require.NoError(t, err)
	assert.Equal(t, other.server.URL, c.InstanceURL)
	assert.Equal(t, uint64(2), c.Generation)
	assert.Equal(t, []string{"nrt_the_login_just_stored"}, other.refreshes())
	assert.Contains(t, h.logs.String(), "leaving it alone")

	record, err := h.store.LoadRecord()
	require.NoError(t, err)
	assert.Equal(t, other.server.URL, record.InstanceURL, "the login's record must survive")
}

// The token the colliding login made unkeepable goes back to the instance that issued it (M11) — not to
// the one the store names now, which has never seen it.
func TestADroppedTokenGoesBackToItsOwnInstance(t *testing.T) {
	h := newHarness(t)
	other := newFakeInstance(t, h.clock)
	h.f.set(func(f *fakeInstance) {
		f.beforeRefresh = func() { loginTo(t, h.store, other.server.URL, "nrt_the_login_just_stored") }
	})
	h.start()

	h.advanceUntil(time.Second, 15*time.Second, func() bool { return h.f.handedBackToken() != "" })
	assert.Equal(t, "nrt_rotated_1", h.f.handedBackToken(), "the issuing instance is the one told to revoke it")
	assert.Empty(t, other.handedBackToken(), "the instance the login switched to never saw this token")
	require.Eventually(t, func() bool {
		return strings.Contains(h.logs.String(), "revoked the token this daemon could no longer keep")
	}, time.Second, 5*time.Millisecond)
}

// A hand-back that fails is logged and survived: a token it could not revoke leaves exactly the situation
// that existed before hand-backs did.
func TestAFailedHandBackDoesNotStopTheDaemon(t *testing.T) {
	for name, arrange := range map[string]func(*fakeInstance){
		"the instance refuses it":       func(f *fakeInstance) { f.logoutStatus = http.StatusInternalServerError },
		"the connection drops under it": func(f *fakeInstance) { f.logoutBroken = true },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			other := newFakeInstance(t, h.clock)
			h.f.set(func(f *fakeInstance) {
				arrange(f)
				f.beforeRefresh = func() { loginTo(t, h.store, other.server.URL, "nrt_the_login_just_stored") }
			})
			h.start()

			// Signing in with the login's credential afterwards is the assertion that nothing stopped.
			h.advanceUntil(time.Second, 15*time.Second, func() bool { return len(other.refreshes()) == 1 })
			c, err := h.current()
			require.NoError(t, err)
			assert.Equal(t, other.server.URL, c.InstanceURL)
		})
	}
}

// A lock the daemon could not take says nothing about what is on disk. M7 handed the renewed token back
// here and went without a session, which on a running daemon signs it out over a keyring that hesitated
// once. Kept instead — and the next write-back has to name the token the store *still* holds as the one
// spent, or it is refused as though somebody had logged in.
func TestAnUnavailableStoreKeepsTheSessionAndCatchesUpLater(t *testing.T) {
	h := newHarness(t)
	held := flock.New(filepath.Join(h.dir, "credentials.lock"))
	h.f.set(func(f *fakeInstance) { f.beforeRefresh = func() { require.NoError(t, held.Lock()) } })
	t.Cleanup(func() { _ = held.Unlock() })
	h.start()

	c, err := h.current()
	require.NoError(t, err, "a store that could not be written must not cost the session")
	assert.Equal(t, "eyJ.access.1", c.AccessToken)
	assert.Contains(t, h.logs.String(), "keeping the session")
	assert.Empty(t, h.f.handedBackToken(), "the renewed token is held, so nothing is handed back")

	require.NoError(t, held.Unlock())
	assert.Equal(t, "nrt_from_login", h.stored(), "the store still holds what the login stored")

	// The next renewal presents what this process holds and writes over what the store holds.
	h.advanceUntil(30*time.Second, 15*time.Minute, func() bool { return len(h.f.refreshes()) == 2 })
	assert.Equal(t, "nrt_rotated_1", h.f.refreshes()[1])
	require.Eventually(t, func() bool { return h.stored() == "nrt_rotated_2" }, time.Second, 5*time.Millisecond,
		"the store catches up — which needs the write-back to name the token the store held, not the one spent")
	_, err = h.current()
	require.NoError(t, err, "and the session is still the same one")
	assert.NotContains(t, h.logs.String(), "changed while it was being renewed")
}

// A writer that is merely finishing is waited out by the lock itself, so the write-back lands.
func TestARenewalWaitsOutABrieflyHeldLock(t *testing.T) {
	h := newHarness(t)
	held := flock.New(filepath.Join(h.dir, "credentials.lock"))
	h.f.set(func(f *fakeInstance) {
		f.beforeRefresh = func() {
			require.NoError(t, held.Lock())
			go func() {
				time.Sleep(20 * time.Millisecond)
				_ = held.Unlock()
			}()
		}
	})
	h.start()

	_, err := h.current()
	require.NoError(t, err)
	assert.Equal(t, "nrt_rotated_1", h.stored(), "a lock held briefly must not cost the renewed token")
}

// A store that refuses the write-back still holds the token just spent. M7 cleared it, which on a running
// daemon meant signing out over one failed write (M19 /code-review). The session is kept instead, and what a
// restart would present is settled when the daemon stops: the write is tried one last time.
func TestARefusedWriteKeepsTheSessionAndIsSettledAtShutdown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix directory modes do not describe Windows ACLs")
	}
	if os.Geteuid() == 0 {
		t.Skip("root writes a directory whatever its mode")
	}
	h := newHarness(t)
	h.f.set(func(f *fakeInstance) {
		f.beforeRefresh = func() { require.NoError(t, os.Chmod(h.dir, 0o500)) }
	})
	t.Cleanup(func() { _ = os.Chmod(h.dir, 0o700) })
	h.start()

	c, err := h.current()
	require.NoError(t, err, "a refused write must not cost the session")
	assert.Equal(t, "eyJ.access.1", c.AccessToken)
	assert.Contains(t, h.logs.String(), "keeping the session")
	assert.Empty(t, h.f.handedBackToken())

	require.NoError(t, os.Chmod(h.dir, 0o700))
	assert.Equal(t, "nrt_from_login", h.stored(), "the store still holds the spent token")
	h.stop()
	assert.Equal(t, "nrt_rotated_1", h.stored(), "stopping stores what the daemon held")
	assert.Contains(t, h.logs.String(), "stored the renewed credential before stopping")
}

// A refresh that is failing waits out its backoff, however often something asks for a token. Obeying every
// nudge let each gateway reconnect, and from M20 each attached client, skip the backoff (M19 /code-review).
func TestAFailingRefreshKeepsItsBackoffWhenAsked(t *testing.T) {
	h := newHarness(t)
	h.start()
	_, err := h.current()
	require.NoError(t, err)

	h.f.set(func(f *fakeInstance) { f.status = http.StatusServiceUnavailable })
	h.advanceUntil(30*time.Second, 15*time.Minute, func() bool { return len(h.f.refreshes()) == 2 })

	// Past the refresh point, every Current nudges. None of it may produce a refresh before the backoff.
	for range 50 {
		h.tryCurrent()
	}
	assert.False(t, waitBriefly(func() bool { return len(h.f.refreshes()) > 2 }),
		"a nudge cut a failing refresh's backoff short")
}

// Ended is how the gateway connection learns that the account it is streaming is no longer signed in.
func TestEndedClosesWhenTheSignInIsOver(t *testing.T) {
	h := newHarness(t)
	h.start()
	c, err := h.current()
	require.NoError(t, err)

	ended := h.src.Ended(c.Generation)
	select {
	case <-ended:
		t.Fatal("a live sign-in has not ended")
	default:
	}

	require.NoError(t, h.store.Clear()) // a logout
	h.src.Reload()
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("a logout did not end the sign-in")
	}

	select {
	case <-h.src.Ended(c.Generation + 7):
	default:
		t.Fatal("a generation that is not live has ended, by definition")
	}
}

// The keyring done-when. A systemd user unit can start the daemon before the session keyring unlocks; the
// record names the keyring and the read fails. M7 tried once and ran with no session for its whole life.
// Simulated with the file backend made unreadable, which fails Load the same way: an error that is not
// ErrNoCredential.
func TestADaemonStartedBeforeItsKeyringUnlocksReachesItAfterwards(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file modes do not describe Windows ACLs")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	h := newHarness(t)
	secrets, err := filepath.Glob(filepath.Join(h.dir, "token-*"))
	require.NoError(t, err)
	require.Len(t, secrets, 1)
	require.NoError(t, os.Chmod(secrets[0], 0o000))
	t.Cleanup(func() { _ = os.Chmod(secrets[0], 0o600) })
	h.start()

	h.noCredential()
	assert.Contains(t, h.logs.String(), "could not be read; trying again")
	assert.Empty(t, h.f.refreshes())

	// The keyring unlocks.
	require.NoError(t, os.Chmod(secrets[0], 0o600))
	h.advanceUntil(time.Second, 10*time.Second, func() bool { return len(h.f.refreshes()) == 1 })
	c, err := h.current()
	require.NoError(t, err)
	assert.Equal(t, "ada", c.Username)
}

// A logout leaves no record. A daemon holding a live session hands its token back — it is the only live
// credential for this device, and nobody else will ever present it — and stops.
func TestALogoutEndsTheSessionAndHandsTheTokenBack(t *testing.T) {
	h := newHarness(t)
	h.start()
	_, err := h.current()
	require.NoError(t, err)

	require.NoError(t, h.store.Clear())
	h.src.Reload()
	require.Eventually(t, func() bool { return h.f.handedBackToken() == "nrt_rotated_1" },
		time.Second, 5*time.Millisecond)
	h.noCredential()
}

// A reload that finds the store as this process left it costs nothing: no refresh, same session.
func TestAReloadOfAnUnchangedStoreChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.start()
	before, err := h.current()
	require.NoError(t, err)

	h.src.Reload()
	assert.False(t, waitBriefly(func() bool { return len(h.f.refreshes()) > 1 }))
	after, err := h.current()
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

// ---------- failures the daemon must survive ----------

// A daemon that refuses to start because nobody has logged in cannot be installed before its first login,
// and `norite daemon install` deliberately runs before anything else (M3).
func TestNoStoredCredentialIsNotAFailure(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.store.Clear())
	h.start()

	h.noCredential()
	assert.Empty(t, h.f.refreshes(), "with nothing stored there is nothing to present")
	assert.Contains(t, h.logs.String(), "norite login", "the log must say how to fix it")

	// And signing in later reaches the running daemon.
	loginTo(t, h.store, h.f.server.URL, "nrt_later")
	h.src.Reload()
	c, err := h.current()
	require.NoError(t, err)
	assert.Equal(t, "grace", c.Username)
}

// An unreachable instance is waited out, not given up on.
func TestAnUnreachableInstanceIsRetried(t *testing.T) {
	h := newHarness(t)
	h.f.set(func(f *fakeInstance) { f.status = http.StatusServiceUnavailable })
	h.start()

	h.advanceUntil(time.Second, 10*time.Second, func() bool { return len(h.f.refreshes()) >= 3 })
	h.noCredential()
	assert.Contains(t, h.logs.String(), "could not renew the session; trying again")
	assert.Equal(t, "nrt_from_login", h.stored(), "nothing the instance did not acknowledge was spent")

	h.f.set(func(f *fakeInstance) { f.status = 0 })
	h.advanceUntil(time.Second, 10*time.Second, func() bool { _, ok := h.tryCurrent(); return ok })
}

// A refused token is the ordinary outcome of a password reset, a logout elsewhere, or reuse detection. The
// log points at the fix, the stored credential is left alone, and nothing is presented again.
func TestARefusedTokenSaysWhatToDo(t *testing.T) {
	h := newHarness(t)
	h.f.set(func(f *fakeInstance) {
		f.status = http.StatusUnauthorized
		f.body = `{"error":{"code":"unauthorized","message":"invalid or expired refresh token"}}`
	})
	h.start()

	h.advanceUntil(time.Second, 10*time.Second, func() bool {
		return strings.Contains(h.logs.String(), "norite login` again")
	})
	h.noCredential()
	assert.Equal(t, "nrt_from_login", h.stored(),
		"clearing would turn a transient instance-side problem into a lost session")
	h.clock.Advance(time.Hour)
	assert.Len(t, h.f.refreshes(), 1, "a refused token is not presented again")
}

// Half a pair would leave the next start holding a token the instance has already rotated away from.
func TestAnIncompletePairIsRefusedAndNothingIsStored(t *testing.T) {
	h := newHarness(t)
	h.f.set(func(f *fakeInstance) { f.status, f.body = http.StatusOK, `{"access_token":"eyJ.a.b"}` })
	h.start()

	h.advanceUntil(time.Second, 10*time.Second, func() bool { return len(h.f.refreshes()) >= 2 })
	h.noCredential()
	assert.Equal(t, "nrt_from_login", h.stored(), "the original must survive a response that made no sense")
}

// ---------- rule 8 ----------

// The log is the one artifact from all of this that gets pasted into a bug report — and on every path:
// renewals, refusals, failures, hand-backs.
func TestNoTokenEverReachesTheLog(t *testing.T) {
	h := newHarness(t)
	h.start()
	_, err := h.current()
	require.NoError(t, err)
	h.advanceUntil(time.Minute, time.Hour, func() bool { return len(h.f.refreshes()) >= 4 })

	h.f.set(func(f *fakeInstance) { f.status = http.StatusBadGateway })
	h.clock.Advance(15 * time.Minute)
	waitBriefly(func() bool { return false })
	h.f.set(func(f *fakeInstance) { f.status = http.StatusUnauthorized })
	h.src.Revoked()
	h.advanceUntil(time.Second, 30*time.Second, func() bool {
		return strings.Contains(h.logs.String(), "norite login` again")
	})

	logs := h.logs.String()
	assert.NotContains(t, logs, "nrt_")
	assert.NotContains(t, logs, "eyJ.")
}

func TestHandingBackATokenLogsNoToken(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusInternalServerError} {
		h := newHarness(t)
		other := newFakeInstance(t, h.clock)
		h.f.set(func(f *fakeInstance) {
			f.logoutStatus = status
			f.beforeRefresh = func() { loginTo(t, h.store, other.server.URL, "nrt_the_login_just_stored") }
		})
		h.start()
		h.advanceUntil(time.Second, 15*time.Second, func() bool { return h.f.handedBackToken() != "" })
		h.stop()

		assert.NotContains(t, h.logs.String(), "nrt_", "status %d", status)
	}
}

// ---------- the pieces ----------

func TestRefreshDueIsAQuarterOfTheRemainingLifeBeforeExpiry(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, now.Add(11*time.Minute+15*time.Second), refreshDue(now.Add(15*time.Minute), now))
	assert.Equal(t, now.Add(minRefreshGap), refreshDue(now.Add(5*time.Second), now),
		"never sooner than the floor, however short the life")
	assert.Equal(t, now.Add(minRefreshGap), refreshDue(now.Add(-time.Hour), now),
		"an expiry in the past is the floor, not a loop")
}

func TestServerClockAdvancesOnTheLocalClockFromItsLastSample(t *testing.T) {
	local := newFakeClock()
	c := &serverClock{local: skewedClock{fakeClock: local, skew: -6 * time.Hour}}
	assert.Equal(t, local.Now().Add(-6*time.Hour), c.now(), "before a sample, the local clock is all there is")

	c.observe(local.Now())
	local.Advance(90 * time.Second)
	assert.Equal(t, local.Now(), c.now(), "after one, the instance's time plus what has passed since")

	c.observe(time.Time{})
	assert.Equal(t, local.Now(), c.now(), "a missing Date header is no sample")
}
