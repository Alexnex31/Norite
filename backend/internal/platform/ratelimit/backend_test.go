// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ratelimit

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/platform/redistest"
)

// eachBackend runs a test against the in-memory store and the Redis one. The Redis store is what the
// flagship counts with once it runs more than one replica (M114), and until M18 it had never been run.
func eachBackend(t *testing.T, test func(t *testing.T, newBackend func() Backend, bucket string)) {
	t.Run("memory", func(t *testing.T) {
		test(t, MemoryBackend, "memory")
	})
	t.Run("redis", func(t *testing.T) {
		url := redistest.URL(t)
		test(t, func() Backend { return newRedisBackend(t, url) }, redistest.Namespace(t))
	})
}

func newRedisBackend(t *testing.T, url string) Backend {
	t.Helper()
	b, err := NewRedisBackend(context.Background(), url, 10*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestALimitHoldsOnEitherBackend(t *testing.T) {
	eachBackend(t, func(t *testing.T, newBackend func() Backend, bucket string) {
		lim, err := New(Options{Rate: "3-M", Bucket: bucket, Backend: newBackend()})
		require.NoError(t, err)

		for i := range 3 {
			res, err := lim.Allow(context.Background(), "account:1")
			require.NoError(t, err)
			assert.True(t, res.Allowed, "attempt %d is within the limit", i+1)
		}
		res, err := lim.Allow(context.Background(), "account:1")
		require.NoError(t, err)
		assert.False(t, res.Allowed, "the fourth attempt in a 3-per-minute window is refused")

		other, err := lim.Allow(context.Background(), "account:2")
		require.NoError(t, err)
		assert.True(t, other.Allowed, "another key counts separately")
	})
}

// The /64 rule is the package's one global invariant, and it lives in the key, so it has to hold whichever
// store counts the key.
func TestIPv6GroupsByPrefixOnEitherBackend(t *testing.T) {
	eachBackend(t, func(t *testing.T, newBackend func() Backend, bucket string) {
		mw, err := Middleware(Options{Rate: "2-M", Bucket: bucket, Backend: newBackend()})
		require.NoError(t, err)
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))

		assert.Equal(t, http.StatusNoContent, do(h, "[2001:db8:1:2::1]:1000").Code)
		assert.Equal(t, http.StatusNoContent, do(h, "[2001:db8:1:2::ffff]:1000").Code)
		assert.Equal(t, http.StatusTooManyRequests, do(h, "[2001:db8:1:2:dead:beef::7]:1000").Code,
			"a third address in the same /64 shares the first two's count")
	})
}

// What the Redis store is for: two replicas limiting one client count together. With a store per process
// the flagship's effective limit would be the configured one times the replica count (M114's done-when).
func TestReplicasSharingRedisCountTogether(t *testing.T) {
	url, bucket := redistest.URL(t), redistest.Namespace(t)
	replicaA, err := New(Options{Rate: "2-M", Bucket: bucket, Backend: newRedisBackend(t, url)})
	require.NoError(t, err)
	replicaB, err := New(Options{Rate: "2-M", Bucket: bucket, Backend: newRedisBackend(t, url)})
	require.NoError(t, err)

	for _, lim := range []*Limiter{replicaA, replicaB} {
		res, err := lim.Allow(context.Background(), "account:1")
		require.NoError(t, err)
		require.True(t, res.Allowed)
	}
	res, err := replicaA.Allow(context.Background(), "account:1")
	require.NoError(t, err)
	assert.False(t, res.Allowed, "the third attempt is refused even though each replica has seen fewer than two")
}

// M1 decided the limiter fails open: a Redis blip must not become an API outage. That branch could not be
// reached until a store could fail, so this is the first time it has run.
func TestAFailedStoreLetsTheRequestThrough(t *testing.T) {
	backend := newRedisBackend(t, redistest.URL(t))
	mw, err := Middleware(Options{Rate: "1-M", Bucket: redistest.Namespace(t), Backend: backend})
	require.NoError(t, err)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))

	require.NoError(t, backend.redis.Close())
	for range 3 {
		rec := do(h, "203.0.113.9:1000")
		assert.Equal(t, http.StatusNoContent, rec.Code, "a store that cannot answer must not refuse the request")
		assert.Empty(t, rec.Header().Get("X-RateLimit-Limit"), "and must not report a limit it did not count")
	}
}
