// Package ratelimit provides the backend's base REST/gateway rate-limiting middleware.
//
// The one rule this package exists to guarantee is global, not per-feature (docs/architecture.md §11
// "Rate limiting", §14.18): **all IP-based limiting groups IPv6 traffic by /64 subnet, never by exact
// address.** A single bad actor is routinely handed an entire /64 by their ISP or VPS provider, so
// per-address counting would let IPv6 traffic walk around every limit in the system for free — login
// attempts, matchmaking joins, webhook posts, all of it. Every limiter anywhere in the codebase must be
// built through this package so that property holds by construction rather than by reviewer vigilance.
//
// The store is in-memory here, which is correct for the self-hosted single-process deployment shape. The
// flagship swaps in ulule/limiter's Redis-backed store (docs/architecture.md §12) so replica count can't
// multiply an intended limit; that swap changes the Store only, never the key function below.
package ratelimit

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/ulule/limiter/v3"
	"github.com/ulule/limiter/v3/drivers/store/memory"

	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/logging"
)

// ipv6GroupBits is the prefix length IPv6 clients are grouped by. See the package comment — this is a
// global invariant, not a tunable.
const ipv6GroupBits = 64

// maskOptions are the address-masking rules every key derivation in this package uses: exact address for
// IPv4, /64 prefix for IPv6.
//
// TrustForwardHeader stays false deliberately. Deciding who the client is happens once, upstream, in the
// router's conditional httpx.RealIP middleware, which normalizes r.RemoteAddr; letting this library
// independently re-read X-Forwarded-For would mean two places could disagree about the client's identity,
// and the looser of the two would win.
var maskOptions = limiter.Options{
	IPv4Mask:           net.CIDRMask(32, 32),
	IPv6Mask:           net.CIDRMask(ipv6GroupBits, 128),
	TrustForwardHeader: false,
}

// Options configures Middleware.
type Options struct {
	// Rate is a ulule/limiter formatted rate, "<limit>-<period>" with period one of S, M, H, D.
	Rate string
	// Bucket namespaces this limiter's counters. Separate buckets count independently, which is how
	// stricter per-route limits (e.g. /auth/* from Milestone M4) coexist with the base limit.
	Bucket string
}

// Middleware builds rate-limiting middleware over an in-memory store.
//
// This is a thin handler around limiter.Limiter rather than ulule/limiter's own stdlib middleware driver,
// for one reason: that driver's error hook cannot resume the chain, so a store failure there means the
// request is answered with an empty body no matter what the hook does. Owning ~20 lines here buys correct
// fail-open behavior (see onStoreFailure) and exact control of the response envelope and headers.
func Middleware(opts Options) (func(http.Handler) http.Handler, error) {
	rate, err := limiter.NewRateFromFormatted(opts.Rate)
	if err != nil {
		return nil, fmt.Errorf("ratelimit: invalid rate %q (want \"<limit>-<S|M|H|D>\", e.g. \"600-M\"): %w", opts.Rate, err)
	}

	bucket := opts.Bucket
	if bucket == "" {
		bucket = "base"
	}

	store := memory.NewStoreWithOptions(limiter.StoreOptions{
		Prefix:          "norite:ratelimit:" + bucket,
		CleanUpInterval: limiter.DefaultCleanUpInterval,
	})

	instance := limiter.New(store, rate,
		limiter.WithIPv4Mask(maskOptions.IPv4Mask),
		limiter.WithIPv6Mask(maskOptions.IPv6Mask),
		limiter.WithTrustForwardHeader(maskOptions.TrustForwardHeader),
	)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			result, err := instance.Get(r.Context(), ClientKey(r))
			if err != nil {
				onStoreFailure(r, err)
				next.ServeHTTP(w, r)
				return
			}

			h := w.Header()
			h.Set("X-RateLimit-Limit", strconv.FormatInt(result.Limit, 10))
			h.Set("X-RateLimit-Remaining", strconv.FormatInt(result.Remaining, 10))
			h.Set("X-RateLimit-Reset", strconv.FormatInt(result.Reset, 10))

			if result.Reached {
				// Retry-After is the header well-behaved HTTP clients — and the CLI's own backoff
				// logic — actually read, so it must always be present on a 429. Rounding up, with a
				// floor of one second: truncation would drop the header entirely for the whole final
				// second of a window, i.e. precisely on the retries about to succeed, and a literal
				// "0" would invite an immediate retry that is still refused.
				h.Set("Retry-After", strconv.Itoa(retryAfterSeconds(result.Reset, time.Now())))
				httpx.WriteError(w, r, httpx.ErrRateLimited)
				return
			}

			next.ServeHTTP(w, r)
		})
	}, nil
}

// ClientKey derives the rate-limiting key for a request: the exact address for IPv4 clients, the /64
// prefix for IPv6 clients.
//
// It reads r.RemoteAddr only. Whether that value came straight off the socket or was rewritten from a
// proxy header is the router's decision, made once for the whole chain.
func ClientKey(r *http.Request) string {
	ip := limiter.GetIPWithMask(r, maskOptions)
	if ip == nil {
		// An unparseable RemoteAddr should never happen behind net/http, but if it does, collapsing
		// every such request into one shared bucket is the safe failure: it throttles rather than
		// exempts.
		return "unknown"
	}
	if ip.To4() != nil {
		return ip.String()
	}
	return ip.String() + "/" + strconv.Itoa(ipv6GroupBits)
}

// retryAfterSeconds renders the Retry-After value for a window resetting at the given Unix second.
func retryAfterSeconds(resetUnix int64, now time.Time) int {
	remaining := time.Unix(resetUnix, 0).Sub(now)
	seconds := int(math.Ceil(remaining.Seconds()))
	if seconds < 1 {
		return 1
	}
	return seconds
}

// onStoreFailure records a rate-limit store error; the caller then lets the request through unthrottled.
//
// Failing open is the deliberate choice. The in-memory store used by self-hosted instances cannot fail,
// so this path only becomes reachable once the flagship swaps in the Redis-backed store — and there,
// turning a Redis blip into a full API outage would be a worse failure than briefly serving unthrottled.
// Operationally this log line is alert-worthy: it means limits are not being enforced.
func onStoreFailure(r *http.Request, err error) {
	logging.FromContext(r.Context()).Error().Err(err).
		Msg("rate limit store unavailable — request allowed unthrottled")
}
