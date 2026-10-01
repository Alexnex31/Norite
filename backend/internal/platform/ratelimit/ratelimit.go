// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ratelimit provides the backend's base REST/gateway rate-limiting middleware.
//
// The one rule this package exists to guarantee is global, not per-feature (docs/architecture.md §11
// "Rate limiting", §14.18): **all IP-based limiting groups IPv6 traffic by /64 subnet, never by exact
// address.** A single bad actor is routinely handed an entire /64 by their ISP or VPS provider, so
// per-address counting would let IPv6 traffic walk around every limit in the system for free — login
// attempts, matchmaking joins, webhook posts, all of it. Every limiter anywhere in the codebase must be
// built through this package so that property holds by construction rather than by reviewer vigilance.
//
// The store is in-memory by default, which is correct for the self-hosted single-process deployment shape.
// The flagship swaps in ulule/limiter's Redis-backed store (docs/architecture.md §12, see Backend) so
// replica count can't multiply an intended limit; that swap changes the store only, never the key function
// below.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/ulule/limiter/v3"
	"github.com/ulule/limiter/v3/drivers/store/memory"
	sredis "github.com/ulule/limiter/v3/drivers/store/redis"

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

// Options configures a limiter.
type Options struct {
	// Rate is a ulule/limiter formatted rate, "<limit>-<period>" with period one of S, M, H, D.
	Rate string
	// Bucket namespaces this limiter's counters. Separate buckets count independently, which is how
	// stricter per-route limits (e.g. /auth/* from Milestone M4) coexist with the base limit.
	Bucket string
	// Backend is where the counters live. The zero value is the in-memory store.
	Backend Backend
}

// Backend is the counter store every limiter on an instance shares: memory for a single process, Redis so
// that a limit counts across the flagship's replicas rather than once per pod (M114).
//
// One value chosen at startup and handed to every limiter, rather than a store picked per call site, so no
// bucket can end up counting per process on a deployment where every other one counts globally. That would
// silently multiply its limit by the replica count, which is the failure M114's done-when names.
type Backend struct {
	redis *redis.Client
}

// MemoryBackend is the single-process store. It is also the zero Backend.
func MemoryBackend() Backend { return Backend{} }

// NewRedisBackend connects to a Redis (or Valkey) server and verifies the connection, for the reason
// database.New does. The URL may carry a password and is never echoed.
func NewRedisBackend(ctx context.Context, url string, connectTimeout time.Duration) (Backend, error) {
	parsed, err := redis.ParseURL(url)
	if err != nil {
		return Backend{}, errors.New("ratelimit: could not parse the configured Redis URL")
	}
	client := redis.NewClient(parsed)
	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return Backend{}, fmt.Errorf("ratelimit: could not reach Redis within %s: %w", connectTimeout, err)
	}
	return Backend{redis: client}, nil
}

// Close releases the backend's connection, if it has one.
func (b Backend) Close() error {
	if b.redis == nil {
		return nil
	}
	return b.redis.Close()
}

func (b Backend) store(bucket string) (limiter.Store, error) {
	opts := limiter.StoreOptions{
		Prefix:          "norite:ratelimit:" + bucket,
		CleanUpInterval: limiter.DefaultCleanUpInterval,
	}
	if b.redis == nil {
		return memory.NewStoreWithOptions(opts), nil
	}
	store, err := sredis.NewStoreWithOptions(b.redis, opts)
	if err != nil {
		return nil, fmt.Errorf("ratelimit: preparing the Redis store for bucket %q: %w", bucket, err)
	}
	return store, nil
}

// Limiter counts attempts against one rate, keyed by whatever the caller counts: an address from
// ClientKey for HTTP, an account or a connection for the gateway.
type Limiter struct {
	instance *limiter.Limiter
}

// Result is one counted attempt.
type Result struct {
	Allowed   bool
	Limit     int64
	Remaining int64
	// Reset is the Unix second the current window ends.
	Reset int64
}

// New builds a limiter.
func New(opts Options) (*Limiter, error) {
	rate, err := limiter.NewRateFromFormatted(opts.Rate)
	if err != nil {
		return nil, fmt.Errorf("ratelimit: invalid rate %q (want \"<limit>-<S|M|H|D>\", e.g. \"600-M\"): %w", opts.Rate, err)
	}

	bucket := opts.Bucket
	if bucket == "" {
		bucket = "base"
	}
	store, err := opts.Backend.store(bucket)
	if err != nil {
		return nil, err
	}

	return &Limiter{instance: limiter.New(store, rate,
		limiter.WithIPv4Mask(maskOptions.IPv4Mask),
		limiter.WithIPv6Mask(maskOptions.IPv6Mask),
		limiter.WithTrustForwardHeader(maskOptions.TrustForwardHeader),
	)}, nil
}

// Allow counts one attempt for key.
//
// An error means the store could not answer, and the caller decides what that means; see onStoreFailure
// for why HTTP lets the request through. A key derived from an address must come from ClientKey, which is
// the only place the /64 rule is applied.
func (l *Limiter) Allow(ctx context.Context, key string) (Result, error) {
	res, err := l.instance.Get(ctx, key)
	if err != nil {
		return Result{}, err
	}
	return Result{Allowed: !res.Reached, Limit: res.Limit, Remaining: res.Remaining, Reset: res.Reset}, nil
}

// Middleware builds rate-limiting middleware over opts.Backend, keyed by ClientKey.
//
// This is a thin handler around limiter.Limiter rather than ulule/limiter's own stdlib middleware driver,
// for one reason: that driver's error hook cannot resume the chain, so a store failure there means the
// request is answered with an empty body no matter what the hook does. Owning ~20 lines here buys correct
// fail-open behavior (see onStoreFailure) and exact control of the response envelope and headers.
func Middleware(opts Options) (func(http.Handler) http.Handler, error) {
	lim, err := New(opts)
	if err != nil {
		return nil, err
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			result, err := lim.Allow(r.Context(), ClientKey(r))
			if err != nil {
				onStoreFailure(r, err)
				next.ServeHTTP(w, r)
				return
			}

			h := w.Header()
			h.Set("X-RateLimit-Limit", strconv.FormatInt(result.Limit, 10))
			h.Set("X-RateLimit-Remaining", strconv.FormatInt(result.Remaining, 10))
			h.Set("X-RateLimit-Reset", strconv.FormatInt(result.Reset, 10))

			if !result.Allowed {
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
