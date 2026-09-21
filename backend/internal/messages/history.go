// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package messages

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/guildauth"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// MessageVersion is one prior version of a message.
//
// There is no editor field and the table could not carry one: only a message's author may edit it (see
// [Service.Update]), so the editor is always the author already named on the envelope. A future
// moderator-edit feature would owe the column — and there is deliberately no such feature, because
// rewriting somebody's words puts them in their mouth under their name.
type MessageVersion struct {
	ID       snowflake.ID `json:"id"`
	Content  string       `json:"content"`
	EditedAt time.Time    `json:"edited_at"`
}

// MessageHistory is a message's prior versions plus what it says now.
//
// # Why the current version is here at all
//
// There is no single-message GET anywhere in this API — `/channels/{channel_id}/messages/{message_id}`
// serves PATCH and DELETE only, and the sole read is the paginated backlog behind PermReadMessageHistory.
// So without CurrentContent a moderator reads every version a message used to have and has no route that
// will tell them what it says today, which is the question a report is actually about. M16's report detail
// resolves target content at read time for the same reason.
//
// # Three states, and the third is why IsE2E is on the envelope
//
// CurrentContent is nil and IsE2E true for an encrypted message, whose Versions is empty as well: rule 13's
// exclusion lives in the SQL, so an encrypted target yields no readable rows rather than rows the mapper
// had to remember to blank. CurrentContent is non-nil and IsE2E false for an ordinary one. There is no
// vanished-target state here, unlike a report's: the message *is* the object being addressed, so one that
// does not resolve is a 404 rather than a row with holes in it.
type MessageHistory struct {
	MessageID snowflake.ID  `json:"message_id"`
	ChannelID snowflake.ID  `json:"channel_id"`
	AuthorID  *snowflake.ID `json:"author_id"`
	IsE2E     bool          `json:"is_e2e"`
	DeletedAt *time.Time    `json:"deleted_at"`
	EditedAt  *time.Time    `json:"edited_at"`
	// CurrentContent is nil exactly when IsE2E is true. See the type comment.
	CurrentContent *string          `json:"current_content"`
	Versions       []MessageVersion `json:"versions"`
}

// HistoryInput is a request for one message's edit history.
type HistoryInput struct {
	ChannelID snowflake.ID
	MessageID snowflake.ID
	Before    *snowflake.ID
	Limit     int32
}

