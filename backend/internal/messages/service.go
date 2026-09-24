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
	"unicode/utf8"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
// Only a text channel accepts one. [ChannelGuildText] carries the reasoning; the check is here rather
// than in guildauth because "may this channel hold a message" is this package's question, not an
// authorization one — guildauth is right that the caller may act in the channel.
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
		// The non-locking variant, deliberately, on the product's highest-volume write.
		//
		// AuthorizeChannel reads the channel FOR UPDATE and holds it to commit, which is right for the
		// four `guilds` mutations that *diff* the channel row — M14 reproduced the race it closes. A send
		// diffs nothing: it reads two fields off this row, `type` and `guild_id`, and writes to
		// `messages`. Taking the lock anyway serialized every send in a channel behind every other for a
		// whole transaction: eight concurrent senders managed 777 sends/s into one channel against 2,294
		// after this change, and the one-channel penalty relative to eight separate channels fell from
		// 3.3x to about 1.5x. That ceiling is per channel and no amount of horizontal scale lifts it,
		// because it is one row lock in one database.
		//
		// What the lock was incidentally buying is covered in `SetChannelLastMessage` (pointer
		// monotonicity, now GREATEST's job) and in docs/security-ledger.md (a channel-overwrite mute no
		// longer blocks an in-flight send). The permission read is unaffected either way: Authorize reads
		// guild_members, roles and permission_overwrites unlocked in *both* variants, so this lock never
		// protected rule 1's freshness.
		channel, guildID, _, err := guildauth.AuthorizeChannelUnlocked(
			ctx, q, actor, in.ChannelID, roles.PermSendMessages,
		)
		if err != nil {
			return err
		}

		// The channel row is already in hand, so the type check is free — and it has to happen somewhere,
		// because guildauth only refuses a channel belonging to no guild. See ChannelGuildText.
		if channel.Type != ChannelGuildText {
			return httpx.Errorf(httpx.ErrBadRequest, "this channel does not hold messages")
		}

		if err := checkContent(in.Content); err != nil {
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
			if vanished := channelVanished(err); vanished != nil {
				return vanished
			}
			return fmt.Errorf("messages: create message: %w", err)
		}

		if err := q.SetChannelLastMessage(ctx, db.SetChannelLastMessageParams{
			ID: int64(in.ChannelID), LastMessageID: &row.ID,
		}); err != nil {
			return fmt.Errorf("messages: set last message: %w", err)
		}

		// M16b. Nothing for the overwhelming majority of guilds, which have not opted in — see record.
		if err := s.record(
			ctx, q, guildID, actor.UserID, snowflake.ID(row.ID), AuditCreate,
		); err != nil {
			return err
		}

		out = messageFromRow(row)
		return nil
	})
	return out, err
}

// checkContent bounds a message body in the service, not only at the handler.
//
// The handler's `validate:"min=1,max=4000"` tag is what a request actually hits, and it has to be a
// literal because a struct tag cannot reference a constant. That left MaxContentLength documenting a
// bound nothing read — a code review found it inert, and the security-ledger entry recorded for this
// decision pointed at a constant that did nothing.
//
// Checking here closes that, and closes the ledger entry's own reopening condition in advance: it names
// "a write path that reaches `messages` without passing the handler's validator — a bulk import, a
// webhook ingest (M60), or a bot-automation path (M22)". Every one of those calls the service, not the
// handler. Same reasoning M14 used for the unknown audit-action filter, which is checked in the service
// and not only in the handler.
//
// **Runes, not bytes**, because that is what the handler's validator counts. go-playground/validator's
// `max` on a string is utf8.RuneCountInString, so a byte-length check here disagrees with it on every
// non-ASCII message: 4,000 Japanese characters are 12,000 bytes and 4,000 emoji are 16,000, all of which
// the handler accepts and a `len()` check refuses. The first version of this function used `len()` and
// would have rejected a perfectly ordinary message for every user not writing in a Latin script — caught
// by asking what the tag it claims to agree with actually measures, which is the whole reason the two are
// pinned to each other.
//
// TestTheHandlerTagAgreesWithTheConstant pins the literal against this constant, so they cannot drift.
func checkContent(content string) error {
	switch {
	case content == "":
		return httpx.Errorf(httpx.ErrBadRequest, "content is required")
	case utf8.RuneCountInString(content) > MaxContentLength:
		return httpx.Errorf(httpx.ErrBadRequest, "content must be at most %d characters", MaxContentLength)
	}
	return nil
}

