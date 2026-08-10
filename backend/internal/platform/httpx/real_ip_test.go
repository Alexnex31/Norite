package httpx

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRealIP(t *testing.T) {
	tests := []struct {
		name       string
		hops       int
		remoteAddr string
		forwarded  []string
		want       string
	}{
		{
			name:       "single proxy takes the entry the proxy appended",
			hops:       1,
			remoteAddr: "10.0.0.1:4000", // the proxy itself
			forwarded:  []string{"203.0.113.7"},
			want:       "203.0.113.7:4000",
		},
		{
			// The attack chi's version is vulnerable to: the client pre-seeds the header and the proxy
			// appends the address it actually saw. Counting from the right ignores the injected value.
			name:       "a client-injected leading entry is ignored",
			hops:       1,
			remoteAddr: "10.0.0.1:4000",
			forwarded:  []string{"198.51.100.9, 203.0.113.7"},
			want:       "203.0.113.7:4000",
		},
		{
			name:       "many injected entries are still ignored",
			hops:       1,
			remoteAddr: "10.0.0.1:4000",
			forwarded:  []string{"1.1.1.1, 2.2.2.2, 3.3.3.3, 203.0.113.7"},
			want:       "203.0.113.7:4000",
		},
		{
			name:       "two trusted hops count two from the right",
			hops:       2,
			remoteAddr: "10.0.0.1:4000",
			forwarded:  []string{"198.51.100.9, 203.0.113.7, 10.0.0.9"},
			want:       "203.0.113.7:4000",
		},
		{
			// RFC 7230 §3.2.2: repeated headers are equivalent to one comma-joined value, and proxies do
			// emit them separately. Positions must be counted across the flattened list.
			name:       "repeated header lines are flattened before counting",
			hops:       1,
			remoteAddr: "10.0.0.1:4000",
			forwarded:  []string{"198.51.100.9", "203.0.113.7"},
			want:       "203.0.113.7:4000",
		},
		{
			name:       "ipv6 entry",
			hops:       1,
			remoteAddr: "10.0.0.1:4000",
			forwarded:  []string{"2001:db8:1:2::1"},
			want:       "[2001:db8:1:2::1]:4000",
		},
		{
			name:       "bracketed ipv6 with a port",
			hops:       1,
			remoteAddr: "10.0.0.1:4000",
			forwarded:  []string{"[2001:db8:1:2::1]:9000"},
			want:       "[2001:db8:1:2::1]:4000",
		},
		{
			name:       "ipv4 with a port",
			hops:       1,
			remoteAddr: "10.0.0.1:4000",
			forwarded:  []string{"203.0.113.7:9000"},
			want:       "203.0.113.7:4000",
		},
		{
			// Failing closed: without a trustworthy value the proxy's own address stands. Less precise,
			// but never attacker-chosen.
			name:       "no header leaves RemoteAddr untouched",
			hops:       1,
			remoteAddr: "10.0.0.1:4000",
			forwarded:  nil,
			want:       "10.0.0.1:4000",
		},
		{
			name:       "fewer entries than trusted hops leaves RemoteAddr untouched",
			hops:       2,
			remoteAddr: "10.0.0.1:4000",
			forwarded:  []string{"203.0.113.7"},
			want:       "10.0.0.1:4000",
		},
		{
			name:       "a garbage entry at the trusted position is rejected",
			hops:       1,
			remoteAddr: "10.0.0.1:4000",
			forwarded:  []string{"203.0.113.7, not-an-ip"},
			want:       "10.0.0.1:4000",
		},
		{
			name:       "empty entries are skipped, not counted",
			hops:       1,
			remoteAddr: "10.0.0.1:4000",
			forwarded:  []string{"198.51.100.9, , 203.0.113.7"},
			want:       "203.0.113.7:4000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen string
			h := RealIP(tt.hops)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				seen = r.RemoteAddr
			}))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remoteAddr
			for _, v := range tt.forwarded {
				req.Header.Add("X-Forwarded-For", v)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)

			assert.Equal(t, tt.want, seen)

			// Whatever the outcome, RemoteAddr must keep net/http's documented "IP:port" shape so
			// downstream net.SplitHostPort callers don't silently get an empty host.
			_, _, err := net.SplitHostPort(seen)
			assert.NoError(t, err, "RemoteAddr must stay parseable as host:port")
		})
	}
}

// These carry no positional information, so a value set by a trusted proxy is indistinguishable from one
// a client sent. Honoring them would reopen the hole counting-from-the-right closes.
func TestRealIPIgnoresPositionlessHeaders(t *testing.T) {
	var seen string
	h := RealIP(1)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.RemoteAddr
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:4000"
	req.Header.Set("X-Real-IP", "198.51.100.9")
	req.Header.Set("True-Client-IP", "198.51.100.10")
	h.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, "10.0.0.1:4000", seen)
}

func TestRealIPClampsNonsensicalHopCounts(t *testing.T) {
	var seen string
	h := RealIP(0)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.RemoteAddr
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:4000"
	req.Header.Set("X-Forwarded-For", "198.51.100.9, 203.0.113.7")
	h.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, "203.0.113.7:4000", seen, "0 hops must behave as 1, never as \"take the leftmost\"")
}