// History returns a message's prior versions, newest first.
//
// # The permission, and the one departure it needs
//
// PermManageMessages, which M16 established as the gate for message-scoped moderation reads — this reuses
// it rather than inventing a bit. The departure is that it authorizes through
// [guildauth.AuthorizeChannelIgnoringVisibility] rather than the entry point every other operation in this
// package uses: M16 settled that a moderator reads a reported message's content whether or not they can
// currently view the channel it came from, and this is the same content reached by the same moderator one
// step later. Folding the view bit in would mean a moderator could read what a reported message says now
// and not what it said before, inside the flow this milestone exists to serve.
//
// What that entry point does *not* lift is the refusal downgrade: somebody who fails here and also cannot
// view the channel is still answered 404 rather than 403, so nothing about a hidden channel's existence
// leaks to anyone who could not already see it.
//
// # The author reads their own, and it is a carve-out rather than a second permission
//
// Refusing somebody the prior versions of their own words is hard to defend, and it discloses nothing to
// anyone new — they wrote the text. It is checked after the row is loaded, which makes it an authorization
// question rather than an input one (M12's "refuse before explaining"): somebody who cannot reach the
// channel at all never gets here, and the branch only ever *widens* who may read, so a bug in it cannot
// turn into a disclosure to a stranger.
//
// The ordering matters for a subtler reason too. Authorizing first and then loading means the 404 for a
// message in another channel is reached by everyone identically, rather than authors learning something
// moderators do not.
//
// # Rule 13
//
// Satisfied in the SQL, twice, and tested rather than argued. E2E is DM-only, a DM has no guild, and this
// route is guild-scoped, so an encrypted message is unreachable here by shape — which is exactly the claim
// M11a made about password reset not bypassing the second factor, and M11a's lesson is that such a claim
// stops being true quietly. So the exclusion is real and there is a test that fails if the shape argument
// ever stops holding.
func (s *Service) History(ctx context.Context, actor auth.Actor, in HistoryInput) (MessageHistory, error) {
	// Not AuthorizeChannelUnlocked. See the doc comment — and note this reads on the pool rather than in a
	// transaction because nothing here writes: rule 1 asks for data freshly loaded for the channel in the
	// path, which this is, and there is no later write for a concurrent demotion to slip in front of.
	//
	// Asked at need 0, which is a membership check: a non-member is refused 404 before any message row is
	// read, and the Decision that comes back already carries this caller's permissions *resolved in this
	// channel*, since Authorize was given the channel id. So the bit is tested below without a second
	// resolution — the author carve-out needs the message row to decide, and loading it first would mean
	// authorizing after reading.
	_, _, decision, err := guildauth.AuthorizeChannelIgnoringVisibility(
		ctx, s.queries, actor, in.ChannelID, 0,
	)
	if err != nil {
		return MessageHistory{}, err
	}

	// # Both reads take one snapshot, and the reason is a duplicate rather than a torn page
	//
	// The envelope and the version list are two statements. Under READ COMMITTED each takes its own
	// snapshot, so an edit committing between them is visible to the second and not the first: the
	// envelope still says the message reads "B" while the version list already carries the "B" that the
	// edit just displaced. The moderator is then shown the same text as the current version *and* as the
	// most recent prior one, and never sees what it actually says now. That reads as a bug in the writer,
	// which is the worst kind of wrong answer for a moderation surface — it invites somebody to go looking
	// at Update for a double-append that is not there.
	//
	// Reading them in the other order is not a fix, it is a worse one: the envelope would carry "C" and
	// the version list would omit "B" entirely, so a moderation view would silently drop a version. A
	// duplicate is visible; a gap is not.
	//
	// So: one REPEATABLE READ snapshot, read-only. Both statements then see the same instant and the two
	// halves of the response agree. This is a moderation read rather than a hot path, so a transaction
	// here costs a connection for two indexed lookups — note this is *not* the rule 1 question the
	// authorize above answers, which is about resolving permissions against fresh data. Read consistency
	// across two statements is a different property and neither implies the other.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return MessageHistory{}, fmt.Errorf("messages: begin history read: %w", err)
	}
	// Read-only and never committed: rolling back releases the snapshot, and there is nothing to persist.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := s.queries.WithTx(tx)

	row, err := q.GetMessageWithHistoryTarget(ctx, int64(in.MessageID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return MessageHistory{}, httpx.ErrNotFound
		}
		return MessageHistory{}, fmt.Errorf("messages: get message for history: %w", err)
	}

	// The channel in the route is what was authorized, so a message id from elsewhere must not be reachable
	// through it — loadInChannel's rule, and 404 rather than 403 for its reason.
	if snowflake.ID(row.ChannelID) != in.ChannelID {
		return MessageHistory{}, httpx.ErrNotFound
	}

	// The permission question, asked once the row exists so the author carve-out can be applied to it. A
	// moderator passes on the bit, which the Decision above already answers; the author passes on being the
	// author.
	isAuthor := row.AuthorID != nil && snowflake.ID(*row.AuthorID) == actor.UserID
	if !isAuthor && !decision.Allows(roles.PermManageMessages) {
		// Refusing, and the *code* is re-derived by asking guildauth for the bit rather than written here.
		// A caller who lacks it and also cannot view this channel must get 404 and not 403, or the answer
		// becomes a probe for hidden channels — and that downgrade belongs to the one function that owns
		// it. This costs a second resolution on the refusal path only, which is not a path under load.
		_, _, _, refusal := guildauth.AuthorizeChannelIgnoringVisibility(
			ctx, s.queries, actor, in.ChannelID, roles.PermManageMessages,
		)
		if refusal == nil {
			// Unreachable: Decision.Allows and Authorize test the same bits against the same resolution.
			// Refusing anyway rather than falling through, because the alternative to an impossible state
			// is not an open door.
			return MessageHistory{}, httpx.ErrForbidden
		}
		return MessageHistory{}, refusal
	}

	limit := in.Limit
	switch {
	case limit <= 0:
		limit = defaultPageSize
	case limit > maxPageSize:
		limit = maxPageSize
	}

	versions, err := q.ListMessageEditHistory(ctx, db.ListMessageEditHistoryParams{
		MessageID: row.ID,
		Before:    idOrNil(in.Before),
		Limit:     limit,
	})
	if err != nil {
		return MessageHistory{}, fmt.Errorf("messages: list edit history: %w", err)
	}

	return historyFromRows(row, versions), nil
}

// historyFromRows builds the wire shape. Separate from History so the mapping is testable without a
// database, which is what caught the three-state question being collapsed into two at M16.
func historyFromRows(
	row db.GetMessageWithHistoryTargetRow, versions []db.ListMessageEditHistoryRow,
) MessageHistory {
	out := MessageHistory{
		MessageID:      snowflake.ID(row.ID),
		ChannelID:      snowflake.ID(row.ChannelID),
		IsE2E:          row.IsE2e,
		CurrentContent: row.CurrentContent,
		Versions:       make([]MessageVersion, 0, len(versions)),
	}
	if row.AuthorID != nil {
		id := snowflake.ID(*row.AuthorID)
		out.AuthorID = &id
	}
	if row.DeletedAt.Valid {
		t := row.DeletedAt.Time
		out.DeletedAt = &t
	}
	if row.EditedAt.Valid {
		t := row.EditedAt.Time
		out.EditedAt = &t
	}
	for _, v := range versions {
		out.Versions = append(out.Versions, MessageVersion{
			ID:       snowflake.ID(v.ID),
			Content:  v.Content,
			EditedAt: v.EditedAt.Time,
		})
	}
	return out
}
