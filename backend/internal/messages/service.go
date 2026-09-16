// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package messages is the channel message surface: send, read, edit and delete.
//
// Authority is decided by [guildauth], never here. That package is the chokepoint M12 built and M15
// extracted so this package could reach it without importing `guilds` — so every operation below opens by
// calling it, and there is no second path to a message that skips it.
package messages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/guildauth"
	"github.com/Alexnex31/Norite/backend/internal/platform/database"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// Service owns the message operations.
type Service struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	ids     *snowflake.Generator
}

// ServiceOptions configures [NewService].
type ServiceOptions struct {
	Pool *pgxpool.Pool
	IDs  *snowflake.Generator
}

// NewService validates its dependencies at construction rather than at first use.
func NewService(opts ServiceOptions) (*Service, error) {
	switch {
	case opts.Pool == nil:
		return nil, errors.New("messages: a database pool is required")
	case opts.IDs == nil:
		return nil, errors.New("messages: an ID generator is required")
	}
	return &Service{pool: opts.Pool, queries: db.New(opts.Pool), ids: opts.IDs}, nil
}

func (s *Service) inTx(ctx context.Context, fn func(q *db.Queries) error) error {
	return database.RunInTx(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(s.queries.WithTx(tx))
	})
}

// SendInput is a request to post a message.
type SendInput struct {
	ChannelID snowflake.ID
	Content   string
	ReplyToID *snowflake.ID
}

// Send posts a message to a channel.
//
// The whole operation is one transaction: the authorization (so rule 1's "freshly loaded" data is read in
// the same snapshot the write happens in), the insert, and the `last_message_id` update that would
// otherwise be a second round trip a crash could lose.
//
// No audit entry. Rule 2 was narrowed at M15 and posting in a channel you are permitted to post in
// exercises authority over nobody — see [ActionMessageDelete] for the whole reasoning.
func (s *Service) Send(ctx context.Context, actor auth.Actor, in SendInput) (Message, error) {
	id, err := s.ids.Next()
	if err != nil {
		return Message{}, fmt.Errorf("messages: mint message id: %w", err)
	}

	var out Message
	err = s.inTx(ctx, func(q *db.Queries) error {
		// The locking variant, because this writes. AuthorizeChannelForRead exists for the read path and
		// the difference is the point — see guildauth.AuthorizeChannelForRead.
		if _, _, _, err := guildauth.AuthorizeChannel(
			ctx, q, actor, in.ChannelID, roles.PermSendMessages,
		); err != nil {
			return err
		}

		reply, err := s.resolveReply(ctx, q, in.ChannelID, in.ReplyToID)
		if err != nil {
			return err
		}

		authorID := int64(actor.UserID)
		row, err := q.CreateMessage(ctx, db.CreateMessageParams{
			ID:        int64(id),
			ChannelID: int64(in.ChannelID),
			AuthorID:  &authorID,
			Content:   in.Content,
			Type:      0,
			ReplyToID: reply,
		})
		if err != nil {
			return fmt.Errorf("messages: create message: %w", err)
		}

		if err := q.SetChannelLastMessage(ctx, db.SetChannelLastMessageParams{
			ID: int64(in.ChannelID), LastMessageID: &row.ID,
		}); err != nil {
			return fmt.Errorf("messages: set last message: %w", err)
		}

		out = messageFromRow(row)
		return nil
	})
	return out, err
}

// resolveReply validates reply_to_id, which must name a live message in *this* channel.
//
// Same reasoning as M12's parent_id check: an unvalidated cross-channel reference is a disclosure, not
// merely a data-integrity problem. Replying to a message id from a channel the caller cannot see would
// confirm that id exists, which is exactly what the channel filter withholds — so the refusal is a plain
// 400 that says nothing about whether the target exists somewhere else.
func (s *Service) resolveReply(
	ctx context.Context, q *db.Queries, channelID snowflake.ID, replyTo *snowflake.ID,
) (*int64, error) {
	if replyTo == nil {
		return nil, nil
	}

	target, err := q.GetMessage(ctx, int64(*replyTo))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.Errorf(httpx.ErrBadRequest, "reply_to_id does not name a message in this channel")
		}
		return nil, fmt.Errorf("messages: get reply target: %w", err)
	}
	if snowflake.ID(target.ChannelID) != channelID {
		return nil, httpx.Errorf(httpx.ErrBadRequest, "reply_to_id does not name a message in this channel")
	}

	v := target.ID
	return &v, nil
}

