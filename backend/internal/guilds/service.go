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
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
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
