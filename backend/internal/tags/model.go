// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package tags

import (
	"time"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// Ceilings, and they exist because M12 settled that a list which cannot be paginated is bounded at
// creation instead — the channel and role ceilings are the same decision.
//
// [ListGuildMessageTags] returns a guild's whole tag set with no cursor, deliberately: a client needs all
// of them to render a picker, and paginating would make it fetch in a loop (rule 21's chattiness). So the
// bound has to live at the other end.
//
// **The private ceiling is the one that is load-bearing rather than tidy.** Creating a shared tag needs
// PermManageMessages, which is granted to nobody by default, so that side is already gated. A private tag
// needs nothing beyond membership — `defaultEveryonePermissions` puts every member in reach of it — so
// without a per-member cap any member of any guild could write rows without limit, which is the exact
// hazard rule 2's M15 narrowing was about on `audit_log_entries`. Bounded per member rather than per
// guild, or one member could exhaust the guild's allowance and lock everybody else out of a feature that
// needs no permission.
//
// Both are checked *after* authorization, or "is this guild full" becomes something a non-member can
// measure — M12's correction.
const (
	MaxSharedTagsPerGuild   = 200
	MaxPrivateTagsPerMember = 100
)

// MaxTagNameLength bounds a tag name, and it is the varchar(50) in 000024 rather than a number chosen
// here — checked in the service as well as at the handler for the reason `checkContent` is: a write path
// that reaches this package without passing the handler's validator (a bulk import, M22's automation)
// would otherwise meet the database's own limit as a 500.
//
// Counted in runes, not bytes, because that is what go-playground/validator's `max` counts. M15 shipped a
// byte-based check against a rune-based tag and would have refused any near-limit message in a non-Latin
// script; a 50-character limit is far easier to reach in Japanese than in English, so the same mistake
// here would bite sooner.
const MaxTagNameLength = 50

// Tag is the wire representation of a message tag.
//
// `created_by` is present on every tag, shared and private alike. For a shared tag it is provenance — who
// added this to the guild's vocabulary — and for a private one it is always the caller, since a private
// tag reaches nobody else (see the visibility filter in ListGuildMessageTags).
type Tag struct {
	ID        snowflake.ID `json:"id"`
	GuildID   snowflake.ID `json:"guild_id"`
	Name      string       `json:"name"`
	CreatedBy snowflake.ID `json:"created_by"`
	IsShared  bool         `json:"is_shared"`
	CreatedAt time.Time    `json:"created_at"`
}

func tagFromRow(row db.MessageTag) Tag {
	// Field by field rather than a struct conversion, the choice every other model in this codebase makes:
	// a conversion compiles only while the two types keep identical fields in identical order, and starts
	// silently mis-assigning the moment either gains one.
	return Tag{
		ID:        snowflake.ID(row.ID),
		GuildID:   snowflake.ID(row.GuildID),
		Name:      row.Name,
		CreatedBy: snowflake.ID(row.CreatedBy),
		IsShared:  row.IsShared,
		CreatedAt: row.CreatedAt.Time,
	}
}

// AppliedTag is a tag as it appears on a message: the tag itself plus who put it there.
//
// `applied_by` is deliberately present. A tag on a message is an assertion somebody made about it, and a
// reader deciding whether to trust "spam" wants to know who said so — the same reason the audit log names
// an actor. It is not a disclosure: everybody named here is a member of a guild the reader is also in.
type AppliedTag struct {
	Tag
	AppliedBy snowflake.ID `json:"applied_by"`
	AppliedAt time.Time    `json:"applied_at"`
}

// The two verbs this package writes to `audit_log_entries`, and when (rule 2).
//
// Tagging is mostly outside rule 2: applying a tag, or removing one you applied, exercises authority over
// nobody, the way sending a message does. Two acts are over somebody else, and those are these verbs:
//
//   - `tag.remove`: taking an application off a message when somebody *else* applied it. After M17's
//     sweep that is only ever a PermManageMessages holder, since a private tag can be applied by nobody
//     but its owner.
//   - `tag.delete`: deleting a shared tag while other people's applications of it exist, because the
//     cascade takes their labels with it. A shared tag nobody else has used is vocabulary and nothing
//     more, and its deletion is recorded no more than its creation is (docs/security-ledger.md).
//
// Neither payload carries message content: ids, the tag's name and a count. Rule 13 is satisfied the way
// messages.writeModerationAudit satisfies it, by there being nothing to exclude.
//
// Written here and validated by the reader in `guilds`, which cannot import this package. The two sides
// are pinned equal by a test in cmd/server, the shape M15 established for `message.delete`.
const (
	ActionTagRemove = "tag.remove"
	ActionTagDelete = "tag.delete"
)