// ListInput is a request for one page of a channel's backlog.
type ListInput struct {
	ChannelID snowflake.ID
	Before    *snowflake.ID
	After     *snowflake.ID
	Limit     int32
}

// List returns one page of a channel's messages, newest first.
//
// Authorized with the *non-locking* entry point. This is the hottest read in the product and the locking
// variant would take an exclusive row lock on the channel per page, serializing every member reading the
// same channel and queueing them behind any in-flight channel edit.
func (s *Service) List(ctx context.Context, actor auth.Actor, in ListInput) ([]Message, error) {
	// PermReadMessageHistory, not merely PermViewChannel. AuthorizeChannelForRead folds the view bit in
	// on top of whatever is asked for, so this is "can see the channel AND may read what was said in it" —
	// the two are separate grants, and a channel configured view+send without history is one whose earlier
	// conversation is not for whoever was added last.
	if _, _, _, err := guildauth.AuthorizeChannelForRead(
		ctx, s.queries, actor, in.ChannelID, roles.PermReadMessageHistory,
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

	rows, err := s.queries.ListChannelMessages(ctx, db.ListChannelMessagesParams{
		ChannelID: int64(in.ChannelID),
		Before:    idOrNil(in.Before),
		After:     idOrNil(in.After),
		Limit:     limit,
	})
	if err != nil {
		return nil, fmt.Errorf("messages: list channel messages: %w", err)
	}

	out := make([]Message, 0, len(rows))
	for _, row := range rows {
		out = append(out, messageFromRow(row))
	}
	return out, nil
}

// UpdateInput is a request to edit a message's content.
type UpdateInput struct {
	ChannelID snowflake.ID
	MessageID snowflake.ID
	Content   string
}

// Update edits a message, and only its author may do it.
//
// **PermManageMessages does not grant this, deliberately.** That bit lets a moderator *delete* somebody
// else's message, which is a moderation action with a visible outcome. Rewriting somebody's words is not
// moderation — it puts words in their mouth under their name, and an audit entry recording that it
// happened does not make the message honest. So there is no moderator path here at all, and consequently
// no `message.edit` audit verb.
//
// The prior content is appended to message_edit_history inside this transaction, so an edit that commits
// without its history row is not a state the code can reach.
func (s *Service) Update(ctx context.Context, actor auth.Actor, in UpdateInput) (Message, error) {
	historyID, err := s.ids.Next()
	if err != nil {
		return Message{}, fmt.Errorf("messages: mint history id: %w", err)
	}

	var out Message
	err = s.inTx(ctx, func(q *db.Queries) error {
		if _, _, _, err := guildauth.AuthorizeChannel(ctx, q, actor, in.ChannelID, 0); err != nil {
			return err
		}

		row, err := s.loadInChannel(ctx, q, in.ChannelID, in.MessageID)
		if err != nil {
			return err
		}

		// Authorship is checked after the row is loaded, which makes it an authorization question rather
		// than an input one — M12's "refuse before explaining" correction. Somebody who can see the
		// channel gets 403 here; somebody who cannot never reached this line.
		if row.AuthorID == nil || snowflake.ID(*row.AuthorID) != actor.UserID {
			return httpx.Errorf(httpx.ErrForbidden, "only a message's author may edit it")
		}

		if err := q.AppendMessageEditHistory(ctx, db.AppendMessageEditHistoryParams{
			ID:        int64(historyID),
			MessageID: row.ID,
			Content:   row.Content, // the content *before* this edit
		}); err != nil {
			return fmt.Errorf("messages: append edit history: %w", err)
		}

		updated, err := q.UpdateMessageContent(ctx, db.UpdateMessageContentParams{
			ID: row.ID, Content: in.Content,
		})
		if err != nil {
			return fmt.Errorf("messages: update message: %w", err)
		}

		out = messageFromRow(updated)
		return nil
	})
	return out, err
}

// Delete removes a message: its author may, and so may a holder of PermManageMessages.
//
// **Only the moderator path writes an audit entry.** An author deleting their own message exercises
// authority over nobody, and rule 2 covers administrative mutations — so the entry exists exactly when
// somebody acted on a person other than themselves. The `changes` payload carries ids and the author,
// never the content: rule 13 forbids reading message content on a server-side path without excluding E2E
// DMs, and the cheapest way to satisfy that here is not to put content in the audit log at all.
func (s *Service) Delete(
	ctx context.Context, actor auth.Actor, channelID, messageID snowflake.ID,
) error {
	auditID, err := s.ids.Next()
	if err != nil {
		return fmt.Errorf("messages: mint audit entry id: %w", err)
	}

	return s.inTx(ctx, func(q *db.Queries) error {
		_, guildID, decision, err := guildauth.AuthorizeChannel(ctx, q, actor, channelID, 0)
		if err != nil {
			return err
		}

		row, err := s.loadInChannel(ctx, q, channelID, messageID)
		if err != nil {
			return err
		}

		isAuthor := row.AuthorID != nil && snowflake.ID(*row.AuthorID) == actor.UserID
		if !isAuthor && !decision.Allows(roles.PermManageMessages) {
			return httpx.Errorf(httpx.ErrForbidden, "only the author or a moderator may delete a message")
		}

		if err := q.SoftDeleteMessage(ctx, row.ID); err != nil {
			return fmt.Errorf("messages: soft delete message: %w", err)
		}

		if isAuthor {
			return nil
		}

		return s.writeModerationAudit(ctx, q, auditID, guildID, actor.UserID, row)
	})
}

// writeModerationAudit records a moderator deleting somebody else's message (rule 2).
//
// Written inline rather than through a shared helper because this package has exactly one audit verb.
// `guilds` has sixteen and its own writer; a second general-purpose one here would be an abstraction
// built for a single caller. If a third package needs to write guild audit entries, that is the point to
// extract one — the same argument that moved the authorization chokepoint at M15, applied to the same
// shape and not yet earned.
func (s *Service) writeModerationAudit(
	ctx context.Context, q *db.Queries, auditID, guildID, actorID snowflake.ID, row db.Message,
) error {
	changes := map[string]any{"channel_id": snowflake.ID(row.ChannelID)}
	if row.AuthorID != nil {
		changes["author_id"] = snowflake.ID(*row.AuthorID)
	}

	encoded, err := json.Marshal(changes)
	if err != nil {
		return fmt.Errorf("messages: encode audit changes: %w", err)
	}

	guild := int64(guildID)
	target := row.ID
	if err := q.WriteAuditLogEntry(ctx, db.WriteAuditLogEntryParams{
		ID:       int64(auditID),
		GuildID:  &guild,
		ActorID:  int64(actorID),
		Action:   ActionMessageDelete,
		TargetID: &target,
		Changes:  encoded,
	}); err != nil {
		return fmt.Errorf("messages: write audit entry: %w", err)
	}
	return nil
}

// loadInChannel reads a message and refuses one that belongs to a different channel.
//
// The channel in the path is what was authorized, so a message id from elsewhere must not be reachable
// through it — otherwise the permission check covers one channel and the mutation lands in another. The
// refusal is 404 rather than 403 for the reason the channel routes answer 404: whether an id names a
// message in a channel the caller cannot see is exactly what the filter withholds.
func (s *Service) loadInChannel(
	ctx context.Context, q *db.Queries, channelID, messageID snowflake.ID,
) (db.Message, error) {
	row, err := q.GetMessageForUpdate(ctx, int64(messageID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Message{}, httpx.ErrNotFound
		}
		return db.Message{}, fmt.Errorf("messages: get message: %w", err)
	}
	if snowflake.ID(row.ChannelID) != channelID {
		return db.Message{}, httpx.ErrNotFound
	}
	return row, nil
}

func idOrNil(id *snowflake.ID) *int64 {
	if id == nil {
		return nil
	}
	v := int64(*id)
	return &v
}
