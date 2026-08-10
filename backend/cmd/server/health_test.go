package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubPinger struct {
	mu     sync.Mutex
	err    error
	called int
}

func (s *stubPinger) Ping(context.Context) (int32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.called++
	if s.err != nil {
		return 0, s.err
	}
	return 1, nil
}

func (s *stubPinger) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.called
}

func TestHealthReportsStartingUntilMarkedReady(t *testing.T) {
	pinger := &stubPinger{}
	h := newHealth(pinger)

	status, body := callHealth(t, h)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, "starting", body.Status)
	assert.Contains(t, body.Detail, "starting up")

	// Not ready means not even asking the database: the answer is already known, and a probe should not
	// generate load against a database that is still being migrated.
	assert.Zero(t, pinger.calls())

	h.MarkReady()

	status, body = callHealth(t, h)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "ok", body.Status)
	assert.Equal(t, 1, pinger.calls())
}

func TestHealthReportsDegradedWhenTheDatabaseIsUnreachable(t *testing.T) {
	h := newHealth(&stubPinger{err: errors.New("dial tcp 10.0.0.3:5432: connect: connection refused")})
	h.MarkReady()

	status, body := callHealth(t, h)

	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, "degraded", body.Status)
	// This endpoint is unauthenticated, so it must not disclose internal hostnames or driver detail.
	assert.NotContains(t, body.Detail, "10.0.0.3")
}

// A draining instance must not claim to be booting. An operator watching a rolling restart would otherwise
// see the exact opposite of what is happening, including a false hint that a migration may be running.
func TestHealthReportsStoppingDuringShutdown(t *testing.T) {
	h := newHealth(&stubPinger{})
	h.MarkReady()
	h.MarkStopping()

	status, body := callHealth(t, h)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, "stopping", body.Status)
	assert.Contains(t, body.Detail, "shutting down")
	assert.NotContains(t, body.Detail, "migrations")
}

// The endpoint is unauthenticated and exempt from rate limiting, so request rate must not translate into
// database load — otherwise a flood could exhaust the intentionally small pool.
func TestHealthCachesTheDatabaseProbe(t *testing.T) {
	pinger := &stubPinger{}
	h := newHealth(pinger)
	h.MarkReady()

	clock := time.Now()
	h.now = func() time.Time { return clock }

	for i := 0; i < 50; i++ {
		status, _ := callHealth(t, h)
		require.Equal(t, http.StatusOK, status)
	}
	assert.Equal(t, 1, pinger.calls(), "50 probes within the TTL must cost one query")

	// Past the TTL the result is refreshed, so a database that goes away is still noticed promptly.
	clock = clock.Add(healthCacheTTL + time.Millisecond)
	status, _ := callHealth(t, h)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, 2, pinger.calls())
}

func TestHealthCacheExpiryPicksUpFailure(t *testing.T) {
	pinger := &stubPinger{}
	h := newHealth(pinger)
	h.MarkReady()

	clock := time.Now()
	h.now = func() time.Time { return clock }

	status, _ := callHealth(t, h)
	require.Equal(t, http.StatusOK, status)

	pinger.mu.Lock()
	pinger.err = errors.New("connection refused")
	pinger.mu.Unlock()

	// Still cached: the previous success stands until the TTL lapses.
	status, _ = callHealth(t, h)
	assert.Equal(t, http.StatusOK, status)

	clock = clock.Add(healthCacheTTL + time.Millisecond)
	status, body := callHealth(t, h)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, "degraded", body.Status)
}

// A client that hangs up must not take the cached result down with it — the probe runs on a detached
// context so one canceled request can't poison the answer for everyone else.
func TestHealthProbeSurvivesRequestCancellation(t *testing.T) {
	pinger := &stubPinger{}
	h := newHealth(pinger)
	h.MarkReady()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, h.probe(ctx))
	assert.Equal(t, 1, pinger.calls())
}

func callHealth(t *testing.T, h *health) (int, healthResponse) {
	t.Helper()

	rec := httptest.NewRecorder()
	h.Handler(rec, httptest.NewRequest(http.MethodGet, healthzPath, nil))

	var body healthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return rec.Code, body
}
