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

	// Tags are the tags on the message this caller can see (M17): every shared one, plus their own
	// private ones — somebody else's private tag never appears.
	//
	// **Null, not empty, when the credential may not read tags.** An API token without `tags.read` gets
	// null, because an empty array would say "this message has no tags" when the truth is "that is not
	// this credential's to see" — a scope bounds what a delegated credential reaches, and `messages.read`
	// reaching tags would widen it. A user actor passes every scope and always gets an array.
	//
	// Resolved for a whole page in one statement (see Service.attachTags), never once per message —
	// fetching them per message is what M17's optimization review found the API forcing on every client.
	Tags []AppliedTag `json:"tags"`
}

// AppliedTag is a tag as it appears on a message, and it is the same wire shape as tags.AppliedTag.
//
// Duplicated rather than imported: `messages` and `tags` both reach `guildauth` and neither may import
// the other, which is the M15 extraction's whole point. The two are one schema in the contract
// (AppliedMessageTag), `contract_payload_test.go` validates responses from both packages against it, and
// `TestTheAppliedTagShapeAgreesAcrossPackages` in cmd/server pins the JSON field sets equal — the
// literal-plus-pin shape this codebase uses for every value written in two packages.
type AppliedTag struct {
	ID        snowflake.ID `json:"id"`
	GuildID   snowflake.ID `json:"guild_id"`
	Name      string       `json:"name"`
	CreatedBy snowflake.ID `json:"created_by"`
	IsShared  bool         `json:"is_shared"`
	CreatedAt time.Time    `json:"created_at"`
	AppliedBy snowflake.ID `json:"applied_by"`
	AppliedAt time.Time    `json:"applied_at"`
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

// The recording vocabulary (M16b), and it is a *different* vocabulary from the one above.
//
// These three are written to `message_audit_entries` and never to `audit_log_entries`. That is not merely
// a different table — it is a different question, asked by a different reader, under a different
// permission, in guilds that opted in. The audit log answers "what did somebody with authority do here";
// this answers "what was said here", for a guild that decided it wants that kept.
//
// **They must never be added to `guilds.AuditActions()`**, and the temptation is real, because that slice
// already carries `message.delete` as a literal for the cross-package reason. Two things would break.
// That slice is what `GET /guilds/{id}/audit-log` validates an `action` filter against, so a verb in it
// that the table never holds makes the filter *validate* and match nothing — which is M14's "an unknown
// filter value is refused, not answered with an empty page" inverted into a typo that reads as evidence
// of absence. And `TestTheOnlyMessageVerbIsDeleteAndItCarriesNoContent` iterates that slice and fails on
// any `message.*` verb other than delete, with instructions to re-reason about rule 13 before changing
// it: adding these and then editing that test to allow them would disarm the one guard M14 built for
// exactly this milestone.
//
// TestTheMessageAuditVerbsAreNotGuildAuditVerbs in `cmd/server` pins the two vocabularies **disjoint**.
// That is the same mechanism as the M15 and M16 cross-package pins with the assertion inverted — they
// hold two literals equal, this one holds two sets apart.
//
// Bare verbs rather than `message.create` and so on: the table is already about messages, and the prefix
// would only invite somebody to grep for it and find the guild vocabulary.
const (
	AuditCreate = "create"
	AuditEdit   = "edit"
	AuditDelete = "delete"
)

// MessageAuditActions is every action the recording log records.
//
// A copy, for the reason guilds.AuditActions returns one: an exported slice is not a closed vocabulary,
// and the cross-package test that pins these disjoint from the guild verbs would otherwise be asserting
// something any importer could change.
func MessageAuditActions() []string { return []string{AuditCreate, AuditEdit, AuditDelete} }

// ChannelGuildText is the one channel type that holds messages, and it is guilds.ChannelGuildText.
//
// # Why the value is duplicated rather than imported
//
// `messages` may not import `guilds` — that is what the M15 chokepoint extraction was for, and importing
// it back would make `guilds` the import root of every domain that acts inside a channel. So the value is
// a literal here, exactly as the `message.delete` verb is a literal on the `guilds` side, and the pin that
// stops the two drifting is a test in `cmd/server`, which imports both:
// TestTheTextChannelTypeAgreesAcrossPackages.
//
// # Why the check exists at all
//
// guildauth.guildOf refuses a channel belonging to no guild, which rules out DMs and group DMs. It says
// nothing about the rest of the vocabulary, and `guilds.isGuildChannelType` is consulted only when a
// channel is *created* — so before this constant a member holding view+send on a category could post into
// it, read it back, and advance its `last_message_id`. Reproduced on all three of GUILD_CATEGORY,
// GUILD_VOICE and the reserved GUILD_ANNOUNCEMENT during M15's security audit.
//
// That is not an authorization bypass: the caller genuinely holds the bits on that row. It is a place to
// park content no client renders — no screen in docs/design/tui/SCREENS.md draws a category's backlog —
// which makes it invisible to moderation while remaining served by the API.
//
// # Text only, deliberately, and it is the reversible direction
//
// Discord accepts text in voice channels and this may want to later. Allowing a type later is additive;
// disallowing one later strands whatever was already stored in it. So the narrow answer goes in first and
// widening it stays a decision somebody makes on purpose. GUILD_ANNOUNCEMENT and GUILD_STAGE_VOICE are
// reserved (rule 10) and unbuildable today — this is what stops them becoming message-bearing by default
// on the milestone that finally creates one.
//
// Only Send is gated. List, Update and Delete still work on anything already stored, so content that
// predates this check stays readable and — more to the point — removable.
const ChannelGuildText int16 = 0
