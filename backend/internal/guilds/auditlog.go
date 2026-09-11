// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"context"
	"fmt"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// Audit-log page size, matching the member listing's ceiling for the reason that one states: 100 is what
// every comparable API uses, so a client written against one of them paginates correctly here without
// having read this.
const (
	defaultAuditLogPageSize = 50
	maxAuditLogPageSize     = 100
)

// ListAuditLogInput is a cursor page request.
type ListAuditLogInput struct {
	// Before is the entry id to resume from, exclusive. Zero starts at the newest.
	//
	// Before rather than the member listing's After, because this listing runs newest-first and a cursor
	// names the direction of travel. The plan for this milestone said "follow the member listing exactly";
	// following it on this field would page backwards through history from the oldest entry, which is not
	// what anybody opens an audit log to read.
	//
	// A cursor on the id and not on created_at. Snowflakes are time-ordered (ADR 0003) so the two agree on
	// order, and only the id is unique — nothing constrains created_at, so a cursor over it would skip or
	// repeat an entry at any page boundary landing inside a group of equal values. Migration 000017 carries
	// the full argument, including the measurement showing that group does not occur today and why that
	// argues for the id rather than against it.
	Before snowflake.ID

	// Action and ActorID narrow the page. Empty and zero mean absent, which is why the query uses
	// sqlc.narg rather than relying on "" and 0 being unreachable values.
	Action  string
	ActorID snowflake.ID

	Limit int32
}

// ListAuditLog returns a page of a guild's audit log, newest first.
//
// # What this endpoint discloses, which is a decision rather than an oversight
//
// Entries name channels, and since M13 the channel listing hides channels a member cannot view. This
// listing does not filter to match: a reader holding PermViewAuditLog sees `channel.create` for a channel
// their own sidebar will not show them, by id and — in the entry's `changes` — by name.
//
// Three options were on the table and this is the middle one. Filtering entries by what the reader can
// currently see is the tempting answer and is wrong twice over: half the interesting entries are
// deletions, whose target no longer exists and can no longer be resolved to a permission at all, and the
// visibility of a channel *today* is not the question an audit log answers about an action taken a month
// ago — a moderator could hide their tracks after the fact by locking a channel down. Withholding the
// whole surface unless the reader is an administrator was the other option, and it makes the log useless
// for exactly the delegated-moderator case it exists for.
//
// So the permission is the boundary. PermViewAuditLog is not granted by default, is not implied by
// PermManageGuild, and cannot be given by somebody who does not hold it (refuseEscalation) — granting it
// is granting sight of every moderation action in the guild, including the ones taken in channels the
// grantee cannot open. That is stated in the contract, in docs/security-ledger.md, and here.
//
// # What it does not resolve
//
// Ids stay ids. target_id is never joined to the channel, role or member it names: half the entries are
// deletions whose target is gone, and resolving the rest would be an N+1 across four tables on a
// paginated endpoint. A client renders a name from its own cache or shows the id.
func (s *Service) ListAuditLog(
	ctx context.Context, actor auth.Actor, guildID snowflake.ID, in ListAuditLogInput,
) ([]AuditLogEntry, error) {
	if err := s.authorize(ctx, actor, guildID, 0, roles.PermViewAuditLog); err != nil {
		return nil, err
	}

	limit := in.Limit
	switch {
	case limit <= 0:
		limit = defaultAuditLogPageSize
	case limit > maxAuditLogPageSize:
		// Clamped rather than refused, as the member listing is. A client asking for more than the ceiling
		// is not making an error worth failing a request over, and returning the ceiling tells it what the
		// ceiling is more clearly than a 400 it has to parse.
		limit = maxAuditLogPageSize
	}

	params := db.ListGuildAuditLogParams{GuildID: int64(guildID), Lim: limit}

	// Absent means absent. Passing a zero through as a value would make "no cursor" mean "before entry 0"
	// and "no actor filter" mean "actor 0" — the first is harmless by accident and the second is a filter
	// that matches nothing, which reads to a client as an empty log.
	if in.Before != 0 {
		before := int64(in.Before)
		params.Before = &before
	}
	if in.Action != "" {
		action := in.Action
		params.Action = &action
	}
	if in.ActorID != 0 {
		actorID := int64(in.ActorID)
		params.ActorID = &actorID
	}

	rows, err := s.queries.ListGuildAuditLog(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("guilds: list audit log: %w", err)
	}

	out := make([]AuditLogEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, auditLogEntryFromRow(row))
	}

	return out, nil
}

// knownAuditAction reports whether an action filter names a verb this build writes.
//
// Refused rather than passed through, and the reason is not validation hygiene. An unknown action is a
// query that matches nothing, which is indistinguishable from a guild that has taken no such action — so
// a typo reads as evidence. The vocabulary is closed and this codebase owns every value in it.
func knownAuditAction(action string) bool {
	for _, known := range AllAuditActions {
		if action == known {
			return true
		}
	}
	return false
}

// refuseUnknownAuditAction is knownAuditAction as the error the handler returns.
//
// Names no valid action in the message. The vocabulary is public — it is in the contract — so this is not
// a secret, but a refusal that enumerates is a habit rather than a judgement, and the contract is where a
// client should be reading it from.
func refuseUnknownAuditAction(action string) error {
	if knownAuditAction(action) {
		return nil
	}
	return httpx.Errorf(httpx.ErrBadRequest, "action is not an audit action this instance writes")
}
