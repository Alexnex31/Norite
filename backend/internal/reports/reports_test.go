// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package reports

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/auth"
	"github.com/Alexnex31/Norite/backend/internal/platform/database"
	"github.com/Alexnex31/Norite/backend/internal/platform/dbtest"
	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
	"github.com/Alexnex31/Norite/backend/migrations"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

type fixture struct {
	svc  *Service
	pool *pgxpool.Pool
	ctx  context.Context
	ids  *snowflake.Generator

	guildID, channelID snowflake.ID
	owner, member, mod snowflake.ID
	stranger           snowflake.ID
	everyoneRoleID     snowflake.ID
	messageID          snowflake.ID
}

// newFixture builds a guild with an owner, a member, a moderator and a stranger, by direct SQL.
//
// The guilds service would do it through real endpoints and this package cannot import it — the M15
// chokepoint extraction is exactly what stops it — so the setup is a handful of inserts, as `messages`
// does for the same reason.
func newFixture(t *testing.T) *fixture {
	t.Helper()

	dsn := dbtest.FreshDatabase(t)
	ctx := t.Context()

	require.NoError(t, database.Migrate(ctx, database.MigrateOptions{
		DatabaseURL: dsn, Source: migrations.FS, SourceDir: ".", LockTimeout: 30 * time.Second,
	}))

	pool, err := database.New(ctx, database.PoolOptions{
		DatabaseURL: dsn, MaxConns: 8, MinConns: 1, ConnectTimeout: 10 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	ids, err := snowflake.NewGenerator(0)
	require.NoError(t, err)

	svc, err := NewService(ServiceOptions{Pool: pool, IDs: ids})
	require.NoError(t, err)

	f := &fixture{svc: svc, pool: pool, ctx: ctx, ids: ids}
	f.owner, f.member, f.mod, f.stranger = f.next(t), f.next(t), f.next(t), f.next(t)
	f.guildID, f.channelID, f.everyoneRoleID = f.next(t), f.next(t), f.next(t)

	for _, u := range []struct {
		id   snowflake.ID
		name string
	}{{f.owner, "owner"}, {f.member, "member"}, {f.mod, "mod"}, {f.stranger, "stranger"}} {
		f.exec(t, `INSERT INTO users (id, username, email, display_name, created_at, updated_at)
		           VALUES ($1,$2::text,$2::text||'@example.test',$2::text,now(),now())`, int64(u.id), u.name)
	}

	f.exec(t, `INSERT INTO guilds (id, name, owner_id, created_at, updated_at)
	           VALUES ($1,'g',$2,now(),now())`, int64(f.guildID), int64(f.owner))
	f.exec(t, `INSERT INTO channels (id, guild_id, name, type, position, created_at, updated_at)
	           VALUES ($1,$2,'general',0,0,now(),now())`, int64(f.channelID), int64(f.guildID))

	everyone := roles.PermViewChannel | roles.PermReadMessageHistory | roles.PermSendMessages
	f.exec(t, `INSERT INTO roles (id, guild_id, name, permissions, position, is_default, created_at, updated_at)
	           VALUES ($1,$2,'@everyone',$3,0,true,now(),now())`,
		int64(f.everyoneRoleID), int64(f.guildID), everyone.Int64())

	for _, u := range []snowflake.ID{f.owner, f.member, f.mod} {
		f.exec(t, `INSERT INTO guild_members (guild_id, user_id, joined_at) VALUES ($1,$2,now())`,
			int64(f.guildID), int64(u))
	}

	modRole := f.next(t)
	f.exec(t, `INSERT INTO roles (id, guild_id, name, permissions, position, is_default, created_at, updated_at)
	           VALUES ($1,$2,'mod',$3,1,false,now(),now())`,
		int64(modRole), int64(f.guildID), roles.PermManageMessages.Int64())
	f.exec(t, `INSERT INTO guild_member_roles (guild_id, user_id, role_id) VALUES ($1,$2,$3)`,
		int64(f.guildID), int64(f.mod), int64(modRole))

	f.messageID = f.newMessage(t, f.channelID, f.owner, "something objectionable", false)
	return f
}

func (f *fixture) next(t *testing.T) snowflake.ID {
	t.Helper()
	id, err := f.ids.Next()
	require.NoError(t, err)
	return id
}

func (f *fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	_, err := f.pool.Exec(f.ctx, sql, args...)
	require.NoError(t, err)
}

func (f *fixture) newMessage(
	t *testing.T, channelID, author snowflake.ID, content string, e2e bool,
) snowflake.ID {
	t.Helper()
	id := f.next(t)
	f.exec(t, `INSERT INTO messages (id, channel_id, author_id, content, type, is_e2e, created_at)
	           VALUES ($1,$2,$3,$4,0,$5,now())`, int64(id), int64(channelID), int64(author), content, e2e)
	return id
}

func userActor(id snowflake.ID) auth.Actor {
	return auth.Actor{UserID: id, Kind: auth.ActorUser}
}

func (f *fixture) file(t *testing.T, actor snowflake.ID, target snowflake.ID) Report {
	t.Helper()
	r, err := f.svc.File(f.ctx, userActor(actor), FileInput{
		TargetType: "message", TargetID: target, ReasonCategory: "spam",
	})
	require.NoError(t, err)
	return r
}

// --- filing ---

func TestAMemberCanFileAReportAgainstAMessage(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	detail := "this is abusive"
	report, err := f.svc.File(f.ctx, userActor(f.member), FileInput{
		TargetType: "message", TargetID: f.messageID, ReasonCategory: "harassment", Detail: &detail,
	})
	require.NoError(t, err)

	require.Equal(t, "message", report.TargetType)
	require.Equal(t, f.messageID, report.TargetID)
	require.Equal(t, "harassment", report.ReasonCategory)
	require.Equal(t, "open", report.Status)
	require.NotNil(t, report.GuildID)
	require.Equal(t, f.guildID, *report.GuildID)
	require.Nil(t, report.ResolvedBy)
	require.Nil(t, report.ResolvedAt)
}

// TestTheRoutingIsComputedAndNotTakenFromTheClient covers the half no HTTP test reaches.
//
// [FileInput] has no routing field at all, so the service cannot be told where to send a report — and the
// request struct has none either, which means the handler answers 400 rather than ignoring one (unknown
// fields are rejected). This asserts what actually lands in the column, which is the part that would still
// be wrong if a later milestone added the field "just to return a friendlier error".
func TestTheRoutingIsComputedAndNotTakenFromTheClient(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	report := f.file(t, f.member, f.messageID)

	var routedTo int16
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT routed_to FROM reports WHERE id = $1`, int64(report.ID)).Scan(&routedTo))
	require.Equal(t, RoutedToGuildModerators, routedTo,
		"a report against a guild message must route to that guild's moderators; routing it to the "+
			"instance queue would put it past the very people who are supposed to see it")
}

// TestAMessageInAnUnseeableChannelCannotBeReported is the anti-enumeration half of filing.
//
// A stranger holding a real message id must be refused exactly as though it did not exist. Which of the
// two it is is precisely what M13's channel filter withholds.
func TestAMessageInAnUnseeableChannelCannotBeReported(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	_, err := f.svc.File(f.ctx, userActor(f.stranger), FileInput{
		TargetType: "message", TargetID: f.messageID, ReasonCategory: "spam",
	})
	require.ErrorIs(t, err, httpx.ErrNotFound)
}

// TestADeletedMessageIsStillReportable pins what 000020's soft delete is for.
//
// Without it, deleting quickly is how somebody dodges a report.
func TestADeletedMessageIsStillReportable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	f.exec(t, `UPDATE messages SET deleted_at = now() WHERE id = $1`, int64(f.messageID))

	report := f.file(t, f.member, f.messageID)
	require.Equal(t, f.messageID, report.TargetID)
}

// TestOnlyAMessageCanBeReportedToday asserts the reserved target types are refused rather than stored.
//
// A target type nothing routes would be a report filed into a queue that never shows it, which is M14's
// "reads as evidence of absence" pointed at a write.
func TestOnlyAMessageCanBeReportedToday(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	for _, kind := range []string{"whisper", "channel", "user"} {
		_, err := f.svc.File(f.ctx, userActor(f.member), FileInput{
			TargetType: kind, TargetID: f.messageID, ReasonCategory: "spam",
		})
		require.ErrorIsf(t, err, httpx.ErrBadRequest, "%s must be refused until a milestone routes it", kind)
	}

	_, err := f.svc.File(f.ctx, userActor(f.member), FileInput{
		TargetType: "nonsense", TargetID: f.messageID, ReasonCategory: "spam",
	})
	require.ErrorIs(t, err, httpx.ErrBadRequest)
}

func TestAnUnknownReasonCategoryIsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	_, err := f.svc.File(f.ctx, userActor(f.member), FileInput{
		TargetType: "message", TargetID: f.messageID, ReasonCategory: "because-i-say-so",
	})
	require.ErrorIs(t, err, httpx.ErrBadRequest)
}

// TestASecondOpenReportOnTheSameTargetIsRefused exercises 000021's partial unique index.
//
// Concurrently, because the guard is in the index rather than in Go precisely so that it holds under a
// race — M10's invite redemption was a check-then-act and four of four concurrent racers got in. A
// sequential-only test would pass against the check-then-act version this one exists to rule out.
func TestASecondOpenReportOnTheSameTargetIsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	const racers = 6
	var wg sync.WaitGroup
	results := make([]error, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, results[i] = f.svc.File(f.ctx, userActor(f.member), FileInput{
				TargetType: "message", TargetID: f.messageID, ReasonCategory: "spam",
			})
		}()
	}
	close(start)
	wg.Wait()

	var created int
	for _, err := range results {
		if err == nil {
			created++
			continue
		}
		require.ErrorIs(t, err, httpx.ErrConflict)
	}
	require.Equal(t, 1, created, "exactly one of %d concurrent filings may win", racers)

	// A different reporter is a different report, and re-filing after the first is closed is allowed —
	// both are what makes the index partial rather than absolute.
	second := f.file(t, f.mod, f.messageID)
	require.NotZero(t, second.ID)
}

func TestAClosedReportLetsTheSameReporterFileAgain(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	first := f.file(t, f.member, f.messageID)
	_, err := f.svc.Resolve(f.ctx, userActor(f.mod), f.guildID, first.ID, "dismissed")
	require.NoError(t, err)

	again := f.file(t, f.member, f.messageID)
	require.NotEqual(t, first.ID, again.ID)
}

// --- triage reads ---

func TestTheTriageQueueRequiresManageMessages(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.file(t, f.member, f.messageID)

	_, err := f.svc.List(f.ctx, userActor(f.member), f.guildID, ListInput{})
	require.ErrorIs(t, err, httpx.ErrForbidden, "a member without the bit already knows the guild exists")

	_, err = f.svc.List(f.ctx, userActor(f.stranger), f.guildID, ListInput{})
	require.ErrorIs(t, err, httpx.ErrNotFound, "a non-member must not learn the guild exists")

	page, err := f.svc.List(f.ctx, userActor(f.mod), f.guildID, ListInput{})
	require.NoError(t, err)
	require.Len(t, page, 1)
}

// TestNoTriageResponseNamesTheReporter is the milestone's anonymity decision, asserted on the wire.
//
// Marshaled rather than inspected field by field, because what matters is what a client receives. A
// `reporter_id` added to any of the three shapes — or an embedded struct quietly carrying one — shows up
// here whatever it is called.
func TestNoTriageResponseNamesTheReporter(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	filed := f.file(t, f.member, f.messageID)

	page, err := f.svc.List(f.ctx, userActor(f.mod), f.guildID, ListInput{})
	require.NoError(t, err)
	require.Len(t, page, 1)

	detail, err := f.svc.Get(f.ctx, userActor(f.mod), f.guildID, filed.ID)
	require.NoError(t, err)

	for name, payload := range map[string]any{
		"the filing response": filed,
		"a triage page":       page,
		"a report's detail":   detail,
	} {
		encoded, err := json.Marshal(payload)
		require.NoError(t, err)
		lowered := strings.ToLower(string(encoded))
		require.NotContainsf(t, lowered, "reporter", "%s names the reporter", name)
		// The id itself, in case a field is ever named something other than "reporter".
		require.NotContainsf(t, string(encoded), f.member.String(),
			"%s carries the reporter's user id", name)
	}
}

func TestAModeratorReadsTheReportedMessage(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	filed := f.file(t, f.member, f.messageID)

	detail, err := f.svc.Get(f.ctx, userActor(f.mod), f.guildID, filed.ID)
	require.NoError(t, err)

	require.NotNil(t, detail.TargetContent)
	require.Equal(t, "something objectionable", *detail.TargetContent)
	require.NotNil(t, detail.TargetIsE2E)
	require.False(t, *detail.TargetIsE2E)
	require.Nil(t, detail.TargetDeletedAt)
	require.NotNil(t, detail.TargetChannelID)
	require.Equal(t, f.channelID, *detail.TargetChannelID)
}

// TestAnEncryptedMessagesContentIsNeverReturned is rule 13, enforced rather than argued.
//
// The state it builds cannot be reached through the API today: E2E is offered for DMs only, a DM has no
// guild, and this surface is guild-scoped — so `is_e2e` is set here by direct SQL on a guild message
// precisely because no endpoint would produce one.
//
// That is deliberate and is M11a's lesson. It made the same "closed by construction" claim about password
// reset not bypassing the second factor and gave it a test anyway, because such a claim stops being true
// quietly. If a later milestone makes a guild channel capable of carrying encrypted content, this fails
// here rather than leaking there.
func TestAnEncryptedMessagesContentIsNeverReturned(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	secret := f.newMessage(t, f.channelID, f.owner, "CIPHERTEXT-MUST-NOT-APPEAR", true)
	filed := f.file(t, f.member, secret)

	detail, err := f.svc.Get(f.ctx, userActor(f.mod), f.guildID, filed.ID)
	require.NoError(t, err)

	require.Nil(t, detail.TargetContent, "rule 13: an E2E message's content is never returned")
	require.NotNil(t, detail.TargetIsE2E)
	require.True(t, *detail.TargetIsE2E,
		"the flag must say why the content is missing, or a moderator cannot tell a withheld message "+
			"from a vanished one")

	encoded, err := json.Marshal(detail)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "CIPHERTEXT-MUST-NOT-APPEAR")
}

// TestAGuildScopedTriageCannotReachADirectMessage pins the structural half of the argument above.
//
// guildauth.guildOf refuses a channel whose guild_id is NULL — every DM and group DM — so a DM message
// cannot be filed against here at all, which is why rule 13's exclusion is belt-and-braces rather than the
// only thing standing between this surface and encrypted content. If a milestone makes a DM reportable
// through this path, this fails and the exclusion above becomes load-bearing rather than redundant.
func TestAGuildScopedTriageCannotReachADirectMessage(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	dm := f.next(t)
	f.exec(t, `INSERT INTO channels (id, guild_id, name, type, position, created_at, updated_at)
	           VALUES ($1,NULL,NULL,2,0,now(),now())`, int64(dm))
	dmMessage := f.newMessage(t, dm, f.member, "private", false)

	_, err := f.svc.File(f.ctx, userActor(f.member), FileInput{
		TargetType: "message", TargetID: dmMessage, ReasonCategory: "spam",
	})
	require.ErrorIs(t, err, httpx.ErrNotFound,
		"a DM has no guild to route to; those are M74's and must not reach the guild surface")
}

func TestAReportFromAnotherGuildIsNotReachable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	filed := f.file(t, f.member, f.messageID)

	other := f.next(t)
	f.exec(t, `INSERT INTO guilds (id, name, owner_id, created_at, updated_at)
	           VALUES ($1,'other',$2,now(),now())`, int64(other), int64(f.mod))
	f.exec(t, `INSERT INTO guild_members (guild_id, user_id, joined_at) VALUES ($1,$2,now())`,
		int64(other), int64(f.mod))
	otherEveryone := f.next(t)
	f.exec(t, `INSERT INTO roles (id, guild_id, name, permissions, position, is_default, created_at, updated_at)
	           VALUES ($1,$2,'@everyone',$3,0,true,now(),now())`,
		int64(otherEveryone), int64(other), roles.PermManageMessages.Int64())

	_, err := f.svc.Get(f.ctx, userActor(f.mod), other, filed.ID)
	require.ErrorIs(t, err, httpx.ErrNotFound,
		"the guild in the path is what was authorized; reaching a report through another one would mean "+
			"the permission check covered one guild and the read landed in another")
}

func TestAnUnknownStatusFilterIsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	_, err := f.svc.List(f.ctx, userActor(f.mod), f.guildID, ListInput{Status: "pending"})
	require.ErrorIs(t, err, httpx.ErrBadRequest,
		"an empty page for a typo reads as evidence that the guild has none")

	// A reserved-but-unwritten status is a well-formed question with an honest answer.
	page, err := f.svc.List(f.ctx, userActor(f.mod), f.guildID, ListInput{Status: "under_review"})
	require.NoError(t, err)
	require.Empty(t, page)
}

func TestTheStatusFilterSelects(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	first := f.file(t, f.member, f.messageID)
	second := f.newMessage(t, f.channelID, f.owner, "also bad", false)
	f.file(t, f.member, second)

	_, err := f.svc.Resolve(f.ctx, userActor(f.mod), f.guildID, first.ID, "resolved")
	require.NoError(t, err)

	open, err := f.svc.List(f.ctx, userActor(f.mod), f.guildID, ListInput{Status: "open"})
	require.NoError(t, err)
	require.Len(t, open, 1)

	resolved, err := f.svc.List(f.ctx, userActor(f.mod), f.guildID, ListInput{Status: "resolved"})
	require.NoError(t, err)
	require.Len(t, resolved, 1)
	require.Equal(t, first.ID, resolved[0].ID)

	all, err := f.svc.List(f.ctx, userActor(f.mod), f.guildID, ListInput{})
	require.NoError(t, err)
	require.Len(t, all, 2)
}

// --- closing ---

// TestClosingAReportIsAudited is rule 2 for this package, and is what guilds' exemption map points at.
func TestClosingAReportIsAudited(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	filed := f.file(t, f.member, f.messageID)

	closed, err := f.svc.Resolve(f.ctx, userActor(f.mod), f.guildID, filed.ID, "resolved")
	require.NoError(t, err)
	require.Equal(t, "resolved", closed.Status)
	require.NotNil(t, closed.ResolvedBy)
	require.Equal(t, f.mod, *closed.ResolvedBy)
	require.NotNil(t, closed.ResolvedAt)

	var action string
	var actorID, targetID int64
	var changes []byte
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT action, actor_id, target_id, changes FROM audit_log_entries WHERE guild_id = $1`,
		int64(f.guildID)).Scan(&action, &actorID, &targetID, &changes))

	require.Equal(t, ActionReportResolve, action)
	require.Equal(t, int64(f.mod), actorID)
	require.Equal(t, int64(filed.ID), targetID, "the entry names the report that was acted on")

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(changes, &decoded))

	status, ok := decoded["status"].(map[string]any)
	require.True(t, ok, "a changed field is an object carrying from/to (M14's uniform shape)")
	require.Equal(t, "open", status["from"])
	require.Equal(t, "resolved", status["to"])
	require.Equal(t, "message", decoded["target_type"], "context fields are scalars")

	// The two things this payload must never carry.
	require.NotContains(t, strings.ToLower(string(changes)), "reporter")
	require.NotContains(t, string(changes), "something objectionable")
	require.NotContains(t, string(changes), f.member.String())
}

