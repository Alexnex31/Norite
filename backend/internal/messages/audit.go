// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package messages

import (
	"context"
	"fmt"
	"time"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/guildauth"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// MessageAuditEntry is one recorded message action, on the wire (M16b).
//
// # Content is present and that is the whole surface
//
// M16's triage listing deliberately carries no content and puts it behind the single-report read, because
// fifty reports carrying four thousand characters each is a payload nobody needs in order to badge a
// queue. This log is the opposite case: the content *is* what a guild switched recording on to keep, and
// a listing without it would answer nothing. The bound is the page size instead — see [maxPageSize].
//
// # There is no `is_e2e` field, and its absence is the rule-13 answer rather than an omission
//
// An encrypted message produces no row at all: the exclusion is a join predicate in the writer, so
// nothing reaches this table to be flagged. That is deliberately unlike M16a's history envelope, which
// carries `is_e2e` because it addresses one message and has to say *why* it is empty. A log has no such
// obligation — a row that does not exist needs no explanation, and adding a field that is always false
// would be the second place the exclusion had to be right.
//
// Content is nullable all the same, because the column is: a NOT NULL column would turn rule 13's
// exclusion into an error rather than an absence on the day something does write a row without content.
type MessageAuditEntry struct {
	ID        snowflake.ID `json:"id"`
	MessageID snowflake.ID `json:"message_id"`
	ChannelID snowflake.ID `json:"channel_id"`
	ActorID   snowflake.ID `json:"actor_id"`
	// Action is one of create, edit or delete — [MessageAuditActions]. Deliberately not a verb from
	// `guilds.AuditActions()`, which is a different vocabulary for a different table; see the constants.
	Action string `json:"action"`
	// Content is what the message said as of this action. For a delete, that is the text being removed —
	// see [Service.Delete] for why it is recorded rather than left null.
	Content   *string   `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// GuildAuditInput is a request for one page of a guild's recording log.
type GuildAuditInput struct {
	GuildID snowflake.ID
	Before  *snowflake.ID
	Limit   int32
}

// GuildMessageAudit returns one page of a guild's recorded messages, newest first.
//
// # The permission is PermViewMessageAudit and nothing else would have done
//
// This is the widest read in the product: every message in the guild, including channels the caller
// cannot view, including the private ones, with no per-object step of the kind M16 and M16a both had. M16
// bounds content by a report existing; M16a takes a bare message id; this takes a guild id and returns
// the conversation. So the bit is the entire boundary, and it is a bit of its own — see
// [roles.PermViewMessageAudit] for why each of the three existing candidates is wrong differently, and
// `docs/security-ledger.md` for the disclosure stated rather than inherited.
//
// **An Instance Admin passes, deliberately**, because [guildauth.Decision.Allows] answers for layer 1 and
// nothing here overrides it. That is M16a's already-ledgered gap — the tier reads any guild's content and
// rule 14 has nowhere to record it until M72 — and this does not widen it: what the tier cannot do is
// *start* the recording, which is [guilds.mayFlipMessageAudit]'s refusal. Reading what a guild chose to
// collect and deciding what the instance collects about a guild that chose nothing are different acts,
// and only the second is refused here.
//
// # Guild-level authorize, not channel-level
//
// The log spans every channel in the guild, so there is no channel to resolve and no visibility question
// to ask — which is the same shape M16's triage queue has, and it uses the same entry point. A non-member
// gets 404 from [guildauth.Authorize] before any row is read; a member without the bit gets 403, because
// they already know the guild exists.
//
// # Rule 13 is not re-checked here, and that is the decision rather than the oversight
//
// The exclusion happened at write time. Repeating it on the read would be the bound-enforced-twice
// mistake M15 recorded, where `checkContent` measured bytes and the validator it claimed to agree with
// measured runes — two copies that disagree are worse than one. The structural half is pinned by a test
// asserting `message_audit_entries.guild_id` is NOT NULL: a DM has no guild, so no DM's message could
// ever have produced a row here.
//
// # No transaction
//
// One statement, so there is no second read for it to agree with — which is the property M16a's History
// needed a REPEATABLE READ snapshot for, and the reason that comment is worth reading before adding a
// second query to this function.
func (s *Service) GuildMessageAudit(
	ctx context.Context, actor auth.Actor, in GuildAuditInput,
) ([]MessageAuditEntry, error) {
	if _, err := guildauth.Authorize(
		ctx, s.queries, actor, in.GuildID, 0, roles.PermViewMessageAudit,
	); err != nil {
		return nil, err
	}

	limit := in.Limit
	switch {
	case limit <= 0:
		limit = defaultPageSize
	case limit > maxPageSize:
		limit = maxPageSize
	}

	rows, err := s.queries.ListGuildMessageAudit(ctx, db.ListGuildMessageAuditParams{
		GuildID: int64(in.GuildID),
		Before:  idOrNil(in.Before),
		Limit:   limit,
	})
	if err != nil {
		return nil, fmt.Errorf("messages: list guild message audit: %w", err)
	}

	out := make([]MessageAuditEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, MessageAuditEntry{
			ID:        snowflake.ID(row.ID),
			MessageID: snowflake.ID(row.MessageID),
			ChannelID: snowflake.ID(row.ChannelID),
			ActorID:   snowflake.ID(row.ActorID),
			Action:    row.Action,
			Content:   row.Content,
			CreatedAt: row.CreatedAt.Time,
		})
	}
	return out, nil
}
