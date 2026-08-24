package main

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/config"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/logging"
	"github.com/Alexnex31/Norite/backend/internal/platform/ratelimit"
)

// apiBase is the versioned REST prefix every endpoint lives under (docs/architecture.md §2 "REST API").
const apiBase = "/api/v1"

// healthzPath is the readiness route's full path, needed as a literal because the rate limiter is mounted
// on the root chain and skips it by path before routing has happened.
const healthzPath = apiBase + "/healthz"

type routerOptions struct {
	Config  config.Config
	Logger  zerolog.Logger
	Health  *health
	Auth    *auth.Handler
	AuthSvc *auth.Service
}

// authRateLimit is the stricter bucket the unauthenticated auth routes sit behind.
//
// The base limit is sized for ordinary API traffic; login, registration and refresh are password- and
// token-guessing surfaces where that is far too generous. A separate Bucket rather than a lower global
// limit, so throttling a credential-stuffing run cannot also throttle a legitimate client's normal
// requests — the two are counted independently (see internal/platform/ratelimit).
const authRateLimit = "20-M"

// devicePollRateLimit is the bucket the device-code endpoints sit in.
//
// The auth bucket is the wrong shape here in a way that would break the flow rather than protect it: a
// client polling at the documented five-second interval spends twelve requests a minute, which on its own
// is most of that bucket, and two people signing in from behind one NAT would throttle each other off the
// instance. So it counts separately and is sized for what a well-behaved client actually does, with room
// for a handful of them at once.
//
// Still a ceiling rather than an exemption. A poll is one indexed lookup and, at most once, a session
// insert — cheap, but the endpoint is unauthenticated and takes a credential, so it gets a limit like
// everything else that does.
const devicePollRateLimit = "120-M"

