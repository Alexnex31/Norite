// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package reports is the guild-scoped half of the reports system: filing one, and the moderator queue
// that triages it.
//
// Authority is decided by [guildauth] and nowhere else — the chokepoint M12 built and M15 extracted so a
// second package could reach it. `reports` is one of the four packages named in that extraction's own
// comment, and it is the second to arrive.
//
// # What this package is not
//
// The instance-scoped half is M74's: whisper, plain-DM and Group-DM reports, none of which have a guild
// owner to escalate to, plus the break-glass rules for reading whisper content. Everything here assumes a
// target that resolves to a guild channel, and refuses one that does not.
package reports

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/guildauth"
	"github.com/Alexnex31/Norite/backend/internal/platform/database"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// Service owns the report operations.
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
		return nil, errors.New("reports: a database pool is required")
	case opts.IDs == nil:
		return nil, errors.New("reports: an ID generator is required")
	}
	return &Service{pool: opts.Pool, queries: db.New(opts.Pool), ids: opts.IDs}, nil
}

func (s *Service) inTx(ctx context.Context, fn func(q *db.Queries) error) error {
	return database.RunInTx(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(s.queries.WithTx(tx))
	})
}

// errUnknownStoredValue reports a column holding a value this build has no name for.
//
// A 500 rather than a rendered number, because the alternative is mislabeling: a status this build cannot
// name is one a newer binary wrote, and guessing at it in a moderation surface is worse than failing. It
// is unreachable today — the vocabularies are closed and this package writes every value — which is why it
// exists as a named error rather than as a silent default branch.
func errUnknownStoredValue(column string, value int16) error {
	return fmt.Errorf("reports: %s holds unknown value %d", column, value)
}

// FileInput is a request to report something.
type FileInput struct {
	TargetType     string
	TargetID       snowflake.ID
	ReasonCategory string
	Detail         *string
}

// File records a report against a message.
//
// # What a filer must already be able to see
//
// The target is resolved to its channel and authorized with [guildauth.AuthorizeChannelUnlocked] at a
// `need` of zero, which folds in PermViewChannel and nothing else. **Not PermReadMessageHistory**, which
// [messages.Service.List] requires: a member without the history bit still watches live messages arrive
// once M18 fans them out, and a design where somebody can see abuse and cannot report it is the worse
// failure. What they cannot do is report a message in a channel they cannot see — that refusal is the
// channel filter's existing 404, so filing discloses nothing the listing does not.
//
// A DM is refused here by shape rather than by a check: guildauth.guildOf rejects a channel with a NULL
// guild_id, which is every DM and group DM. Those are M74's, and they are exactly the targets with no
// guild to escalate to.
//
// # A deleted message is still reportable
//
// [db.Queries.GetMessageForReport] deliberately does not filter `deleted_at`. A message removed seconds
// after it was posted is the ordinary case for a report, and refusing to file against one would make
// deleting quickly the way to dodge being reported. 000020 made the delete soft for this.
//
// # No audit entry
//
// Rule 2 covers administrative mutations, and a member filing a report exercises authority over nobody —
// the moderator is the one who decides. See the verb constants for the rest, including why auditing this
// would hand @everyone an unbounded write to the audit log.
func (s *Service) File(ctx context.Context, actor auth.Actor, in FileInput) (Report, error) {
	// The vocabulary checks live in the service and not only in the handler, for the reason
	// guilds.ListAuditLog validates its action filter here: a second caller — the TUI's M-x surface, a bot
	// over local IPC, a test — would otherwise reach the insert with a value no triage view can render.
	// Above the authorization check because they are questions about the request and disclose nothing; the
	// vocabularies are this codebase's own and identical on every instance.
	targetType, ok := targetTypeValue(in.TargetType)
	if !ok {
		return Report{}, httpx.Errorf(httpx.ErrBadRequest, "target_type is not a kind of thing that can be reported")
	}
	if targetType != TargetMessage {
		// Named distinctly from the unknown case: the type exists in the vocabulary and is not accepted
		// yet, which is a different thing for a client to learn than a typo.
		return Report{}, httpx.Errorf(httpx.ErrBadRequest,
			"this instance does not yet accept reports against a %s", in.TargetType)
	}
	if !knownReasonCategory(in.ReasonCategory) {
		return Report{}, httpx.Errorf(httpx.ErrBadRequest, "reason_category is not a category this instance accepts")
	}
	if in.Detail != nil && utf8.RuneCountInString(*in.Detail) > MaxDetailLength {
		return Report{}, httpx.Errorf(httpx.ErrBadRequest, "detail must be at most %d characters", MaxDetailLength)
	}

	id, err := s.ids.Next()
	if err != nil {
		return Report{}, fmt.Errorf("reports: mint report id: %w", err)
	}

	var out Report
	err = s.inTx(ctx, func(q *db.Queries) error {
		message, err := q.GetMessageForReport(ctx, int64(in.TargetID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// 404 and nothing more. Whether an id names a message anywhere on the instance is not
				// something a caller who cannot see it should learn — M15's loadInChannel reasoning, and
				// the same answer the next branch gives for a channel they cannot view.
				return httpx.ErrNotFound
			}
			return fmt.Errorf("reports: get target message: %w", err)
		}

		// Non-locking: this reads two fields off the channel row and writes to `reports`, which is exactly
		// the test guildauth states for which entry point to use. Nothing here describes the channel.
		_, guildID, _, err := guildauth.AuthorizeChannelUnlocked(
			ctx, q, actor, snowflake.ID(message.ChannelID), 0,
		)
		if err != nil {
			return err
		}

		guild := int64(guildID)
		detail := in.Detail
		row, err := q.CreateReport(ctx, db.CreateReportParams{
			ID:             int64(id),
			ReporterID:     int64(actor.UserID),
			TargetType:     targetType,
			TargetID:       int64(in.TargetID),
			GuildID:        &guild,
			ReasonCategory: in.ReasonCategory,
			Detail:         detail,
			// Computed, never client-supplied. A guild channel's report goes to that guild's moderators;
			// the targets that route to instance admins are the ones with no guild, and this path has
			// already refused those.
			RoutedTo: RoutedToGuildModerators,
		})
		if err != nil {
			if dup := duplicateOpenReport(err); dup != nil {
				return dup
			}
			return fmt.Errorf("reports: create report: %w", err)
		}

		out, err = reportFromRow(row)
		return err
	})
	return out, err
}

