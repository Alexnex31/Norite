package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/operatortoken"
)

// The operator: whoever holds this instance's configuration file.
//
// # Why a third authority exists at all
//
// Two already do. An access token says "I am this account"; an API token says "I act for this account,
// within these scopes". Neither can create the *first* account, because both are things an account issues,
// and a fresh instance has none — which is the chicken-and-egg M2 left behind when it built the setup
// wizard before `users` existed.
//
// The operator is the answer, and it is not a new secret: it is the instance's own HS256 signing key,
// which lives in instance.toml beside the database password. Anyone who can read that file can already
// forge an access token for any account on the instance, so treating possession of it as the highest
// authority here concedes nothing that was not already conceded. What it buys is that the authority is
// *stated* — a bootstrap request proves filesystem access rather than being trusted because it arrived
// early, so there is no window in which whoever reaches a newly-migrated instance first becomes its admin.
//
// docs/architecture.md's rule 16 asks every local trust tier to say which it is. This one is
// filesystem-permission-protected, the same tier as the config file itself, and deliberately not the same
// as either token tier above it.
//
// # Why the operator is not an Actor
//
// Actor answers "which account is calling", and an operator is not an account — there is nobody to name.
// Adding an ActorKind for it would give every existing reader of ActorFrom a case it does not handle:
// HasScope would have to decide what an accountless actor may do, and Actor.IsZero would answer true for a
// UserID of 0, which is precisely the value an operator would carry. So it travels on its own context key
// and nothing that resolves accounts can see it.
//
// # Why it is minted by the client and not handed out by the server
//
// There is no endpoint to ask for one, because asking would require authenticating, which is the problem.
// The CLI builds it from the key it read off disk.
//
// That is why the *format* is not in this package: Go's internal/ rule makes backend/internal unreachable
// from the CLI module, so the claims, the type, the lifetime and the algorithm pin live in
// backend/operatortoken, which both sides import. What stays here is the authorization decision — which
// requests such a token may authorize — because that belongs where the routes are.

// OperatorTokenTTL is how long a minted operator token stays valid. See operatortoken.TTL.
const OperatorTokenTTL = operatortoken.TTL

// ErrNotOperator is the one refusal every way of failing the operator check produces.
//
// Undifferentiated for the reason TokenIssuer.Verify's is: telling a caller their signature was good but
// their token had expired confirms they hold a genuine signing key.
var ErrNotOperator = errors.New("this operation requires the instance operator or an instance administrator")

// IssueOperatorToken mints a token proving possession of the instance signing key.
//
// Present on this type for symmetry with Issue, and used by the tests here; the `norite` CLI calls
// operatortoken.Issue directly, since it has a key and no Service.
func (t *TokenIssuer) IssueOperatorToken() (string, error) {
	return operatortoken.Issue(t.key, t.now())
}

// VerifyOperatorToken reports whether raw is a live operator token issued for this instance.
//
// The format — claims, type, lifetime, algorithm pin — belongs to operatortoken, which the CLI implements
// the other half of. What stays here is which requests such a token may authorize, below.
func (t *TokenIssuer) VerifyOperatorToken(raw string) error {
	if err := operatortoken.Verify(t.key, raw, t.now()); err != nil {
		return ErrNotOperator
	}
	return nil
}

// operatorContextKey marks a request as carrying operator authority. Unexported, so nothing outside this
// package can put one on a context and manufacture the authority it stands for.
type operatorContextKey struct{}

// WithOperator marks a context as carrying operator authority.
func WithOperator(ctx context.Context) context.Context {
	return context.WithValue(ctx, operatorContextKey{}, true)
}

// IsOperator reports whether the request authenticated as the instance operator rather than as an account.
//
// Handlers need this where the two callers differ — an audit line naming a user, or a column that takes a
// granting account. Everything else treats the two the same, which is the point of resolving them in one
// middleware.
func IsOperator(ctx context.Context) bool {
	v, _ := ctx.Value(operatorContextKey{}).(bool)
	return v
}

// AuthenticateInstanceAdmin admits the instance operator, or an account holding the Instance Admin tier.
//
// Mounted on /instance, *outside* the group that runs Authenticate. That separation is deliberate: the
// ordinary Bearer path routes a credential by prefix to one of two verifiers, and an operator token must
// never be one of the things it can land on. Keeping the two chains apart means the question "can an
// operator token authenticate an ordinary request" is answered by the router rather than by a `typ` check
// remembering to be there — the same reason nrp_ is absent from LooksLikeOpaqueToken.
//
// Unlike Authenticate, this rejects on its own. There is no public route under /instance and there is not
// going to be one, so the exemption-list argument that keeps Authenticate permissive does not apply.
func AuthenticateInstanceAdmin(svc *Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Fail closed on a service-less router rather than leaving the caller to guard the mount.
			//
			// This is not hypothetical tidiness: the contract test builds a router with a nil AuthSvc to
			// walk the route table without a database, and while the /instance mount was conditional on
			// that field those routes did not exist there at all — so the check that every served route
			// appears in openapi.yaml (rule 6) could not see them. Making the middleware handle nil lets
			// the routes mount unconditionally, which is what puts them back under that check.
			if svc == nil {
				unauthorizedInstance(w, r)
				return
			}

			raw, ok := bearerCredential(r)
			if !ok {
				unauthorizedInstance(w, r)
				return
			}

			// The operator first, and cheaply: it is a signature check with no database round trip, and it
			// is the only path that works on an instance with no accounts at all.
			if err := svc.issuer.VerifyOperatorToken(raw); err == nil {
				next.ServeHTTP(w, r.WithContext(WithOperator(r.Context())))
				return
			}

			// Otherwise an ordinary account, which must hold the tier. An API token is refused here by
			// construction rather than by a kind check: this branch only accepts a JWT, and a scoped
			// credential minted by an admin is not the admin. Instance administration is not delegable,
			// for the reason token management is not (docs/architecture.md, M4's auth notes).
			if LooksLikeOpaqueToken(raw) {
				unauthorizedInstance(w, r)
				return
			}
			actor, err := svc.AuthenticateAccessToken(r.Context(), raw)
			if err != nil {
				unauthorizedInstance(w, r)
				return
			}

			admin, err := svc.queries.IsInstanceAdmin(r.Context(), int64(actor.UserID))
			if err != nil {
				// Passed through rather than wrapped in a sentinel: WriteError logs a 5xx itself and
				// returns no detail to the client, which is exactly the treatment a failed tier lookup
				// wants. Failing closed matters more here than the message does.
				httpx.WriteError(w, r, fmt.Errorf("resolving the instance admin tier: %w", err))
				return
			}
			if !admin {
				// 403 rather than 401: the credential is genuine and the caller is authenticated, so
				// re-authenticating would not help and would send a client into a refresh loop.
				httpx.WriteError(w, r, httpx.Errorf(httpx.ErrForbidden, "%s", ErrNotOperator.Error()))
				return
			}

			next.ServeHTTP(w, r.WithContext(WithActor(r.Context(), actor)))
		})
	}
}

// unauthorizedInstance is the single 401 every way of failing to present a usable credential produces.
func unauthorizedInstance(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="norite"`)
	httpx.WriteError(w, r, httpx.Errorf(httpx.ErrUnauthorized, "%s", ErrNotOperator.Error()))
}
