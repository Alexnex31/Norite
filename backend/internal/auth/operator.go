package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
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
// The CLI builds it from the key it read off disk. That makes the issuing half of this file the only code
// in this package the server itself never calls — it exists here anyway so the claim shape has exactly one
// definition, for the reason daemon/credentials owns the stored-credential format (M7): two
// implementations of one wire format drift, and the failure is a bootstrap that reports a bad signature
// against a token that is perfectly well formed.

// operatorTokenType is the `typ` claim that separates an operator token from every other JWT this package
// signs. The same mechanism keeps an access token from being spendable as an OAuth signup.
const operatorTokenType = "operator"

// OperatorTokenTTL is how long a minted operator token stays valid.
//
// Two minutes, which is far shorter than any other credential here and is sized for its actual use: the
// CLI mints one, makes one request, and discards it. Nothing holds one, nothing stores one, and nothing
// refreshes one. The short life matters because this is the only credential in the system that is not
// revocable — there is no row to delete, so the only thing that ends it is the clock.
const OperatorTokenTTL = 2 * time.Minute

// ErrNotOperator is the one refusal every way of failing the operator check produces.
//
// Undifferentiated for the reason TokenIssuer.Verify's is: telling a caller their signature was good but
// their token had expired confirms they hold a genuine signing key.
var ErrNotOperator = errors.New("this operation requires the instance operator or an instance administrator")

// operatorClaims is what an operator token carries, which is as close to nothing as a JWT gets.
//
// No subject: there is no account. No scopes: the authority is not delegable, so there is nothing to
// narrow it to. What the signature proves is the whole message — that whoever produced this could read
// the instance's signing key — and any further claim would be a fact the server can check for itself.
type operatorClaims struct {
	jwt.RegisteredClaims

	TokenType string `json:"typ"`
}

// IssueOperatorToken mints a token proving possession of the instance signing key.
//
// Called by the `norite` CLI, never by the server. See this file's header for why it lives here anyway.
func (t *TokenIssuer) IssueOperatorToken() (string, error) {
	now := t.now()
	claims := operatorClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(OperatorTokenTTL)),
			ID:        newJTI(),
		},
		TokenType: operatorTokenType,
	}

	signed, err := t.sign(claims)
	if err != nil {
		return "", err
	}
	return signed, nil
}

// VerifyOperatorToken reports whether raw is a live operator token issued for this instance.
func (t *TokenIssuer) VerifyOperatorToken(raw string) error {
	var claims operatorClaims

	_, err := jwt.ParseWithClaims(raw, &claims, t.keyFunc,
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(tokenIssuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(t.now),
	)
	if err != nil {
		return ErrNotOperator
	}

	// The claim that does the work. Without it every access token on the instance is also an operator
	// token, since both are signed with the same key and the registered claims alone would validate.
	if claims.TokenType != operatorTokenType {
		return ErrNotOperator
	}
	// An operator names no account, and a token that does is not one this package minted. Refused rather
	// than ignored: a subject here would mean somebody built a token from a different template, and
	// guessing which parts of it to honor is how a confused deputy starts.
	if claims.Subject != "" {
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