func TestDismissingUsesItsOwnVerb(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	filed := f.file(t, f.member, f.messageID)

	_, err := f.svc.Resolve(f.ctx, userActor(f.mod), f.guildID, filed.ID, "dismissed")
	require.NoError(t, err)

	var action string
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT action FROM audit_log_entries WHERE guild_id = $1`, int64(f.guildID)).Scan(&action))
	require.Equal(t, ActionReportDismiss, action,
		"the outcome is the whole content of a triage decision, so it is the verb rather than a field")
}

// TestAClosedReportCannotBeClosedAgain exercises the guard in the UPDATE's WHERE.
//
// Concurrently as well as sequentially: two moderators acting at once must produce one decision and one
// audit entry, which is the property a read-then-write would lose.
func TestAClosedReportCannotBeClosedAgain(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	filed := f.file(t, f.member, f.messageID)

	const racers = 6
	var wg sync.WaitGroup
	results := make([]error, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, results[i] = f.svc.Resolve(f.ctx, userActor(f.mod), f.guildID, filed.ID, "resolved")
		}()
	}
	close(start)
	wg.Wait()

	var won int
	for _, err := range results {
		if err == nil {
			won++
			continue
		}
		require.ErrorIs(t, err, httpx.ErrConflict)
	}
	require.Equal(t, 1, won, "exactly one of %d concurrent closes may win", racers)

	var entries int
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM audit_log_entries WHERE guild_id = $1`, int64(f.guildID)).Scan(&entries))
	require.Equal(t, 1, entries, "one decision, one entry")

	_, err := f.svc.Resolve(f.ctx, userActor(f.mod), f.guildID, filed.ID, "dismissed")
	require.ErrorIs(t, err, httpx.ErrConflict)
}

