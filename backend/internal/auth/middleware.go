package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
)

// bearerPrefix is the only Authorization scheme this API accepts.
const bearerPrefix = "Bearer "

// Authenticate resolves the Authorization header to an Actor and puts it on the request context.
//
// It slots into the chain below RateLimit (docs/architecture.md §2), so an unauthenticated flood is
// throttled before it reaches any credential verification — argon2id is not on this path, but an API-token
// lookup is still a database round trip.
//
// # Why this does not reject on its own
//
// A missing or bad credential is *not* rejected here. The middleware records who the caller is, if anyone,
// and RequireAuth decides whether a given route needs one. Rejecting centrally would mean every public
// route (healthz, login, register) needed an exemption list, and an exemption list is a thing people add to
// by accident.
func Authenticate(svc *Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerCredential(r)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}

			// Route to the right verifier by prefix rather than by trying each in turn: an opaque token is
			// not a JWT and attempting to parse it as one on every bot request would be wasted work.
			var (
				actor Actor
				err   error
			)
			if LooksLikeOpaqueToken(raw) {
				actor, err = svc.AuthenticateAPIToken(r.Context(), raw)
			} else {
				actor, err = svc.AuthenticateAccessToken(r.Context(), raw)
			}
			if err != nil {
				// An invalid credential is treated as no credential. The route's own RequireAuth then
				// produces the 401, so a protected route still refuses while a public one still works —
				// and no handler has to distinguish "absent" from "rejected".
				next.ServeHTTP(w, r)
				return
			}

			next.ServeHTTP(w, r.WithContext(WithActor(r.Context(), actor)))
		})
	}
}

// RequireAuth rejects a request that carries no valid credential.
//
// Mounted per-route or per-group rather than globally, so that adding a protected route is an explicit act
// and forgetting to protect one is visible in the router rather than hidden in a list of exemptions.
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := ActorFrom(r.Context()); !ok {
			// WWW-Authenticate is what tells a well-behaved client *how* to authenticate, and RFC 9110
			// requires it on a 401. Omitting it is the difference between a client that knows to retry
			// with a token and one that just sees a failure.
			w.Header().Set("WWW-Authenticate", `Bearer realm="norite"`)
			httpx.WriteError(w, r, httpx.Errorf(httpx.ErrUnauthorized, "authentication required"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireScope rejects a request whose actor lacks the named scope.
//
// Always mounted *after* RequireAuth. A user actor passes every scope check by design — scopes restrict
// delegated credentials, not people (see Actor.HasScope).
func RequireScope(scope Scope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor, ok := ActorFrom(r.Context())
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer realm="norite"`)
				httpx.WriteError(w, r, httpx.Errorf(httpx.ErrUnauthorized, "authentication required"))
				return
			}
			if !actor.HasScope(scope) {
				// 403, not 401: the credential is genuine and the caller is authenticated. Re-authenticating
				// would not help, and a 401 would send a client into a pointless refresh loop.
				httpx.WriteError(w, r, httpx.Errorf(httpx.ErrForbidden, "this token is missing the %q scope", scope))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireUserActor rejects a request authenticated with an API token rather than a user's own access token.
//
// Used for operations a delegated credential must never perform — minting further tokens above all, since
// that would let a token grant itself scopes it does not hold.
func RequireUserActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, ok := ActorFrom(r.Context())
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="norite"`)
			httpx.WriteError(w, r, httpx.Errorf(httpx.ErrUnauthorized, "authentication required"))
			return
		}
		if actor.Kind != ActorUser {
			httpx.WriteError(w, r, httpx.Errorf(httpx.ErrForbidden,
				"this operation requires a logged-in session, not an API token"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerCredential extracts the credential from the Authorization header.
//
// The scheme match is case-insensitive because RFC 9110 says scheme names are, and a client sending
// "bearer" lowercase is following the spec even if it is unusual.
func bearerCredential(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if len(header) < len(bearerPrefix) || !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	credential := strings.TrimSpace(header[len(bearerPrefix):])
	if credential == "" {
		return "", false
	}
	return credential, true
}

// isAuthFailure reports whether err means "the caller's credential was not good", as opposed to a server
// fault. Used by handlers to avoid logging a routine bad password at error level.
func isAuthFailure(err error) bool {
	return errors.Is(err, ErrInvalidCredentials) ||
		errors.Is(err, ErrInvalidToken) ||
		errors.Is(err, ErrInvalidRefreshToken) ||
		errors.Is(err, ErrSessionReuse)
}
