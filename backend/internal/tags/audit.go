// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package tags

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// writeAudit records one moderation act over somebody else's tagging (rule 2), in the caller's
// transaction so the mutation cannot commit without it.
//
// Inline rather than shared, like messages.writeModerationAudit and reports.writeResolutionAudit. This is
// the third package with a writer of its own, which is the condition reports' comment names for
// extracting one. That extraction touches two other packages and is left as a decision rather than folded
// into a security fix.
//
// `changes` follows M14's two kinds of key: a changed field is an object carrying `from`/`to`, context is
// a scalar. Every key here is context except `name` on a deletion, which is the field that went away.
func (s *Service) writeAudit(
	ctx context.Context, q *db.Queries, guildID, actorID snowflake.ID,
	action string, targetID snowflake.ID, changes map[string]any,
) error {
	auditID, err := s.ids.Next()
	if err != nil {
		return fmt.Errorf("tags: mint audit entry id: %w", err)
	}
	encoded, err := json.Marshal(changes)
	if err != nil {
		return fmt.Errorf("tags: encode audit changes: %w", err)
	}

	guild := int64(guildID)
	target := int64(targetID)
	if err := q.WriteAuditLogEntry(ctx, db.WriteAuditLogEntryParams{
		ID:       int64(auditID),
		GuildID:  &guild,
		ActorID:  int64(actorID),
		Action:   action,
		TargetID: &target,
		Changes:  encoded,
	}); err != nil {
		return fmt.Errorf("tags: write audit entry: %w", err)
	}
	return nil
}
