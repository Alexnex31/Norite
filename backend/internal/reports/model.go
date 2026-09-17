// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package reports

import (
	"time"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// Target types, as stored in reports.target_type.
//
// **Only TargetMessage is reachable at M16**, and the rest are refused at the boundary rather than
// accepted and stranded. A report whose target nothing can triage is a report filed into a queue that will
// never show it, which is M14's unknown-filter lesson pointed at a write: there, an action nobody writes
// matched nothing and read as evidence of absence; here, a target type nothing routes would read as a
// report that was made and ignored.
//
// The values are reserved rather than omitted because 000021 stores them and a later milestone must not
// renumber — the same reasoning that pins the permission bit order. TargetWhisper is M61's, and the
// channel and user cases arrive with M74's instance queue.
const (
	TargetMessage int16 = 0
	TargetWhisper int16 = 1
	TargetChannel int16 = 2
	TargetUser    int16 = 3
)

// Where a report is routed, as stored in reports.routed_to.
//
// **Computed from the resolved target, never taken from the client.** A client-settable routing lets a
// member push a report past their own guild's moderators into the instance queue, and — in the direction
// that actually matters — lets a report *about* a moderator be routed to that same moderator. M16 writes
// only RoutedToGuildModerators; RoutedToInstanceAdmins is M74's, for the targets that have no guild to
// escalate to.
const (
	RoutedToGuildModerators int16 = 0
	RoutedToInstanceAdmins  int16 = 1
)

// Report statuses, as stored in reports.status.
//
// **StatusUnderReview is reserved and unreachable at M16.** The done-when is "see and resolve", a third
// state exercises authority over nobody, and it would add an audit verb with nothing to say. M74 owns the
// richer queue and the state that goes with it — the value is held here so that milestone does not have to
// renumber the two terminal ones.
const (
	StatusOpen        int16 = 0
	StatusUnderReview int16 = 1
	StatusResolved    int16 = 2
	StatusDismissed   int16 = 3
)

// The audit vocabulary for report actions.
//
// Two verbs rather than one, and the split is M14's test for it: `overwrite.set` is a single verb because
// the endpoint does not distinguish creating from replacing, while `member.role_add` and
// `member.role_remove` are two because what an operator reads the log for is which one happened. An
// outcome is the whole content of a triage decision, so burying it in `changes` would make the common
// question a scan.
//
// **Filing writes no entry at all.** Rule 2 covers guild-scoped *administrative* mutations — a member
// filing a report exercises authority over nobody, and the moderator is the one who decides. Auditing it
// would also hand every member of every guild an unbounded write to `audit_log_entries`, which is verbatim
// the hazard M15 named when it narrowed the rule: `defaultEveryonePermissions` puts the filing path within
// reach of @everyone.
//
// Written by this package and validated by the reader in `guilds`, which cannot import it. The two sides
// are pinned equal by a test in cmd/server — the shape M15 established for `message.delete`.
const (
	ActionReportResolve = "report.resolve"
	ActionReportDismiss = "report.dismiss"
)

// Reason categories a report may carry, and the vocabulary is closed.
//
// Refused rather than stored as free text, for the reason the audit log refuses an unknown action filter:
// a category nothing recognizes sorts into no bucket in any triage view, so a typo becomes a report
// nobody sees. The free text a reporter writes goes in `detail`, which is what that field is for.
var reasonCategories = []string{
	"spam",
	"harassment",
	"hate_speech",
	"violence",
	"nsfw",
	"self_harm",
	"illegal",
	"other",
}

// ReasonCategories returns every category this build accepts.
//
// A copy, for the reason guilds.AuditActions returns one: an exported slice is not a closed vocabulary,
// and an importer that appended to or nilled it would change what the endpoint accepts process-wide.
func ReasonCategories() []string {
	out := make([]string, len(reasonCategories))
	copy(out, reasonCategories)
	return out
}

func knownReasonCategory(c string) bool {
	for _, known := range reasonCategories {
		if c == known {
			return true
		}
	}
	return false
}

// MaxDetailLength bounds the reporter's free text, counted in runes.
//
// Runes rather than bytes, and the handler's `max` tag is pinned to this constant by a test. M15 shipped
// the same bound measured two ways — a byte check in the service against go-playground/validator's rune
// count at the handler — which would have refused any near-limit message in a non-Latin script.
const MaxDetailLength = 2000

// Page bounds for the triage queue, matching the audit log's for the reason that one states: 100 is what
// every comparable API uses, so a client written against one of them paginates correctly here.
const (
	defaultPageSize = 50
	maxPageSize     = 100
)

// Report is one report as the API returns it.
//
// # The reporter is deliberately absent, and this is the milestone's other disclosure decision
//
// There is no `reporter_id` on this struct and no query in this package returns one to a guild moderator.
// A guild moderator is not a vetted actor, and a report system has to survive the case where the moderator
// *is* the person being reported — §2's account-export asymmetry already encodes that judgement, including
// reports a user filed and excluding reports filed against them, to protect reporters from retaliation.
// Handing the identity to whoever holds PermManageMessages in the guild the report is about undoes that at
// the one place it matters most.
//
// The column still exists: M74's Instance Admin triage reads it, the partial unique index dedupes on it,
// and the account export returns it to its owner. What is decided here is only who the *guild* surface
// tells.
//
// The cost is real and is named in the ledger: a guild moderator cannot see that five reports came from
// one person. 000021's dedupe index bounds that to one open report per reporter per target, which is the
// half that matters, and M74 owns reporter-history triage by name.
//
// Adding the field later is additive; removing it once clients read it is not, so withholding is the
// conservative direction of a one-way door.
//
// Ids marshal as quoted strings (ADR 0003): 63 bits does not survive a float64.
type Report struct {
	ID             snowflake.ID  `json:"id"`
	GuildID        *snowflake.ID `json:"guild_id"`
	TargetType     string        `json:"target_type"`
	TargetID       snowflake.ID  `json:"target_id"`
	ReasonCategory string        `json:"reason_category"`
	Detail         *string       `json:"detail"`
	Status         string        `json:"status"`
	ResolvedBy     *snowflake.ID `json:"resolved_by"`
	CreatedAt      time.Time     `json:"created_at"`
	ResolvedAt     *time.Time    `json:"resolved_at"`
}

// TriageEntry is one row of a moderator's queue: a report plus two facts about its target.
//
// **No content**, deliberately — see the listing queries in reports.sql. The two facts are what lets a
// queue badge a row without disclosing anything, and the reported text lives behind [Service.Get] alone.
//
// TargetIsE2E is a pointer because it carries three states rather than two: false for an ordinary target,
// true for one whose content rule 13 withholds, and **nil when the target no longer resolves at all**.
// Collapsing the last into false would report a deleted-and-gone target as readable.
type TriageEntry struct {
	Report
	TargetIsE2E     *bool      `json:"target_is_e2e"`
	TargetDeletedAt *time.Time `json:"target_deleted_at"`
}

// TriageDetail is one report with the reported message attached.
//
// This is the only place in M16 that returns content, which is what keeps rule 13's exclusion to a single
// place that has to be right. TargetContent is nil when the target does not resolve *and* when it is
// E2E-encrypted; TargetIsE2E is what tells those apart.
type TriageDetail struct {
	TriageEntry
	TargetContent   *string       `json:"target_content"`
	TargetChannelID *snowflake.ID `json:"target_channel_id"`
	TargetAuthorID  *snowflake.ID `json:"target_author_id"`
}

// targetTypeName renders a stored target type for the wire.
//
// A string rather than the stored smallint, unlike messages.Type, and the difference is what a client does
// with it. A message type is an open, growing vocabulary a client passes through; a report's target type
// is a closed set a client must branch on to know what `target_id` even names. The audit log's `action`
// made the same call for the same reason.
func targetTypeName(t int16) (string, bool) {
	switch t {
	case TargetMessage:
		return "message", true
	case TargetWhisper:
		return "whisper", true
	case TargetChannel:
		return "channel", true
	case TargetUser:
		return "user", true
	}
	return "", false
}

// targetTypeValue is targetTypeName's inverse, over the names a client may send.
//
// Only "message" resolves. The other three are refused by [Service.File] with their own message, so a
// client learns the type is not yet accepted rather than that it does not exist.
func targetTypeValue(name string) (int16, bool) {
	switch name {
	case "message":
		return TargetMessage, true
	case "whisper":
		return TargetWhisper, true
	case "channel":
		return TargetChannel, true
	case "user":
		return TargetUser, true
	}
	return 0, false
}

func statusName(s int16) (string, bool) {
	switch s {
	case StatusOpen:
		return "open", true
	case StatusUnderReview:
		return "under_review", true
	case StatusResolved:
		return "resolved", true
	case StatusDismissed:
		return "dismissed", true
	}
	return "", false
}

// statusValue resolves a status a client may send as a filter or as a resolution outcome.
//
// "under_review" resolves here because it is a real stored value, and is refused by the two callers that
// must not accept it — see [Service.Resolve] and the listing's filter. Refusing it in one place instead
// would make the reserved value unnameable, and a milestone adding it would find the vocabulary silently
// missing a state the database already had.
func statusValue(name string) (int16, bool) {
	switch name {
	case "open":
		return StatusOpen, true
	case "under_review":
		return StatusUnderReview, true
	case "resolved":
		return StatusResolved, true
	case "dismissed":
		return StatusDismissed, true
	}
	return 0, false
}

// reportFromRow converts a stored row to the wire shape, dropping the reporter.
//
// The drop is structural rather than remembered: every path out of this package builds its response here,
// and the field is absent from [Report] entirely, so there is nothing for a handler to forget to strip.
func reportFromRow(row db.Report) (Report, error) {
	target, ok := targetTypeName(row.TargetType)
	if !ok {
		return Report{}, errUnknownStoredValue("target_type", row.TargetType)
	}
	status, ok := statusName(row.Status)
	if !ok {
		return Report{}, errUnknownStoredValue("status", row.Status)
	}

	out := Report{
		ID:             snowflake.ID(row.ID),
		TargetType:     target,
		TargetID:       snowflake.ID(row.TargetID),
		ReasonCategory: row.ReasonCategory,
		Detail:         row.Detail,
		Status:         status,
		CreatedAt:      row.CreatedAt.Time,
	}
	if row.GuildID != nil {
		id := snowflake.ID(*row.GuildID)
		out.GuildID = &id
	}
	if row.ResolvedBy != nil {
		id := snowflake.ID(*row.ResolvedBy)
		out.ResolvedBy = &id
	}
	if row.ResolvedAt.Valid {
		t := row.ResolvedAt.Time
		out.ResolvedAt = &t
	}
	return out, nil
}