func TestClosingRefusesOpenAndUnderReview(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	filed := f.file(t, f.member, f.messageID)

	for _, outcome := range []string{"open", "under_review", "nonsense"} {
		_, err := f.svc.Resolve(f.ctx, userActor(f.mod), f.guildID, filed.ID, outcome)
		require.ErrorIsf(t, err, httpx.ErrBadRequest, "%q must not be an outcome", outcome)
	}
}

func TestClosingRequiresManageMessages(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	filed := f.file(t, f.member, f.messageID)

	_, err := f.svc.Resolve(f.ctx, userActor(f.member), f.guildID, filed.ID, "resolved")
	require.ErrorIs(t, err, httpx.ErrForbidden)

	_, err = f.svc.Resolve(f.ctx, userActor(f.stranger), f.guildID, filed.ID, "resolved")
	require.ErrorIs(t, err, httpx.ErrNotFound)

	// And nothing was written on either refusal.
	var entries int
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM audit_log_entries WHERE guild_id = $1`, int64(f.guildID)).Scan(&entries))
	require.Zero(t, entries)
}

func TestClosingAReportThatIsNotThereIs404(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	_, err := f.svc.Resolve(f.ctx, userActor(f.mod), f.guildID, f.next(t), "resolved")
	require.ErrorIs(t, err, httpx.ErrNotFound)
}

// TestFilingWritesNoAuditEntry is the negative rule 2 asserts, and it has no other home.
//
// Before M15's narrowing this would have been a bug report. It is the property that keeps the moderation
// signal M14 built a reader for from being buried under member traffic.
func TestFilingWritesNoAuditEntry(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	f.file(t, f.member, f.messageID)

	var entries int
	require.NoError(t, f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM audit_log_entries WHERE guild_id = $1`, int64(f.guildID)).Scan(&entries))
	require.Zero(t, entries, "filing exercises authority over nobody and must not write an entry")
}

