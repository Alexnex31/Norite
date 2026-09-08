// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package guilds owns guilds, their channels, their roles and their membership.
//
// # Where authority is decided
//
// Every mutating path in this package goes through [Service.authorize] and nothing else. That is the whole
// design: ADR 0008's layer 1 (Instance Admin) is checked here, layers 2 through 5 are delegated to
// roles.Resolve, and no handler assembles a permission check of its own.
//
// This codebase has had to make a rule structural three times after finding the one call site that forgot
// it — revokeEverything after four milestones each added a claim to revoke, RequireLiveSession after
// POST /auth/tokens escaped two per-handler checks, and factorProof after the device page turned out to
// mint approval without a factor. Each time the fix was to make forgetting impossible rather than to add
// the missing call. This milestone is the first one where the call sites are being written from scratch,
// so the chokepoint exists before them rather than after.
package guilds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/database"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// Sentinel errors, mapped to responses by Handler.writeErr.
//
// Deliberately few. Most refusals in this package are authorize's, and authorize returns httpx sentinels
// directly so that a handler cannot accidentally translate "not a member" into something more informative
// than 404 — see its comment on why the two refusals differ.
var (
	// ErrDefaultRoleImmutable reports an attempt to delete or reposition @everyone.
	ErrDefaultRoleImmutable = errors.New("guilds: the default role cannot be deleted or repositioned")

	// ErrUnsupportedChannelType reports a channel type a guild cannot contain.
	ErrUnsupportedChannelType = errors.New("guilds: unsupported channel type")

	// ErrAlreadyAMember reports a join that collides with an existing membership.
	ErrAlreadyAMember = errors.New("guilds: already a member of this guild")

	// ErrCannotRemoveOwner reports an attempt to remove the guild owner from their own guild.
	//
	// Not a permission question: nothing in ADR 0008 grants the authority, because the owner *is* layer 2
	// and a guild whose owner is not a member has no layer 2 at all. Ownership transfer is the operation
	// that would make this reachable, and it is not this milestone.
	ErrCannotRemoveOwner = errors.New("guilds: the owner cannot be removed from their own guild")
)

// Service holds the guild domain logic.
type Service struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	ids     *snowflake.Generator
}

// ServiceOptions configures NewService.
type ServiceOptions struct {
	Pool *pgxpool.Pool
	IDs  *snowflake.Generator
}

// NewService builds the guild service.
func NewService(opts ServiceOptions) (*Service, error) {
	switch {
	case opts.Pool == nil:
		return nil, errors.New("guilds: a database pool is required")
	case opts.IDs == nil:
		return nil, errors.New("guilds: an ID generator is required")
	}

	return &Service{
		pool:    opts.Pool,
		queries: db.New(opts.Pool),
		ids:     opts.IDs,
	}, nil
}

// inTx runs fn inside a transaction, with a querier bound to it.
//
// Every mutation in this package uses it, because rule 2 requires the mutation and its audit entry to
// share a transaction — and the shortest way to keep that true is for there to be no other way to write.
func (s *Service) inTx(ctx context.Context, fn func(q *db.Queries) error) error {
	return database.RunInTx(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(s.queries.WithTx(tx))
	})
}

// writeAudit records one guild-scoped mutation (rule 2).
//
// It takes the transaction's querier rather than the pool, which is the whole point: a mutation that
// commits without its audit entry is not a state this schema can reach, because there is no code path
// that writes one without the other.
//
// changes may be nil. M12 writes either nil or a flat map of the fields a request actually changed; M14
// owns the before/after diffing that produces a richer shape, and this column is jsonb so that arrives
// without a migration.
func (s *Service) writeAudit(
	ctx context.Context,
	q *db.Queries,
	guildID, actorID snowflake.ID,
	action string,
	targetID *snowflake.ID,
	changes map[string]any,
) error {
	id, err := s.ids.Next()
	if err != nil {
		return fmt.Errorf("guilds: mint audit entry id: %w", err)
	}

	var encoded []byte
	if len(changes) > 0 {
		if encoded, err = json.Marshal(changes); err != nil {
			return fmt.Errorf("guilds: encode audit changes: %w", err)
		}
	}

	var target *int64
	if targetID != nil {
		v := int64(*targetID)
		target = &v
	}

	guild := int64(guildID)

	if err := q.WriteAuditLogEntry(ctx, db.WriteAuditLogEntryParams{
		ID:       int64(id),
		GuildID:  &guild,
		ActorID:  int64(actorID),
		Action:   action,
		TargetID: target,
		Changes:  encoded,
	}); err != nil {
		return fmt.Errorf("guilds: write audit entry: %w", err)
	}

	return nil
}
