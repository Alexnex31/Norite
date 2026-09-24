// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package tags

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/guildauth"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// ApplyInput names a message and the tag to put on it.
type ApplyInput struct {
	ChannelID snowflake.ID
	MessageID snowflake.ID
	TagID     snowflake.ID
}

// Apply puts a tag on a message.
//
// # Reading the message is the permission, and applying is not moderation
//
// PermReadMessageHistory — the bit that lets you read a channel's backlog — because tagging something you
// cannot read is both useless and a probe: without it, applying a tag would answer differently for a
// message id that exists in a channel you cannot see than for one that does not, which is the oracle M12,
// M13 and M16a each closed in their own surface. The locking entry point is not wanted here (M15's test
// is whether the caller *describes* the channel row; this reads two fields off it and writes elsewhere).
//
// Deliberately **not** PermManageMessages. A tag is an annotation, and a guild that lets everybody post
// has already decided they may say things about the conversation; requiring the moderation bit would mean
// the only people who can file anything are the people who can delete it. Creating a *shared* tag is
// where the permission sits, which is the decision M17's entry actually states.
//
// # Three things have to agree and only one of them is checked here
//
// The caller must be able to read the message; the tag must be one they can see; and the tag's guild must
// be the message's guild. The first two are checked in Go because they need the Decision and the actor.
// **The third is a join predicate in the statement** — see ApplyMessageTag — because it is the one that
// must still hold for a writer that is not this function, and because a guard in a statement cannot be
// raced or forgotten. That is M16b's answer to the same shape, and M16's correction to `reports` is what
// happens when nobody asks the question at all.
func (s *Service) Apply(ctx context.Context, actor auth.Actor, in ApplyInput) error {
	return s.inTx(ctx, func(q *db.Queries) error {
		_, guildID, decision, err := guildauth.AuthorizeChannelUnlocked(
			ctx, q, actor, in.ChannelID, roles.PermReadMessageHistory,
		)
		if err != nil {
			return err
		}

		// The tag has to be visible to this caller, and it has to be this guild's. loadInGuild answers both
		// as 404, which is what stops a tag id becoming a probe — see its comment.
		if _, err := s.loadInGuild(ctx, q, actor, decision, guildID, in.TagID); err != nil {
			return err
		}

		affected, err := q.ApplyMessageTag(ctx, db.ApplyMessageTagParams{
			TagID:     int64(in.TagID),
			MessageID: int64(in.MessageID),
			AppliedBy: int64(actor.UserID),
		})
		if err != nil {
			return fmt.Errorf("tags: apply tag: %w", err)
		}

		// Zero rows means the statement's own guards refused: the message is not in this guild, is
		// soft-deleted, or does not exist. All three are 404 for the reason loadInChannel answers 404 — a
		// message the caller cannot reach through this channel is one whose existence is not theirs to
		// learn. A *repeat* application is not in this set: ON CONFLICT DO NOTHING also reports zero, so
		// the service cannot tell them apart from the count alone, which is why the repeat case is
		// resolved below rather than guessed at.
		if affected == 0 {
			if _, err := q.GetMessageTagApplication(ctx, db.GetMessageTagApplicationParams{
				TagID: int64(in.TagID), MessageID: int64(in.MessageID),
			}); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return httpx.ErrNotFound
				}
				return fmt.Errorf("tags: check existing application: %w", err)
			}
			// The row is there, so this was a repeat. Idempotent by design: a client retrying a request it
			// is unsure landed must not be told it failed.
		}
		return nil
	})
}

// Unapply takes a tag off a message.
//
// # Who may
//
// Whoever applied it, the tag's owner, or a PermManageMessages holder. Three, because the three answer
// different questions: undoing your own action needs no permission, a tag is its owner's to withdraw, and
// somebody has to be able to remove a wrong label after both of them have gone.
//
// That is the shape `messages.Delete` already has — author or moderator — with the tag's owner added,
// because unlike a message a tag has two people attached to it.
func (s *Service) Unapply(ctx context.Context, actor auth.Actor, in ApplyInput) error {
	return s.inTx(ctx, func(q *db.Queries) error {
		_, guildID, decision, err := guildauth.AuthorizeChannelUnlocked(
			ctx, q, actor, in.ChannelID, roles.PermReadMessageHistory,
		)
		if err != nil {
			return err
		}

		tag, err := s.loadInGuild(ctx, q, actor, decision, guildID, in.TagID)
		if err != nil {
			return err
		}

		application, err := q.GetMessageTagApplication(ctx, db.GetMessageTagApplicationParams{
			TagID: int64(in.TagID), MessageID: int64(in.MessageID),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return fmt.Errorf("tags: get application: %w", err)
		}

		// After the row loads, so the refusal is an authorization answer rather than an input one.
		isApplier := snowflake.ID(application.AppliedBy) == actor.UserID
		isOwner := snowflake.ID(tag.CreatedBy) == actor.UserID
		if !isApplier && !isOwner && !decision.Allows(roles.PermManageMessages) {
			return httpx.Errorf(httpx.ErrForbidden,
				"only whoever applied the tag, its owner, or a moderator may remove it")
		}

		affected, err := q.UnapplyMessageTag(ctx, db.UnapplyMessageTagParams{
			TagID: int64(in.TagID), MessageID: int64(in.MessageID),
		})
		if err != nil {
			return fmt.Errorf("tags: unapply tag: %w", err)
		}
		if affected == 0 {
			return httpx.ErrNotFound
		}
		return nil
	})
}

// ForMessage returns every tag on one message that this caller can see.
//
// # It goes through the batch query with a one-element array, deliberately
//
// The shape a channel listing needs is "tags for these fifty messages", and resolving that one message at
// a time is §15.2's N+1 on the path every client hits to draw a channel. Building the single-message read
// on top of the batch one means the batch shape exists, is exercised, and is what whoever integrates tags
// into the backlog listing reaches for — rather than a per-message query being the thing already there
// and obviously reusable fifty times.
//
// The visibility filter is in that query: somebody else's private tag never appears, on a message you can
// read or otherwise.
func (s *Service) ForMessage(
	ctx context.Context, actor auth.Actor, channelID, messageID snowflake.ID,
) ([]AppliedTag, error) {
	if _, _, _, err := guildauth.AuthorizeChannelUnlocked(
		ctx, s.queries, actor, channelID, roles.PermReadMessageHistory,
	); err != nil {
		return nil, err
	}

	// The message has to be in the channel that was authorized, or the permission check covered one
	// channel while the read landed in another — M15's loadInChannel, and 404 for its reason.
	msg, err := s.queries.GetMessage(ctx, int64(messageID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound
		}
		return nil, fmt.Errorf("tags: get message: %w", err)
	}
	if snowflake.ID(msg.ChannelID) != channelID {
		return nil, httpx.ErrNotFound
	}

	rows, err := s.queries.ListTagsForMessages(ctx, db.ListTagsForMessagesParams{
		MessageIds: []int64{int64(messageID)},
		ViewerID:   int64(actor.UserID),
	})
	if err != nil {
		return nil, fmt.Errorf("tags: list tags for message: %w", err)
	}

	out := make([]AppliedTag, 0, len(rows))
	for _, row := range rows {
		out = append(out, AppliedTag{
			Tag:       tagFromRow(row.MessageTag),
			AppliedBy: snowflake.ID(row.AppliedBy),
			AppliedAt: row.AppliedAt.Time,
		})
	}
	return out, nil
}
