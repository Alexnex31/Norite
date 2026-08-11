package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/logging"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// MaxAPITokenNameLength bounds the human label on a token.
const MaxAPITokenNameLength = 100

// MintAPITokenInput describes a token to create.
type MintAPITokenInput struct {
	Name   string
	Scopes []Scope
}

// MintedAPIToken is the one and only time a token's raw value exists outside the client.
type MintedAPIToken struct {
	Token db.ApiToken
	// Raw is the credential itself. It is never stored and cannot be recovered — only its SHA-256 is kept —
	// so a caller that loses it must mint a new one.
	Raw string
}

// MintAPIToken creates a scoped token for the actor's own account.
//
// Requires a *user* actor. An API token may not mint API tokens: it could otherwise mint one with scopes it
// does not itself hold, which would make the whole scope system decorative (see model.go). The handler
// enforces the actor kind; this method assumes it and documents the assumption.
func (s *Service) MintAPIToken(ctx context.Context, userID snowflake.ID, in MintAPITokenInput) (MintedAPIToken, error) {
	name := strings.TrimSpace(in.Name)
	switch {
	case name == "":
		return MintedAPIToken{}, fmt.Errorf("%w: a token name is required", ErrUnknownScope)
	case len(name) > MaxAPITokenNameLength:
		return MintedAPIToken{}, fmt.Errorf("%w: a token name must be at most %d bytes", ErrUnknownScope, MaxAPITokenNameLength)
	}

	// Every requested scope must be one this build understands. Dropping an unknown scope silently would
	// hand back a token weaker than the caller believes; keeping it would let a later release, which does
	// understand the string, widen an existing token's reach without anyone re-approving it.
	scopes := make([]string, 0, len(in.Scopes))
	seen := make(map[Scope]struct{}, len(in.Scopes))
	for _, scope := range in.Scopes {
		if !ValidScope(scope) {
			return MintedAPIToken{}, fmt.Errorf("%w: %q", ErrUnknownScope, scope)
		}
		if _, dup := seen[scope]; dup {
			continue
		}
		seen[scope] = struct{}{}
		scopes = append(scopes, string(scope))
	}

	raw, hash, err := GenerateAPIToken()
	if err != nil {
		return MintedAPIToken{}, err
	}
	id, err := s.ids.Next()
	if err != nil {
		return MintedAPIToken{}, fmt.Errorf("generating token ID: %w", err)
	}

	token, err := s.queries.CreateAPIToken(ctx, db.CreateAPITokenParams{
		ID:        int64(id),
		UserID:    int64(userID),
		Name:      name,
		TokenHash: hash,
		Scopes:    scopes,
	})
	if err != nil {
		return MintedAPIToken{}, fmt.Errorf("creating API token: %w", err)
	}

	return MintedAPIToken{Token: token, Raw: raw}, nil
}

// ListAPITokens returns the actor's own tokens, without any credential material.
func (s *Service) ListAPITokens(ctx context.Context, userID snowflake.ID) ([]db.ApiToken, error) {
	tokens, err := s.queries.ListAPITokensForUser(ctx, int64(userID))
	if err != nil {
		return nil, fmt.Errorf("listing API tokens: %w", err)
	}
	return tokens, nil
}

// RevokeAPIToken revokes one of the actor's own tokens.
//
// Ownership is enforced in the statement's WHERE clause rather than by a check here, so there is no version
// of this that forgets it (CLAUDE.md rule 1). A token belonging to someone else is reported as not found —
// the same answer as one that does not exist, so the endpoint cannot be used to probe for valid IDs.
func (s *Service) RevokeAPIToken(ctx context.Context, userID, tokenID snowflake.ID) error {
	_, err := s.queries.RevokeAPIToken(ctx, db.RevokeAPITokenParams{
		ID:     int64(tokenID),
		UserID: int64(userID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("revoking API token: %w", err)
	}
	return nil
}

// AuthenticateAPIToken resolves a raw API token to an actor.
//
// Called on every request presenting one, so it is a single indexed lookup by hash and nothing more.
func (s *Service) AuthenticateAPIToken(ctx context.Context, raw string) (Actor, error) {
	hash, err := ParseAPIToken(raw)
	if err != nil {
		return Actor{}, ErrInvalidToken
	}

	// One statement covers all three conditions — the token exists, is not revoked, and its account is not
	// soft-deleted — so the hot path is a single indexed lookup rather than a token read plus a user read.
	// A deleted account's tokens must stop working immediately; revoking them individually is M11's job,
	// and until then this join is what closes the gap.
	token, err := s.queries.GetActiveAPITokenByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Actor{}, ErrInvalidToken
		}
		return Actor{}, fmt.Errorf("looking up API token: %w", err)
	}

	// Fire-and-forget, and throttled in SQL to at most one write per token per few minutes — see the query.
	// Logged rather than swallowed so a persistently failing write is still visible.
	if err := s.queries.TouchAPIToken(ctx, token.ID); err != nil {
		logging.FromContext(ctx).Warn().Err(err).Msg("could not record API token usage")
	}

	scopes := make([]Scope, 0, len(token.Scopes))
	for _, s := range token.Scopes {
		scopes = append(scopes, Scope(s))
	}

	return Actor{
		Kind:    ActorAPIToken,
		UserID:  snowflake.ID(token.UserID),
		TokenID: snowflake.ID(token.ID),
		Scopes:  scopes,
	}, nil
}

// AuthenticateAccessToken resolves a JWT access token to an actor.
//
// No database round trip: the signature and expiry are the whole check, which is what makes access tokens
// cheap and why their TTL is short (see AccessTokenTTL). Revocation therefore lands on the refresh session,
// not on tokens already issued.
func (s *Service) AuthenticateAccessToken(_ context.Context, raw string) (Actor, error) {
	claims, err := s.issuer.Verify(raw)
	if err != nil {
		return Actor{}, ErrInvalidToken
	}
	userID, err := claims.UserID()
	if err != nil {
		return Actor{}, ErrInvalidToken
	}
	// A missing or malformed session claim is tolerated rather than fatal: it only weakens a future
	// per-session revocation check (M11), and rejecting the token outright would break every token issued
	// before that claim existed.
	sessionID, _ := claims.Session()

	return Actor{Kind: ActorUser, UserID: userID, SessionID: sessionID}, nil
}

// GetUser returns an account by ID.
func (s *Service) GetUser(ctx context.Context, id snowflake.ID) (db.User, error) {
	user, err := s.queries.GetUserByID(ctx, int64(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.User{}, ErrNotFound
		}
		return db.User{}, fmt.Errorf("looking up account: %w", err)
	}
	return user, nil
}

// uniqueViolation returns the constraint name when err is a Postgres unique-violation, or "".
//
// Used to turn a lost uniqueness race into the same conflict the pre-check would have reported, rather than
// a 500 that tells the caller nothing actionable.
func uniqueViolation(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
		if pgErr.ConstraintName != "" {
			return pgErr.ConstraintName
		}
		return "unknown"
	}
	return ""
}
