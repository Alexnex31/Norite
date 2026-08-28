package main

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/logging"
)

// healthCheckTimeout bounds the database round trip a readiness check makes. Short on purpose: a probe
// that hangs is a probe that tells the orchestrator nothing.
const healthCheckTimeout = 2 * time.Second

// healthCacheTTL is how long a database probe result is reused.
//
// This endpoint is unauthenticated and deliberately exempt from rate limiting (probes must never throttle
// themselves), so without a cache every request would be a free database round trip from anyone who can
// reach the port — and the pool is intentionally small (docs/architecture.md §11), so a flood could
// occupy all of it and starve real traffic. Caching decouples request rate from query rate while keeping
// the answer fresh enough for any realistic probe interval, which is seconds at best.
const healthCacheTTL = time.Second

// lifecycle is the instance's coarse serving state.
type lifecycle int32

const (
	// lifecycleStarting is the initial state: the process is up but startup, migrations included, has not
	// finished.
	lifecycleStarting lifecycle = iota
	// lifecycleReady means the instance is serving.
	lifecycleReady
	// lifecycleStopping means shutdown has begun and in-flight requests are draining.
	lifecycleStopping
)

// pinger is the slice of the generated query layer the health endpoint needs. Narrow by design — it keeps
// the endpoint testable without a database and documents exactly what "healthy" is measured against.
type pinger interface {
	Ping(ctx context.Context) (int32, error)
}

// health tracks whether this instance is ready to serve and answers the readiness endpoint.
//
// Readiness is a latch, not a computation: it flips on once startup (migrations included) completes, and
// moves to stopping when shutdown begins. Until it is ready /healthz answers 503, which is what keeps the
// "never serves against a not-yet-migrated schema" guarantee true (docs/architecture.md §2,
// "Cross-cutting").
//
// That sentence used to end "for anything that routes on health", and the qualifier was load-bearing: this
// latch was consulted by /healthz and by nothing else, so on the two production paths architecture.md names
// — a systemd unit and docker-compose, neither of which gates traffic on a probe — every other route served
// straight through the migration window. refuseWhileStarting now reads Starting on every request, which is
// what removed the qualifier rather than merely restating it.
type health struct {
	queries pinger
	state   atomic.Int32

	// mu guards the cached probe result below.
	mu          sync.Mutex
	cachedErr   error
	cachedUntil time.Time
	// now is overridable so the cache can be tested without sleeping.
	now func() time.Time
}

func newHealth(queries pinger) *health {
	return &health{queries: queries, now: time.Now}
}

// MarkReady declares startup complete. Called once migrations have finished.
func (h *health) MarkReady() { h.state.Store(int32(lifecycleReady)) }

// Starting reports whether startup, migrations included, has not finished.
//
// Read by refuseWhileStarting, which is what turns the readiness latch from something only a probe
// consults into something every route obeys. Deliberately not true during shutdown: draining in-flight
// requests is the point of that state, and refusing them would defeat it.
func (h *health) Starting() bool { return lifecycle(h.state.Load()) == lifecycleStarting }

// MarkStopping withdraws readiness at the start of shutdown, so new work stops arriving while in-flight
// requests drain.
func (h *health) MarkStopping() { h.state.Store(int32(lifecycleStopping)) }

// healthResponse is the /healthz body.
type healthResponse struct {
	// Status is "ok", "starting", "stopping", or "degraded".
	Status string `json:"status"`
	// Detail explains a non-ok status in terms an operator can act on. Never carries driver or
	// connection-string detail — this endpoint is reachable without authentication.
	Detail string `json:"detail,omitempty"`
}

// Handler serves GET and HEAD /healthz.
//
// It is a readiness check, not a liveness check: it reports 200 only when this instance can actually do
// its job, which includes having migrated the schema and being able to execute a statement against
// Postgres recently. A bare "the process is up" answer would let an orchestrator route traffic to an
// instance that cannot serve a single request.
//
// The database round trip goes through the sqlc-generated query layer rather than a driver-level ping, so
// what is verified is the same path real requests take: pool checkout, generated call, Postgres, back.
func (h *health) Handler(w http.ResponseWriter, r *http.Request) {
	switch lifecycle(h.state.Load()) {
	case lifecycleStarting:
		httpx.WriteJSON(w, r, http.StatusServiceUnavailable, healthResponse{
			Status: "starting",
			Detail: "instance is still starting up (migrations may be in progress)",
		})
		return
	case lifecycleStopping:
		// Distinct from "starting" on purpose: an operator watching a rolling restart needs to see that
		// this instance is draining, not that it is booting and possibly mid-migration.
		httpx.WriteJSON(w, r, http.StatusServiceUnavailable, healthResponse{
			Status: "stopping",
			Detail: "instance is shutting down and draining in-flight requests",
		})
		return
	}

	if err := h.probe(r.Context()); err != nil {
		// Logged in full server-side; the client gets a generic reason. An unauthenticated endpoint
		// must not disclose hostnames or driver internals.
		logging.FromContext(r.Context()).Error().Err(err).Msg("health check: database unreachable")
		httpx.WriteJSON(w, r, http.StatusServiceUnavailable, healthResponse{
			Status: "degraded",
			Detail: "database is unreachable",
		})
		return
	}

	httpx.WriteJSON(w, r, http.StatusOK, healthResponse{Status: "ok"})
}

// probe returns the database's reachability, reusing a recent result for healthCacheTTL.
func (h *health) probe(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Holding the lock across the query serializes concurrent probes deliberately: a burst should produce
	// one database round trip, not one per caller. The query is already bounded by healthCheckTimeout, so
	// the wait is bounded too.
	if h.now().Before(h.cachedUntil) {
		return h.cachedErr
	}

	queryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), healthCheckTimeout)
	defer cancel()

	_, err := h.queries.Ping(queryCtx)

	h.cachedErr = err
	h.cachedUntil = h.now().Add(healthCacheTTL)
	return err
}

// compile-time check that the generated Queries type satisfies the narrow interface above.
var _ pinger = (*db.Queries)(nil)
