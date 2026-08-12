package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/config"
)

func testConfig() config.Config {
	return config.Config{
		Env:                config.EnvDevelopment,
		ListenAddr:         ":8080",
		DatabaseURL:        "postgres://norite:norite@localhost:5432/norite?sslmode=disable",
		DBMaxConns:         4,
		DBMinConns:         0,
		DBConnectTimeout:   10 * time.Second,
		MigrateLockTimeout: time.Minute,
		LogLevel:           "info",
		LogFormat:          "json",
		RateLimit:          "600-M",
		ShutdownTimeout:    15 * time.Second,
	}
}

func newTestRouter(t *testing.T, cfg config.Config, h *health) http.Handler {
	t.Helper()

	router, err := newRouter(routerOptions{
		Config: cfg,
		Logger: zerolog.New(io.Discard),
		Health: h,
	})
	require.NoError(t, err)
	return router
}

func readyRouter(t *testing.T, cfg config.Config) http.Handler {
	t.Helper()

	h := newHealth(&stubPinger{})
	h.MarkReady()
	return newTestRouter(t, cfg, h)
}

func TestRouterServesHealthzUnderTheAPIPrefix(t *testing.T) {
	router := readyRouter(t, testConfig())

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
}

// Probes are frequent and single-source. If the rate limiter covered readiness, a healthy instance would
// eventually report itself down — the limiter would manufacture the outage it exists to help survive.
func TestRouterDoesNotRateLimitHealthz(t *testing.T) {
	cfg := testConfig()
	cfg.RateLimit = "1-M" // one request per minute would throttle the second probe immediately
	router := readyRouter(t, cfg)

	for i := 0; i < 25; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil)
		req.RemoteAddr = "203.0.113.7:1000"
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "probe %d was throttled", i+1)
	}
}

// A wrong verb must be a 405 with Allow, not a generic 400 — a client needs to know the route exists and
// which verbs it takes.
func TestRouterWrongMethodReturns405WithAllow(t *testing.T) {
	router := readyRouter(t, testConfig())

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, healthzPath, nil))

	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, "GET, HEAD", rec.Header().Get("Allow"), "RFC 9110 requires Allow on a 405")

	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "method_not_allowed", body.Error.Code)
}

// chi registers GET and HEAD separately, so a GET-only route answers HEAD with 405. `curl -I` and several
// load balancers probe with HEAD, so readiness must accept it.
func TestRouterHealthzAnswersHEAD(t *testing.T) {
	router := readyRouter(t, testConfig())

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, healthzPath, nil))

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRouterUnknownRouteReturnsTheErrorEnvelope(t *testing.T) {
	router := newTestRouter(t, testConfig(), newHealth(&stubPinger{}))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil))

	require.Equal(t, http.StatusNotFound, rec.Code)

	var body struct {
		Error struct {
			Code      string `json:"code"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "not_found", body.Error.Code)
	assert.NotEmpty(t, body.Error.RequestID, "even a 404 carries the correlation ID")
}

// Group-mounted rate limiting would leave unmatched paths unthrottled, letting an attacker flood 404s at
// unlimited rate. The limiter sits on the root chain precisely so this is covered.
func TestRouterRateLimitsUnmatchedPaths(t *testing.T) {
	cfg := testConfig()
	cfg.RateLimit = "3-M"
	router := readyRouter(t, cfg)

	var lastCode int
	for i := 0; i < 6; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil)
		req.RemoteAddr = "198.51.100.7:1000"
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		lastCode = rec.Code
	}
	assert.Equal(t, http.StatusTooManyRequests, lastCode, "404 floods must be throttled, not free")
}

func TestRouterSetsSecureHeadersOnEveryResponse(t *testing.T) {
	router := readyRouter(t, testConfig())

	for _, path := range []string{"/api/v1/healthz", "/api/v1/nope"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"), "path %s", path)
		assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"), "path %s", path)
		assert.NotEmpty(t, rec.Header().Get("Content-Security-Policy"), "path %s", path)
	}
}

func TestRouterSetsARequestIDHeader(t *testing.T) {
	router := readyRouter(t, testConfig())

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil))

	assert.NotEmpty(t, rec.Header().Get("X-Request-Id"))
}

// The sanitizer only works if it is mounted above chi's RequestID, which is an ordering fact no unit test
// of the middleware itself can show. Asserted here on the assembled chain, where getting it wrong would
// echo the caller's own string straight back at them.
func TestRouterDoesNotAdoptAClientSuppliedRequestID(t *testing.T) {
	for _, trust := range []bool{false, true} {
		cfg := testConfig()
		cfg.TrustProxyHeaders = trust
		cfg.TrustedProxyHops = 1
		router := readyRouter(t, cfg)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil)
		// Untrusted this must be discarded outright; trusted it must still fail the charset filter.
		req.Header.Set("X-Request-Id", "spoofed id\nwith a newline")

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		echoed := rec.Header().Get("X-Request-Id")
		assert.NotEmpty(t, echoed, "trust_proxy_headers=%v", trust)
		assert.NotContains(t, echoed, "spoofed", "trust_proxy_headers=%v", trust)
		assert.NotContains(t, echoed, "\n", "a header value must never carry a newline")
	}
}

// A misconfigured rate string must fail at construction, before the process starts listening — not on the
// first request that happens to hit a limited route.
func TestRouterRejectsAnInvalidRateLimitConfig(t *testing.T) {
	cfg := testConfig()
	cfg.RateLimit = "not-a-rate"

	_, err := newRouter(routerOptions{
		Config: cfg,
		Logger: zerolog.New(io.Discard),
		Health: newHealth(&stubPinger{}),
	})
	require.Error(t, err)
}

// Turning on proxy trust adds httpx.RealIP to the chain. The observable effect asserted here is that the
// router still builds and serves correctly in both modes; the security-relevant halves of that decision —
// that the rate-limit key and the HSTS header both ignore untrusted forwarded headers — are asserted in
// the ratelimit and httpx packages, where the logic actually lives.
func TestRouterBuildsInBothProxyTrustModes(t *testing.T) {
	for _, trust := range []bool{false, true} {
		cfg := testConfig()
		cfg.TrustProxyHeaders = trust
		router := readyRouter(t, cfg)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil)
		req.RemoteAddr = "203.0.113.7:1000"
		req.Header.Set("X-Forwarded-For", "198.51.100.1")

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, "trust_proxy_headers=%v", trust)
	}
}
