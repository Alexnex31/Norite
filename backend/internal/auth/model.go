package auth

import (
	"context"
	"slices"
	"time"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// Scope is a named capability an API token can be granted.
//
// Scopes exist only to *restrict* an API token below what its owner can already do. They are never a
// privilege grant: holding `messages:send` does not let a token post in a channel its owner cannot post in,
// because permission resolution still runs on top (CLAUDE.md rule 1). The scope check is a ceiling, and the
// permission check is the floor.
type Scope string

// The scope vocabulary as of M4. It grows with the surface it guards — each milestone that adds an
// endpoint an API token should be able to reach adds the scope for it here, rather than inventing scope
// strings at the call site where a typo would silently widen access.
const (
	// ScopeIdentify reads the actor's own account. The narrowest useful scope, and the one a bot needs to
	// confirm which account it is running as.
	ScopeIdentify Scope = "identify"

	// ScopeTokensRead lists the account's API tokens (names, scopes and timestamps — never a token value,
	// which exists only in the response that created it).
	ScopeTokensRead Scope = "tokens:read"
)

// There is deliberately no `tokens:write` scope, and minting an API token requires a *user* actor rather
// than any scope at all. A delegated credential that can mint credentials can mint one with more scopes
// than itself, which would make every restriction in this file decorative. Closing that by construction is
// worth more than the convenience of a bot rotating its own token.

// AllScopes is every scope a token may be granted, used to validate a mint request.
//
// An unknown scope is rejected rather than ignored: silently dropping a scope the caller asked for would
// hand them a token they believe is more capable than it is, and silently *keeping* one this build does not
// understand would mean a future release could widen an existing token's reach.
var AllScopes = []Scope{ScopeIdentify, ScopeTokensRead}

// ValidScope reports whether s is a scope this build understands.
func ValidScope(s Scope) bool { return slices.Contains(AllScopes, s) }

// ActorKind distinguishes how a request authenticated.
type ActorKind string

const (
	// ActorUser is a logged-in human's access token. It carries no scope restriction — a person using the
	// CLI or GUI can do whatever their permissions allow.
	ActorUser ActorKind = "user"
	// ActorAPIToken is a scoped credential held by a bot or a local automation script. Its reach is
	// bounded by Scopes below, and it is deliberately lower-trust (ADR 0017).
	ActorAPIToken ActorKind = "api_token"
)

// Actor is the authenticated identity behind a request.
//
// Every protected handler reads this instead of re-parsing the Authorization header, so "who is calling"
// is decided once, in the middleware, and cannot drift between endpoints.
type Actor struct {
	Kind   ActorKind
	UserID snowflake.ID

	// SessionID is set for ActorUser: the refresh session the access token was issued from.
	SessionID snowflake.ID
	// TokenID is set for ActorAPIToken: the api_tokens row that authenticated.
	TokenID snowflake.ID

	// Scopes is the granted set for an API token. Nil for a user actor, which HasScope treats as
	// unrestricted — see its comment for why that is safe rather than a hole.
	Scopes []Scope
}

// HasScope reports whether the actor may exercise the named capability.
//
// A user actor always may. That is not a bypass: a user's access token represents the person themselves,
// and scopes exist to restrict *delegated* credentials below their owner's reach. The alternative —
// granting every scope explicitly to user tokens — would mean a new scope silently locking human users out
// of a feature until someone remembered to add it to the implicit set.
func (a Actor) HasScope(want Scope) bool {
	if a.Kind == ActorUser {
		return true
	}
	return slices.Contains(a.Scopes, want)
}

// IsZero reports whether this is the empty actor, i.e. an unauthenticated request.
func (a Actor) IsZero() bool { return a.UserID == 0 }

// actorContextKey is unexported so nothing outside this package can inject an actor into a context and
// forge an identity that never passed the middleware.
type actorContextKey struct{}

// WithActor returns a context carrying the authenticated actor.
func WithActor(ctx context.Context, actor Actor) context.Context {
	return context.WithValue(ctx, actorContextKey{}, actor)
}

// ActorFrom returns the authenticated actor, and whether the request was authenticated at all.
func ActorFrom(ctx context.Context) (Actor, bool) {
	actor, ok := ctx.Value(actorContextKey{}).(Actor)
	return actor, ok && !actor.IsZero()
}

// TokenPair is what a successful login or refresh returns.
type TokenPair struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	TokenType    string
}
