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
// # A shared tag also needs PermSendMessages, and a private one does not
//
// "A guild that lets everybody post" was the argument, and posting was never checked — so a member muted
// with a channel overwrite could not say anything and could still label other people's messages, in
// front of everybody, under their own name. Found by M17's sweep; M15's lesson that a write is bounded by
// the permission that bounds writing, whichever verb performs it. A **private** tag is invisible to
// everybody else, a bookmark rather than speech, so it keeps needing only the right to read — a member
// may still file an announcement they cannot reply to. Removing your own application needs no send
// either, for the reason a muted author may still delete their messages: redaction is what a mute wants.
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

		// First, and before anything that could answer differently for a message that exists: the repeat
		// detection below reads the application back, so without this a refused insert on a message in
		// another channel would find somebody else's row there and report success.
		if err := loadMessageInChannel(ctx, q, in.ChannelID, in.MessageID); err != nil {
			return err
		}

		// The tag has to be visible to this caller, and it has to be this guild's. loadInGuild answers both
		// as 404, which is what stops a tag id becoming a probe — see its comment.
		tag, err := s.loadInGuild(ctx, q, actor, guildID, in.TagID)
		if err != nil {
			return err
		}

		// After the message and the tag have both answered, so the caller already knows both exist: they
		// can read this channel and see this tag. A 403 here discloses nothing a 404 would have hidden.
		if tag.IsShared && !decision.Allows(roles.PermSendMessages) {
			return httpx.Errorf(httpx.ErrForbidden,
				"putting a shared tag on a message needs permission to send messages in this channel")
		}

		affected, err := q.ApplyMessageTag(ctx, db.ApplyMessageTagParams{
			TagID:     int64(in.TagID),
			MessageID: int64(in.MessageID),
			ChannelID: int64(in.ChannelID),
			AppliedBy: int64(actor.UserID),
		})
		if err != nil {
			if isForeignKeyViolation(err) {
				// The tag or the message was deleted after this transaction read it and before the insert
				// checked the reference: Delete's lock makes that insert wait, and it then finds the row
				// gone. The same answer a request arriving a moment later would get.
				return httpx.ErrNotFound
			}
			return fmt.Errorf("tags: apply tag: %w", err)
		}

		// Zero rows means the statement's own guards refused: the message is not in this channel or this
		// guild, is soft-deleted, or does not exist. All of them are 404 for the reason loadInChannel
		// answers 404 — a message the caller cannot reach through this channel is one whose existence is
		// not theirs to learn. A *repeat* application is not in this set: ON CONFLICT DO NOTHING also
		// reports zero, so the service cannot tell them apart from the count alone, which is why the
		// repeat case is resolved below rather than guessed at.
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
// Whoever applied it, or a PermManageMessages holder. Undoing your own action needs no permission, and
// somebody has to be able to remove a wrong label after its applier has gone — the shape
// `messages.Delete` has, author or moderator.
//
// **The tag's owner was a third authority until M17's sweep**, and for a shared tag it was the wrong one.
// A shared tag is the guild's once other people apply it, so its creator — who may since have lost the
// moderation bit — could strip labels other members had put on messages. A private tag's owner is still
// covered, because nobody but its owner can apply it: they are always the applier.
//
// # When it is audited
//
// When the application was somebody else's. That is a moderator acting on another member's label, which
// rule 2 counts as administrative whoever performs it — the same line `message.delete` draws.
func (s *Service) Unapply(ctx context.Context, actor auth.Actor, in ApplyInput) error {
	return s.inTx(ctx, func(q *db.Queries) error {
		_, guildID, decision, err := guildauth.AuthorizeChannelUnlocked(
			ctx, q, actor, in.ChannelID, roles.PermReadMessageHistory,
		)
		if err != nil {
			return err
		}

		// Before the application is read, because the authority answer below is 403 and "no such
		// application" is 404 — reached through the wrong channel, that difference says which tags are on
		// a message the caller may not be able to read at all.
		if err := loadMessageInChannel(ctx, q, in.ChannelID, in.MessageID); err != nil {
			return err
		}

		tag, err := s.loadInGuild(ctx, q, actor, guildID, in.TagID)
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
		if !isApplier && !decision.Allows(roles.PermManageMessages) {
			return httpx.Errorf(httpx.ErrForbidden, "only whoever applied the tag, or a moderator, may remove it")
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

		if isApplier {
			return nil
		}
		return s.writeAudit(ctx, q, guildID, actor.UserID, ActionTagRemove, in.MessageID, map[string]any{
			"channel_id": in.ChannelID,
			"tag_id":     in.TagID,
			"tag_name":   tag.Name,
			"applied_by": snowflake.ID(application.AppliedBy),
		})
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

	if err := loadMessageInChannel(ctx, s.queries, channelID, messageID); err != nil {
		return nil, err
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

// loadMessageInChannel refuses a message that is not a live message in the channel the route named.
//
// The channel in the route is what was authorized, so a message from anywhere else has to answer exactly
// as a message that does not exist — or the permission check covered one channel while the act landed in
// another. M15's loadInChannel, one package over. All three routes on a message's tags call it, and
// before anything whose answer could differ for a message that exists: M17 shipped it in the read only,
// and the manual pass found tagging a message in a hidden channel through a visible one answering 204,
// and removing a tag from it answering 403, both against 404 for an id naming nothing.
//
// A soft-deleted message is refused like a missing one, which GetMessage already does. That makes all
// three routes agree: a tag cannot be added to, read from or removed from a message that has been
// deleted — the application row stays, invisible, until the message is hard-deleted and the row cascades.
func loadMessageInChannel(ctx context.Context, q *db.Queries, channelID, messageID snowflake.ID) error {
	msg, err := q.GetMessage(ctx, int64(messageID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound
		}
		return fmt.Errorf("tags: get message: %w", err)
	}
	if snowflake.ID(msg.ChannelID) != channelID {
		return httpx.ErrNotFound
	}
	return nil
}
