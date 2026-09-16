// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

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

	// ScopeGuildsRead reads guilds, their channels, their roles and their membership (M12).
	ScopeGuildsRead Scope = "guilds.read"

	// ScopeGuildsWrite creates, updates and deletes them.
	//
	// Separate from the read scope rather than one "guilds" scope, because the two have very different
	// blast radii and the common bot wants only the first: a status bot that lists channels should not be
	// one compromise away from deleting the guild. Write does *not* imply read — a scope bounds a
	// delegated credential and nothing about holding one justifies granting another, so a token that needs
	// both asks for both.
	//
	// Neither grants anything on its own. Permission resolution still runs on top (rule 1), so a token
	// holding ScopeGuildsWrite can do exactly what its owner could and no more; the scope only ever
	// narrows that. Adding these was the missing half — M12 shipped the first mutating surface a delegated
	// credential can reach, and reached it with no scope gate at all, so an `identify`-only token deleted
	// a guild. Reproduced before it was fixed.
	ScopeGuildsWrite Scope = "guilds.write"

	// ScopeGuildsAudit reads a guild's audit log (M14), and is separate from ScopeGuildsRead for the
	// reason ScopeGuildsRead is separate from ScopeGuildsWrite.
	//
	// The read scope's own comment enumerates what it covers — "guilds, their channels, their roles and
	// their membership" — and an audit log is none of those. It is not current state at all: it is
	// history and attribution, who kicked whom, which permission changed, what a nickname used to be, and
	// the names of channels the reader's own listing hides. A status bot that lists channels should not
	// be one compromise away from the guild's moderation history, which is the sentence already written
	// one constant up about deleting the guild.
	//
	// The permission layer already draws this line: PermViewAuditLog is its own bit, granted by default to
	// nobody and not implied by PermManageGuild. A scope that lumped the log in with the channel listing
	// would be the delegation model contradicting the permission model — and the two are meant to compose,
	// with the scope bounding a credential below whatever the permission already allows.
	//
	// Added while it was free. No token has ever been minted against a released build, so nothing is
	// broken by narrowing guilds.read today; doing it after any exist is a breaking change for every one
	// of them. Same argument M12 made for the permission wire format.
	ScopeGuildsAudit Scope = "guilds.audit"

	// Messages are their own pair, not folded into guilds.read/guilds.write, and the blast radii are why.
	// A bot granted guilds.write can rename the guild and delete channels; one that only needs to post an
	// announcement should be able to do that and nothing else. Conversely guilds.read enumerates a guild's
	// structure, which is metadata — message history is the conversation itself, and a credential that can
	// read every word said in a guild is a different thing to hand out.
	//
	// Write does not imply read, for the reason the guild pair does not: a scope bounds a credential, and
	// holding one is not an argument for being granted another. A webhook-shaped bot that posts needs
	// messages.write alone and never sees a backlog.
	ScopeMessagesRead  Scope = "messages.read"
	ScopeMessagesWrite Scope = "messages.write"
)

// Token management — minting, listing and revoking — has no scope at all: every one of those operations
// requires a *user* actor, so a delegated credential can never touch the account's credential inventory.
//
// Minting is the obvious case: a token that can mint tokens can mint one with scopes it does not itself
// hold, which would make every restriction here decorative. Listing is the same category for a quieter
// reason — it discloses the names, scopes and last-use times of an account's *other* tokens, which is
// reconnaissance for a compromised low-privilege bot: it cannot use a more powerful sibling, but it can
// learn one exists and what it can reach. There is no delegation use case worth that.

// AllScopes is every scope a token may be granted, used to validate a mint request.
//
// An unknown scope is rejected rather than ignored: silently dropping a scope the caller asked for would
// hand them a token they believe is more capable than it is, and silently *keeping* one this build does not
// understand would mean a future release could widen an existing token's reach.
var AllScopes = []Scope{
	ScopeIdentify,
	ScopeGuildsRead, ScopeGuildsWrite, ScopeGuildsAudit,
	ScopeMessagesRead, ScopeMessagesWrite,
}

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