// channelVanished maps the one error the unlocked authorize made reachable, and returns nil otherwise.
//
// Send stopped reading the channel FOR UPDATE, so a channel can be deleted between the authorization read
// and the insert. The row is gone by then and `messages_channel_id_fkey` refuses the write. That must
// reach the caller as the 404 the read would have produced a moment earlier — losing a race to a channel
// deletion is not a server error, and a 500 would also be a worse answer than the one the caller gets if
// they retry.
//
// A named function rather than an inline branch because the inline version could not be tested. Driving
// the real interleaving needs the delete to commit *inside* the service's transaction, which no test can
// arrange without a hook the service does not have — so the first attempt asserted a 404 that came from
// the authorize path instead, and passed with the mapping disabled. Testing the mapping directly is the
// honest version.
func channelVanished(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.ForeignKeyViolation &&
		pgErr.ConstraintName == "messages_channel_id_fkey" {
		return httpx.ErrNotFound
	}
	return nil
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
	// PermReadMessageHistory, not merely PermViewChannel. AuthorizeChannelUnlocked folds the view bit in
	// on top of whatever is asked for, so this is "can see the channel AND may read what was said in it" —
	// the two are separate grants, and a channel configured view+send without history is one whose earlier
	// conversation is not for whoever was added last.
	if _, _, _, err := guildauth.AuthorizeChannelUnlocked(
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
// **It requires PermSendMessages, not merely the ability to see the channel.** Editing is a write into
// the channel, and the commonest overwrite in any guild is a `muted` role denying exactly that bit — so
// authorizing an edit on visibility alone leaves a muted member a write primitive into the channel they
// were just silenced in, one rewrite per message they already posted. M13 spent two decisions keeping a
// restriction from being *shed* (assignment is escalation-checked, DeleteRole refuses to drop overwrites
// whose bits the caller lacks); this is the same restriction being walked around rather than shed, and at
// M18 each rewrite fans out a MESSAGE_UPDATE to everyone in the channel. Found by a security audit after
// the milestone's manual pass, and reproduced before it was fixed.
//
// Delete deliberately stays at the view bit. Removing your own message is redaction, which is the outcome
// a mute wants rather than one it should block.
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
		// PermSendMessages, not merely the view bit the authorize folds in. An edit is a write into this
		// channel and a mute must bound every one of them — see the paragraph above.
		//
		// Non-locking, for Send's reason. The lock this path genuinely needs is on the *message* row, and
		// loadInChannel takes it below with GetMessageForUpdate — which also closes the cascade race Send
		// has to map an FK error for: a channel deleted mid-edit cannot remove this message while that
		// row lock is held, and if it commits first the locking read simply finds nothing and answers 404.
		_, guildID, _, err := guildauth.AuthorizeChannelUnlocked(
			ctx, q, actor, in.ChannelID, roles.PermSendMessages,
		)
		if err != nil {
			return err
		}

		if err := checkContent(in.Content); err != nil {
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

		// After the update, so the recorded content is what the message now says. The version it
		// replaced is not lost — it went to message_edit_history a few lines above, which is M16a's
		// surface and a different permission. Recording the prior text here as well would put the same
		// content in two tables under two gates.
		if err := s.record(
			ctx, q, guildID, actor.UserID, snowflake.ID(updated.ID), AuditEdit,
		); err != nil {
			return err
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
		// Non-locking, for the reason Update is: loadInChannel locks the message row, which is the lock
		// this operation actually needs.
		_, guildID, decision, err := guildauth.AuthorizeChannelUnlocked(ctx, q, actor, channelID, 0)
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

		// M16b, and it records the content it is deleting.
		//
		// §2 left that open ("NULL for a delete if the notice decision lands that way") and this is the
		// answer: the create row only holds what a message said when it was posted, so a message written
		// before the switch went on and deleted after would otherwise have its content recorded nowhere —
		// which is the case an investigation is most likely to be about. The delete is soft, so the row is
		// still there to read it from.
		//
		// **Recorded for an author deleting their own message too**, unlike the audit entry below. The two
		// tables answer different questions: rule 2's log records authority exercised over somebody, and
		// deleting your own message is authority over nobody — while a guild that switched recording on
		// asked for what was said here, and "somebody removed their own message" is squarely that. A
		// recording that skipped self-deletions would be one anybody could evade by deleting their own
		// messages, which is the whole thing the guild opted in to prevent.
		if err := s.record(
			ctx, q, guildID, actor.UserID, snowflake.ID(row.ID), AuditDelete,
		); err != nil {
			return err
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

// record writes one row to a guild's message-audit log, if that guild records at all (M16b).
//
// Called from Send, Update and Delete, inside each one's existing transaction, so a mutation that commits
// without its recording row is not a state the code can reach — the property rule 2 asks of
// `audit_log_entries`, here by the same mechanism on a table rule 2 deliberately does not cover.
//
// # It reads the flag and branches, and the whole-thing-in-one-statement version was measured and dropped
//
// The plan for this milestone had no read at all: one INSERT ... SELECT joining messages to channels to
// guilds with the opt-in as a join predicate, on the argument that a guard which can be raced belongs in
// the statement. It costs 62-90% of the message insert in every guild that has *not* opted in, which is
// all of them until somebody does, against 9% for this. 000023 carries the numbers.
//
// The property that was supposed to justify it turned out to be illusory, which is the part worth
// keeping: both shapes read the flag at some instant *after* the message is written, so neither makes
// "sent while recording" well-defined. The race belongs to a switch that can be flipped mid-transaction
// and no statement shape closes it.
//
// What the statement form was genuinely buying is kept in [db.Queries.RecordMessageAudit]: the flag is
// still a join predicate on the insert that runs, so no row can exist for a guild whose flag is false
// whatever this function believes; rule 13's exclusion is still `NOT m.is_e2e` on the message row; and
// content still comes off that row rather than from a parameter, so what is recorded cannot disagree with
// what was stored.
//
// # The id is minted only if a row is going to be written
//
// It was minted before the flag was read, on every message mutation on the instance, and the comment here
// defended that on two grounds that an optimization review found were both wrong. It is not "a
// process-local counter increment": [snowflake.Generator.Next] takes a package-global mutex, reads the
// clock, and **consumes one of the 4,096 ids that node can issue in a millisecond** — past which it
// busy-waits for the clock to advance. Minting one per message mutation and discarding it in essentially
// every guild halved the id headroom of the product's highest-volume write for nothing. And "a fallible
// call in the middle of a transaction" describes what this function already is: every other call in it
// can fail the same way, in the same place.
//
// Measured at the ceiling, which is the shape that matters: 330 ns/op, zero allocations — a tight loop
// exceeds 4,096/ms, so what that number reports is `waitPast` rather than the mutex. Below the ceiling it
// is a lock and a clock read, which is small; the reason to move it is that it is free to move and it is
// on the one path rule 7 names.
func (s *Service) record(
	ctx context.Context, q *db.Queries, guildID snowflake.ID, actorID snowflake.ID,
	messageID snowflake.ID, action string,
) error {
	recording, err := q.GuildRecordsMessages(ctx, int64(guildID))
	if err != nil {
		// Not pgx.ErrNoRows-tolerant on purpose: the guild was resolved by guildauth moments ago, in this
		// transaction, so a missing row here is a real failure rather than a race to report as 404.
		return fmt.Errorf("messages: read recording flag: %w", err)
	}
	if !recording {
		return nil
	}

	id, err := s.ids.Next()
	if err != nil {
		return fmt.Errorf("messages: mint audit entry id: %w", err)
	}

	if err := q.RecordMessageAudit(ctx, db.RecordMessageAuditParams{
		ID:        int64(id),
		GuildID:   int64(guildID),
		MessageID: int64(messageID),
		ActorID:   int64(actorID),
		Action:    action,
	}); err != nil {
		return fmt.Errorf("messages: record message audit: %w", err)
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