// newRouter assembles the HTTP router and its middleware chain.
//
// The chain order is fixed by docs/architecture.md §2 and is load-bearing rather than stylistic:
//
//	SanitizeInboundRequestID → decides whether the client's own X-Request-Id may be adopted, before
//	             anything downstream treats it as trusted.
//	RequestID  → every later layer, and every log line, can reference the same correlation ID.
//	EchoRequestID → returns that ID to the client so it can quote it in a report.
//	RealIP     → conditional; see below.
//	Recoverer  → outside the logger so a panic in the logging middleware itself is still contained.
//	SecureHeaders → set before any handler can start writing a body.
//	StructuredLogger → attaches the request-scoped logger handlers read from the context.
//	RateLimit  → last, so a throttled request still gets a request ID, headers, and a log line.
//
// AuthenticateBearer slots in below RateLimit at Milestone M4; there is nothing to authenticate yet.
func newRouter(opts routerOptions) (http.Handler, error) {
	rateLimiter, err := ratelimit.Middleware(ratelimit.Options{
		Rate:   opts.Config.RateLimit,
		Bucket: "rest",
	})
	if err != nil {
		return nil, err
	}

	authLimiter, err := ratelimit.Middleware(ratelimit.Options{
		Rate:   authRateLimit,
		Bucket: "auth",
	})
	if err != nil {
		return nil, err
	}

	devicePollLimiter, err := ratelimit.Middleware(ratelimit.Options{
		Rate:   devicePollRateLimit,
		Bucket: "device-poll",
	})
	if err != nil {
		return nil, err
	}

	r := chi.NewRouter()

	// Above RequestID, because that middleware adopts an inbound X-Request-Id verbatim and the value ends
	// up in the response, the logs, and every error body. See httpx.SanitizeInboundRequestID.
	r.Use(httpx.SanitizeInboundRequestID(opts.Config.TrustProxyHeaders))
	r.Use(middleware.RequestID)
	r.Use(httpx.EchoRequestID)

	// RealIP rewrites r.RemoteAddr from X-Forwarded-For, and is only sound behind a proxy the operator
	// controls. Off by default: the client IP is the rate limiter's grouping key, so honoring a
	// client-settable header on a directly-exposed process would hand every caller a free new identity
	// per request and neutralize every IP-based limit in the system. This is where "who is the client"
	// is settled for the whole chain — no downstream middleware re-reads forwarded headers.
	//
	// httpx.RealIP rather than chi's: chi's is deprecated as spoofable because it trusts the leftmost,
	// client-supplied X-Forwarded-For entry. See its doc comment.
	if opts.Config.TrustProxyHeaders {
		r.Use(httpx.RealIP(opts.Config.TrustedProxyHops))
	}

	r.Use(httpx.Recoverer(opts.Logger))
	r.Use(httpx.SecureHeaders(opts.Config.TrustProxyHeaders))
	r.Use(logging.RequestLogger(opts.Logger))

	// Rate limiting is mounted on the root chain, not on the /api/v1 group, and skips only readiness.
	//
	// Group-mounted middleware runs only for routes registered inside that group, so unmatched paths fall
	// through to NotFound/MethodNotAllowed above it — unthrottled. That would leave an attacker free to
	// flood the instance with 404s at unlimited rate, each one burning the full middleware chain and a log
	// line, and the hole would persist after M4 mounts real routers.
	//
	// Readiness is exempt because probes are frequent and single-source; throttling them would let the
	// limiter manufacture the outage it exists to help survive.
	r.Use(skipPath(healthzPath, rateLimiter))

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, r, httpx.ErrNotFound)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		// RFC 9110 requires Allow on a 405. chi's built-in handler emits it, but overriding that handler
		// to get our JSON envelope loses it, and chi keeps the resolved method set unexported. Asking the
		// router itself which verbs match this path is the way to recover it without reimplementing
		// routing — and it stays correct once M4 adds routes with path parameters.
		if allowed := allowedMethods(r, req.URL.Path); allowed != "" {
			w.Header().Set("Allow", allowed)
		}
		httpx.WriteError(w, req, httpx.ErrMethodNotAllowed)
	})

	// The server-rendered password-reset pages, outside the versioned API prefix: a person opens these
	// from an email, they are not an API a client codegens against, and putting them under /api/v1 would
	// imply they move when that version does.
	//
	// httpx.HTMLPage overrides the JSON API's CSP for these two routes only. The global policy is
	// `default-src 'none'; form-action 'none'`, which would render the page and then silently forbid its
	// form from submitting anywhere — see that middleware for what it grants and what it still denies.
	if opts.Auth != nil {
		r.Group(func(r chi.Router) {
			// The same stricter bucket the /auth/* routes carry, and for the same reason: POST /reset is a
			// credential-changing endpoint, and it is one of only two ways to spend a reset token. Being
			// mounted at the root rather than inside /api/v1 put it outside that group by accident — the
			// base limit alone let it run at hundreds of attempts a minute.
			r.Use(authLimiter)
			r.Use(httpx.HTMLPage)
			opts.Auth.PageRoutes(r)
			// The device-code verification pages, in the same bucket for the same reason: /device is
			// where a user code is guessed at, and /device/signin takes a password. Both are exactly the
			// kind of endpoint that bucket exists for.
			opts.Auth.DevicePageRoutes(r)
		})
	}

	r.Route(apiBase, func(r chi.Router) {
		// Instance administration, mounted as a sibling of the group below rather than inside it.
		//
		// That placement is the point. Everything in the group below runs auth.Authenticate, which routes
		// a Bearer credential by prefix to one of two account verifiers; an operator token must never be
		// one of the things it can land on, because an operator names no account and Actor has nowhere to
		// put it. Mounting /instance outside that chain means "can an operator token authenticate an
		// ordinary request" is answered by the router — structurally, once — rather than by a `typ` check
		// in each verifier remembering to be there.
		//
		// AuthenticateInstanceAdmin rejects on its own, unlike Authenticate. There is no public route
		// under /instance and there is not going to be one, so the exemption-list argument that keeps
		// Authenticate permissive does not apply here.
		//
		// The stricter bucket, for the reason /auth carries it: bootstrap runs argon2id, and it is the one
		// endpoint on the instance whose success is irreversible.
		if opts.Auth != nil {
			r.Route("/instance", func(r chi.Router) {
				r.Use(authLimiter)
				// Mounted whether or not a service is present. AuthenticateInstanceAdmin refuses
				// everything when it has none, which keeps these routes visible to the contract test's
				// route walk — that test builds a router with a nil service, and a conditional mount hid
				// this whole group from rule 6's check.
				r.Use(auth.AuthenticateInstanceAdmin(opts.AuthSvc))
				opts.Auth.InstanceRoutes(r)
			})
		}

		// Everything below resolves an account credential, inside an explicit group.
		//
		// A group rather than a bare r.Use on this router, because /instance above is a sibling that must
		// not inherit it — and chi requires every Use to precede every route on the same mux, so a plain
		// Use here would panic now that a route is registered before it. The group scopes the middleware
		// to what it is actually for, which is what the sibling arrangement was asking for anyway.
		r.Group(func(r chi.Router) {
			// Authenticate resolves a Bearer credential into an actor for every request below this point.
			// It rejects nothing on its own — each route decides whether it needs one — so public and
			// protected routes can coexist without an exemption list (see auth.Authenticate).
			if opts.AuthSvc != nil {
				r.Use(auth.Authenticate(opts.AuthSvc))
			}

			if opts.Auth != nil {
				// The auth routes carry the stricter bucket *in addition to* the base limiter already applied
				// on the root chain, so a credential-guessing run is counted twice and stopped by whichever
				// ceiling it reaches first.
				r.Route("/auth", func(r chi.Router) {
					r.Use(authLimiter)
					opts.Auth.Routes(r)
					// OAuth sits in the same stricter bucket: /authorize mints a database row per call and
					// /exchange spends a credential, so both are worth counting separately from ordinary API
					// traffic.
					opts.Auth.OAuthRoutes(r)
				})
				// The device-code endpoints, in two buckets rather than one, because the two halves have
				// opposite shapes. Issuing mints a database row per call and happens once per sign-in, so it
				// carries the stricter bucket for exactly the reason /authorize does. Polling is repetitive by
				// design — twelve requests a minute at the documented interval — and the bucket that stops
				// repetition would throttle a well-behaved client off the instance, so it counts separately.
				r.Route("/auth/device", func(r chi.Router) {
					r.Group(func(r chi.Router) {
						r.Use(authLimiter)
						opts.Auth.DeviceIssueRoutes(r)
					})
					r.Group(func(r chi.Router) {
						r.Use(devicePollLimiter)
						opts.Auth.DevicePollRoutes(r)
					})
				})
				r.Route("/users", opts.Auth.UserRoutes)
			}

			r.Get("/healthz", opts.Health.Handler)
			// chi registers GET and HEAD separately — a GET-only route answers HEAD with 405. Health probes
			// (curl -I, several load balancers and CDNs) do use HEAD, so it gets the same handler; net/http
			// discards the body for HEAD on its own.
			r.Head("/healthz", opts.Health.Handler)

			// Domain routers mount here from Milestone M4 (auth) onward; the root rate limiter already
			// covers them.
		})
	})

	return r, nil
}

// skipPath applies mw to every request except those for exactly the given path.
func skipPath(path string, mw func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		wrapped := mw(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == path {
				next.ServeHTTP(w, r)
				return
			}
			wrapped.ServeHTTP(w, r)
		})
	}
}

// allowedMethods renders the Allow header value for a 405 by asking the router which verbs route to path.
func allowedMethods(mux *chi.Mux, path string) string {
	var allowed []string
	for _, method := range []string{
		http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions,
	} {
		if mux.Match(chi.NewRouteContext(), method, path) {
			allowed = append(allowed, method)
		}
	}
	return strings.Join(allowed, ", ")
}
