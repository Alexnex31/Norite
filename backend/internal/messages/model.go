// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package messages

import (
	"time"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// MaxContentLength bounds a message body, at the handler rather than in the schema.
//
// `messages.content` is bare `text` with no CHECK, matching `channels.topic`, which is bare `text` with a
// `max=1024` validator tag. The schema has no length constraint anywhere and this is not the milestone to
// introduce the first one: raising a message-length cap is a product decision, and behind a constraint it
// becomes an ALTER TABLE validation scan over the largest table in the product. Recorded in
// `docs/security-ledger.md` with the condition that would reopen it — a write path that reaches `messages`
// without passing this validator, which webhooks (M60) and bot automation (M22) both are.
const MaxContentLength = 4000

// defaultPageSize and maxPageSize bound the backlog read.
//
// A cap rather than an unbounded list, for rule 21's reason as much as §15's: this is the first endpoint a
// web client will call in a loop, and an uncapped page is a payload that scales with how long a channel
// has existed.
const (
	defaultPageSize = 50
	maxPageSize     = 100
)

// Message is one message as the API returns it.
//
// Ids marshal as quoted strings (ADR 0003) — a snowflake is 63 bits and a browser's number is a float64,
// so an unquoted id silently loses precision above 2^53.
type Message struct {
	ID        snowflake.ID  `json:"id"`
	ChannelID snowflake.ID  `json:"channel_id"`
	AuthorID  *snowflake.ID `json:"author_id"`
	Content   string        `json:"content"`
	Type      int16         `json:"type"`
	ReplyToID *snowflake.ID `json:"reply_to_id"`
	EditedAt  *time.Time    `json:"edited_at"`
	CreatedAt time.Time     `json:"created_at"`
}

// messageFromRow converts a stored row to the wire shape.
//
// `is_e2e` and `deleted_at` are deliberately not on the wire. The first is server-side bookkeeping that
// nothing sets until M97 and no client may choose — a client-settable flag would let a guild message
// claim an encryption the instance is not providing. The second never reaches a caller here because every
// read filters it, and M16's moderation surface will decide its own shape for the rows it can see.
func messageFromRow(row db.Message) Message {
	m := Message{
		ID:        snowflake.ID(row.ID),
		ChannelID: snowflake.ID(row.ChannelID),
		Content:   row.Content,
		Type:      row.Type,
		CreatedAt: row.CreatedAt.Time,
	}
	if row.AuthorID != nil {
		id := snowflake.ID(*row.AuthorID)
		m.AuthorID = &id
	}
	if row.ReplyToID != nil {
		id := snowflake.ID(*row.ReplyToID)
		m.ReplyToID = &id
	}
	if row.EditedAt.Valid {
		t := row.EditedAt.Time
		m.EditedAt = &t
	}
	return m
}

// The audit vocabulary for message actions, and it is deliberately short.
//
// Rule 2 was narrowed at M15 to guild-scoped *administrative* mutations: a mutation is administrative when
// it changes what the guild is, or exercises authority over somebody else. A member posting, editing or
// deleting their own message does neither, so none of those writes an entry — auditing them would double
// the write volume of the hottest path in the product and bury the moderation signal M14 built a reader
// for, and `defaultEveryonePermissions` grants PermSendMessages to @everyone, so it would also hand every
// member an unbounded write to the audit log.
//
// A moderator deleting somebody *else's* message is authority exercised over a person, so it is audited.
// There is no `message.create` or `message.edit` verb at all: an author cannot exercise authority over
// themselves, and a moderator cannot edit somebody else's words in the first place (see UpdateMessage).
//
// Guilds that want every message recorded opt in at M16b, which writes to `message_audit_entries` — a
// table of its own, never this one.
const ActionMessageDelete = "message.delete"