// duplicateOpenReport maps the partial unique index to a 409, and returns nil otherwise.
//
// The guard is 000021's `reports_open_target_per_reporter_idx` rather than a read-then-insert, for the
// reason M10 rewrote invite redemption that way: as a check-then-act, four of four concurrent racers got
// in. So the only place the collision can be detected is here, on the error the index raises.
//
// 409 discloses nothing the caller does not already know — it is their own outstanding report.
func duplicateOpenReport(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation &&
		pgErr.ConstraintName == "reports_open_target_per_reporter_idx" {
		return httpx.Errorf(httpx.ErrConflict, "you already have an open report against this")
	}
	return nil
}

// ListInput is a request for one page of a guild's triage queue.
type ListInput struct {
	// Status narrows the page. Empty means every status, which is a different query and a different plan —
	// see reports.sql.
	Status string
	// Before is the report id to resume from, exclusive. Nil starts at the newest.
	//
	// A pointer rather than a zero, for the reason M14's audit cursor is one: snowflake.Parse accepts "0",
	// so a client templating ?before={cursor} from an unset variable would send a valid zero that silently
	// meant "no cursor" and restarted a paging loop from the top.
	Before *snowflake.ID
	Limit  int32
}

// List returns one page of a guild's reports, newest first.
//
// Gated by PermManageMessages — the bit that already means "may act on somebody else's message in this
// guild", which is what a triage decision is. M16a reuses it rather than inventing one, and no new
// permission bit is added here: adding one renumbers nothing but does have to be argued for, and this
// surface is squarely what the existing bit is about.
//
// The page carries no message content. See reports.sql for the three reasons, of which the load-bearing
// one is that rule 13's exclusion then has exactly one place to be right.
func (s *Service) List(
	ctx context.Context, actor auth.Actor, guildID snowflake.ID, in ListInput,
) ([]TriageEntry, error) {
	var status int16
	if in.Status != "" {
		v, ok := statusValue(in.Status)
		if !ok {
			// Refused rather than answered with an empty page, which is M14's rule for the audit log's
			// action filter: a value nothing matches is indistinguishable from a guild that has none, so a
			// typo would read as evidence of absence.
			//
			// "under_review" passes deliberately even though nothing writes it yet. It is a real stored
			// value the schema reserves, so filtering on it is a well-formed question with the honest
			// answer "none" — and when M74 starts writing it, the filter already works.
			return nil, httpx.Errorf(httpx.ErrBadRequest, "status is not a report status")
		}
		status = v
	}

	if _, err := guildauth.Authorize(
		ctx, s.queries, actor, guildID, 0, roles.PermManageMessages,
	); err != nil {
		return nil, err
	}

	limit := in.Limit
	switch {
	case limit <= 0:
		limit = defaultPageSize
	case limit > maxPageSize:
		// Clamped here and refused at the handler, which is the split guilds.ListAuditLog explains: an
		// over-limit HTTP request is a client error that gets a 400 naming the ceiling, because a short
		// page means exhausted and a silent clamp would make a client believe that. This is the backstop
		// for a programmatic caller with no response to read.
		limit = maxPageSize
	}

	var before *int64
	if in.Before != nil {
		v := int64(*in.Before)
		before = &v
	}
	guild := int64(guildID)

	var out []TriageEntry
	if in.Status == "" {
		rows, err := s.queries.ListGuildReports(ctx, db.ListGuildReportsParams{
			GuildID: &guild, Before: before, Limit: limit,
		})
		if err != nil {
			return nil, fmt.Errorf("reports: list guild reports: %w", err)
		}
		out = make([]TriageEntry, 0, len(rows))
		for _, row := range rows {
			entry, err := triageEntry(row.Report, row.TargetIsE2e, row.TargetDeletedAt)
			if err != nil {
				return nil, err
			}
			out = append(out, entry)
		}
		return out, nil
	}

	rows, err := s.queries.ListGuildReportsByStatus(ctx, db.ListGuildReportsByStatusParams{
		GuildID: &guild, Status: status, Before: before, Limit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("reports: list guild reports by status: %w", err)
	}
	out = make([]TriageEntry, 0, len(rows))
	for _, row := range rows {
		entry, err := triageEntry(row.Report, row.TargetIsE2e, row.TargetDeletedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

// Get returns one report with the reported message attached.
//
// **The only place this milestone returns content**, which is what keeps rule 13's exclusion to one place.
// The exclusion itself is in the query, not here: `target_content` comes from a join carrying
// `NOT is_e2e`, so an encrypted message contributes a NULL rather than something this function has to
// remember to blank.
//
// # What it discloses, which is a decision rather than a fallout
//
// A PermManageMessages holder reads the reported message whether or not they can currently view the
// channel it was posted in. That is M14's audit-log answer applied to content, and for its two reasons:
// filtering on present visibility would let somebody hide what they did by locking a channel down
// afterwards, and a report about a channel the moderator is not in is exactly the report that needs
// reading. Recorded in docs/security-ledger.md with the condition that would reopen it.
func (s *Service) Get(
	ctx context.Context, actor auth.Actor, guildID, reportID snowflake.ID,
) (TriageDetail, error) {
	if _, err := guildauth.Authorize(
		ctx, s.queries, actor, guildID, 0, roles.PermManageMessages,
	); err != nil {
		return TriageDetail{}, err
	}

	row, err := s.queries.GetGuildReport(ctx, db.GetGuildReportParams{
		ID: int64(reportID), GuildID: ptrInt64(guildID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Scoped by guild in the query, so a report id belonging to another guild is unreachable
			// through this path and answers the same 404 as one that does not exist — M15's loadInChannel
			// rule, one object up.
			return TriageDetail{}, httpx.ErrNotFound
		}
		return TriageDetail{}, fmt.Errorf("reports: get guild report: %w", err)
	}

	entry, err := triageEntry(row.Report, row.TargetIsE2e, row.TargetDeletedAt)
	if err != nil {
		return TriageDetail{}, err
	}

	out := TriageDetail{TriageEntry: entry, TargetContent: row.TargetContent}
	if row.TargetChannelID != nil {
		id := snowflake.ID(*row.TargetChannelID)
		out.TargetChannelID = &id
	}
	if row.TargetAuthorID != nil {
		id := snowflake.ID(*row.TargetAuthorID)
		out.TargetAuthorID = &id
	}
	return out, nil
}

// Resolve closes a report, as either resolved or dismissed.
//
// # Every guard is in the statement
//
// [db.Queries.ResolveReport] carries `status = 0` and the guild id in its WHERE, so a second close finds
// no row and a report from another guild is unreachable. That is deliberate rather than incidental: as a
// read-then-write this is the shape M10's invite redemption had when four of four concurrent racers got
// in, and two moderators clicking at once would each write an audit entry for a decision only one of them
// made.
//
// The follow-up read below runs only to tell 404 from 409 and decides nothing.
//
// # Rule 2
//
// The entry is written in this transaction. `changes` carries the status transition and the target it was
// about, and **neither the reporter nor any content** — the first because a guild moderator is never told
// who filed (see [Report]), the second for the reason messages.writeModerationAudit states: rule 13 is
// satisfied by there being nothing to exclude rather than by an exclusion somebody must remember.
func (s *Service) Resolve(
	ctx context.Context, actor auth.Actor, guildID, reportID snowflake.ID, outcome string,
) (Report, error) {
	status, ok := statusValue(outcome)
	if !ok {
		return Report{}, httpx.Errorf(httpx.ErrBadRequest, "status is not a report status")
	}
	if status != StatusResolved && status != StatusDismissed {
		// Reopening and under_review are both refused, and refused here rather than by omission. Terminal
		// states are terminal (see the query), and under_review is reserved for M74 — a milestone that
		// starts writing it will find this branch rather than a silent success.
		return Report{}, httpx.Errorf(httpx.ErrBadRequest,
			"a report is closed as either resolved or dismissed")
	}

	var out Report
	err := s.inTx(ctx, func(q *db.Queries) error {
		if _, err := guildauth.Authorize(
			ctx, q, actor, guildID, 0, roles.PermManageMessages,
		); err != nil {
			return err
		}

		actorID := int64(actor.UserID)
		row, err := q.ResolveReport(ctx, db.ResolveReportParams{
			ID: int64(reportID), GuildID: ptrInt64(guildID), Status: status, ResolvedBy: &actorID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return s.explainFailedResolve(ctx, q, guildID, reportID)
			}
			return fmt.Errorf("reports: resolve report: %w", err)
		}

		if err := s.writeResolutionAudit(ctx, q, guildID, actor.UserID, row); err != nil {
			return err
		}

		out, err = reportFromRow(row)
		return err
	})
	return out, err
}

// explainFailedResolve turns "the UPDATE matched nothing" into the right refusal.
//
// The statement's WHERE carries three conditions and a miss could be any of them, so this reads the row
// back to distinguish a report that is not here from one that is already closed. It runs *after* the
// authoritative write has already failed to match, so it cannot be the race the guard exists to prevent —
// it decides a status code, not an outcome.
func (s *Service) explainFailedResolve(
	ctx context.Context, q *db.Queries, guildID, reportID snowflake.ID,
) error {
	row, err := q.GetGuildReport(ctx, db.GetGuildReportParams{
		ID: int64(reportID), GuildID: ptrInt64(guildID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound
		}
		return fmt.Errorf("reports: explain failed resolve: %w", err)
	}
	if row.Report.Status != StatusOpen {
		return httpx.Errorf(httpx.ErrConflict, "this report has already been closed")
	}
	// The row is here and open, so the UPDATE should have matched. Reporting a conflict would be a guess;
	// this is a real inconsistency and is worth a 500 rather than a plausible-looking answer.
	return fmt.Errorf("reports: resolve matched no row for an open report %d", reportID)
}

// writeResolutionAudit records a triage decision (rule 2).
//
// Written inline rather than through a shared helper, for the reason messages.writeModerationAudit is:
// `guilds` has sixteen verbs and its own writer, and a second general-purpose one here would be an
// abstraction built for two callers in one file. The point to extract one is a third package needing it —
// the argument that moved the authorization chokepoint at M15, not yet earned.
func (s *Service) writeResolutionAudit(
	ctx context.Context, q *db.Queries, guildID, actorID snowflake.ID, row db.Report,
) error {
	auditID, err := s.ids.Next()
	if err != nil {
		return fmt.Errorf("reports: mint audit entry id: %w", err)
	}

	action := ActionReportResolve
	if row.Status == StatusDismissed {
		action = ActionReportDismiss
	}

	targetType, ok := targetTypeName(row.TargetType)
	if !ok {
		return errUnknownStoredValue("target_type", row.TargetType)
	}
	newStatus, ok := statusName(row.Status)
	if !ok {
		return errUnknownStoredValue("status", row.Status)
	}

	// M14's two-kinds-of-key shape: a changed field is an object carrying from/to, a context field is a
	// scalar. `status` is the change; the target is context, because one column on the entry cannot say
	// both which report and which message.
	changes := map[string]any{
		"status":      map[string]any{"from": "open", "to": newStatus},
		"target_type": targetType,
		"target_id":   snowflake.ID(row.TargetID),
	}
	encoded, err := json.Marshal(changes)
	if err != nil {
		return fmt.Errorf("reports: encode audit changes: %w", err)
	}

	guild := int64(guildID)
	target := row.ID
	if err := q.WriteAuditLogEntry(ctx, db.WriteAuditLogEntryParams{
		ID:       int64(auditID),
		GuildID:  &guild,
		ActorID:  int64(actorID),
		Action:   action,
		TargetID: &target,
		Changes:  encoded,
	}); err != nil {
		return fmt.Errorf("reports: write audit entry: %w", err)
	}
	return nil
}

func triageEntry(row db.Report, isE2E *bool, deletedAt pgtype.Timestamptz) (TriageEntry, error) {
	base, err := reportFromRow(row)
	if err != nil {
		return TriageEntry{}, err
	}
	out := TriageEntry{Report: base, TargetIsE2E: isE2E}
	if deletedAt.Valid {
		t := deletedAt.Time
		out.TargetDeletedAt = &t
	}
	return out, nil
}

func ptrInt64(id snowflake.ID) *int64 {
	v := int64(id)
	return &v
}