// TestDeletingAGuildTakesItsReportsWithIt asserts the cascade as the state it is.
//
// The same shape M12 asserted for audit_log_entries rather than leaving it to look like a bug: the durable
// record of an instance-level action is rule 14's instance_audit_log (M74), never this table.
func TestDeletingAGuildTakesItsReportsWithIt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.file(t, f.member, f.messageID)

	f.exec(t, `DELETE FROM guilds WHERE id = $1`, int64(f.guildID))

	var remaining int
	require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT count(*) FROM reports`).Scan(&remaining))
	require.Zero(t, remaining)
}

// TestTheDetailTagAgreesWithTheConstant pins the handler's literal against MaxDetailLength.
//
// A struct tag cannot reference a constant, so the bound is written twice and can drift. M15 shipped
// exactly that and the two halves measured different things — bytes against runes — which would have
// refused any near-limit message in a non-Latin script. Both halves here count runes; this pins the number.
func TestTheDetailTagAgreesWithTheConstant(t *testing.T) {
	t.Parallel()

	field, ok := reflectFileRequestField("Detail")
	require.True(t, ok)
	require.Contains(t, field, "max=2000")
	require.Equal(t, 2000, MaxDetailLength)
}

// TestTheDetailBoundCountsCharactersNotBytes is the other half of M15's lesson.
//
// A test suite written in English cannot see this, which is why the cases are Japanese and emoji: 2,000
// Japanese characters are 6,000 bytes and 2,000 emoji are 8,000, all of which a byte-based check would
// refuse and the handler's rune-counting validator would accept.
func TestTheDetailBoundCountsCharactersNotBytes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	for name, text := range map[string]string{
		"japanese": strings.Repeat("あ", MaxDetailLength),
		"emoji":    strings.Repeat("🙂", MaxDetailLength),
		"arabic":   strings.Repeat("ع", MaxDetailLength),
	} {
		t.Run(name, func(t *testing.T) {
			message := f.newMessage(t, f.channelID, f.owner, "target for "+name, false)
			detail := text
			_, err := f.svc.File(f.ctx, userActor(f.member), FileInput{
				TargetType: "message", TargetID: message, ReasonCategory: "spam", Detail: &detail,
			})
			require.NoError(t, err, "exactly at the limit in runes must be accepted")
		})
	}

	over := strings.Repeat("あ", MaxDetailLength+1)
	message := f.newMessage(t, f.channelID, f.owner, "one too many", false)
	_, err := f.svc.File(f.ctx, userActor(f.member), FileInput{
		TargetType: "message", TargetID: message, ReasonCategory: "spam", Detail: &over,
	})
	require.ErrorIs(t, err, httpx.ErrBadRequest)
}

// reflectFileRequestField returns a fileRequest field's validate tag.
func reflectFileRequestField(name string) (string, bool) {
	field, ok := reflect.TypeOf(fileRequest{}).FieldByName(name)
	if !ok {
		return "", false
	}
	return field.Tag.Get("validate"), true
}
