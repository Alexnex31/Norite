package ratelimit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientKeyGroupsIPv6ByPrefixAndIPv4Exactly(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		want       string
	}{
		{"ipv4 keeps its exact address", "203.0.113.7:51234", "203.0.113.7"},
		{"ipv4 neighbor is a different key", "203.0.113.8:51234", "203.0.113.8"},

		{"ipv6 is masked to /64", "[2001:db8:1:2:3:4:5:6]:51234", "2001:db8:1:2::/64"},
		{"another address in the same /64 maps to the same key", "[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:443", "2001:db8:1:2::/64"},
		{"the /64 network address itself maps there too", "[2001:db8:1:2::]:443", "2001:db8:1:2::/64"},
		{"a neighboring /64 is a different key", "[2001:db8:1:3::1]:443", "2001:db8:1:3::/64"},

		// An IPv4-mapped IPv6 address must not be treated as an IPv6 /64 — otherwise a v4 client could
		// be lumped in with unrelated traffic, or dodge its own v4 counter.
		{"ipv4-mapped ipv6 is treated as ipv4", "[::ffff:203.0.113.7]:51234", "203.0.113.7"},

		{"address without a port still parses", "203.0.113.7", "203.0.113.7"},
		{"unparseable address collapses into one throttled bucket", "not-an-address:1", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tt.remoteAddr
			assert.Equal(t, tt.want, ClientKey(r))
		})
	}
}

// ClientKey must ignore proxy headers outright: whether they are trusted is decided once, upstream, by
// the router. If this package honored them independently, a directly-exposed process would hand every
// caller a free identity per request.
func TestClientKeyIgnoresProxyHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.7:51234"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	r.Header.Set("X-Real-IP", "198.51.100.2")

	assert.Equal(t, "203.0.113.7", ClientKey(r))
}

// The M1 done-when criterion: "a burst of requests from one IPv6 /64 block is throttled as a single
// source." Every request below comes from a *different* IPv6 address inside one /64, so the only way the
// burst gets throttled is if the limiter is grouping by prefix.
func TestBurstFromOneIPv6SubnetIsThrottledAsASingleSource(t *testing.T) {
	const limit = 5
	h := handlerWithLimit(t, strconv.Itoa(limit)+"-M")

	// Distinct addresses, one shared /64.
	addrs := []string{
		"[2001:db8:1:2::1]:1000",
		"[2001:db8:1:2::2]:1001",
		"[2001:db8:1:2:ffff::9]:1002",
		"[2001:db8:1:2:1234:5678:9abc:def0]:1003",
		"[2001:db8:1:2::beef]:1004",
	}
	for i, addr := range addrs {
		rec := do(h, addr)
		require.Equal(t, http.StatusOK, rec.Code, "request %d from %s should be within the limit", i+1, addr)
		assert.Equal(t, strconv.Itoa(limit-i-1), rec.Header().Get("X-RateLimit-Remaining"))
	}

	// The sixth address in that same /64 is over the shared budget.
	rec := do(h, "[2001:db8:1:2::cafe]:1005")
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))

	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "rate_limited", body.Error.Code, "throttling uses the standard error envelope")

	// A different /64 has its own untouched budget — the grouping must not be a global bucket.
	assert.Equal(t, http.StatusOK, do(h, "[2001:db8:1:3::1]:1006").Code)
}

func TestBurstFromOneIPv4AddressIsThrottled(t *testing.T) {
	const limit = 3
	h := handlerWithLimit(t, strconv.Itoa(limit)+"-M")

	for i := 0; i < limit; i++ {
		require.Equal(t, http.StatusOK, do(h, "203.0.113.7:1000").Code, "request %d", i+1)
	}
	assert.Equal(t, http.StatusTooManyRequests, do(h, "203.0.113.7:2000").Code)

	// A different IPv4 address is a different source, unlike the IPv6 case above.
	assert.Equal(t, http.StatusOK, do(h, "203.0.113.8:1000").Code)
}

// Truncating toward zero would omit Retry-After for the whole final second of a window — exactly the
// retries about to succeed — and a literal "0" would invite an immediate retry that is still refused.
func TestRetryAfterSecondsAlwaysAtLeastOne(t *testing.T) {
	// The reset instant is a whole Unix second (that is all the store records), so any sub-second
	// remainder comes from where "now" falls inside that second.
	const resetUnix = 1_000_002

	tests := []struct {
		name string
		now  time.Time
		want int
	}{
		{"well inside the window", time.Unix(999_960, 0), 42},
		{"a fractional remainder rounds up", time.Unix(1_000_000, 500_000_000), 2},
		{"under a second still asks for one", time.Unix(1_000_001, 800_000_000), 1},
		{"exactly at the reset asks for one", time.Unix(resetUnix, 0), 1},
		{"already elapsed still asks for one", time.Unix(1_000_007, 0), 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := retryAfterSeconds(resetUnix, tt.now)
			assert.Equal(t, tt.want, got)
			assert.GreaterOrEqual(t, got, 1, "Retry-After must never be zero or negative")
		})
	}
}

func TestMiddlewareSetsRateLimitHeaders(t *testing.T) {
	h := handlerWithLimit(t, "10-M")
	rec := do(h, "203.0.113.7:1000")

	assert.Equal(t, "10", rec.Header().Get("X-RateLimit-Limit"))
	assert.Equal(t, "9", rec.Header().Get("X-RateLimit-Remaining"))
	assert.NotEmpty(t, rec.Header().Get("X-RateLimit-Reset"))
}

func TestSeparateBucketsCountIndependently(t *testing.T) {
	base := handlerWithLimitAndBucket(t, "1-M", "base")
	auth := handlerWithLimitAndBucket(t, "1-M", "auth")

	require.Equal(t, http.StatusOK, do(base, "203.0.113.7:1000").Code)
	assert.Equal(t, http.StatusTooManyRequests, do(base, "203.0.113.7:1000").Code)

	// Same client, different bucket: unaffected. This is what lets M4 add a stricter /auth/* limit
	// without stealing budget from the base one.
	assert.Equal(t, http.StatusOK, do(auth, "203.0.113.7:1000").Code)
}

func TestMiddlewareRejectsInvalidRate(t *testing.T) {
	_, err := Middleware(Options{Rate: "lots-per-fortnight"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid rate")
}

func handlerWithLimit(t *testing.T, rate string) http.Handler {
	t.Helper()
	return handlerWithLimitAndBucket(t, rate, "test")
}

func handlerWithLimitAndBucket(t *testing.T, rate, bucket string) http.Handler {
	t.Helper()

	mw, err := Middleware(Options{Rate: rate, Bucket: bucket})
	require.NoError(t, err)

	return mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func do(h http.Handler, remoteAddr string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}
