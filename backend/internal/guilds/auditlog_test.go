// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/platform/httpx"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// TestTheAuditLogNeedsItsOwnPermission is why PermViewAuditLog is a bit rather than a shape of
// PermManageGuild.
//
// The three answers are the three authority questions M12 settled, on a route that is new: a stranger
// cannot learn the guild exists, a member who can administer the guild still cannot read who moderated
// whom, and the bit alone is sufficient — it does not also require being able to manage anything.
func TestTheAuditLogNeedsItsOwnPermission(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	stranger := f.newUser(ctx, "stranger")
	_, err := f.svc.ListAuditLog(ctx, userActor(stranger), f.guildID, ListAuditLogInput{})
	require.ErrorIs(t, err, httpx.ErrNotFound, "a non-member never learns the guild is there")

	// A member holding the most powerful non-administrator bit in the guild, and not this one.
	f.exec(ctx, `UPDATE roles SET permissions = $1 WHERE id = $2`,
		(roles.PermViewChannel | roles.PermManageGuild | roles.PermManageRoles |
			roles.PermManageChannels | roles.PermKickMembers).Int64(),
		int64(f.modRole))

	_, err = f.svc.ListAuditLog(ctx, userActor(f.mod), f.guildID, ListAuditLogInput{})
	require.ErrorIs(t, err, httpx.ErrForbidden,
		"managing the guild is not reading its audit log; 403 because they already know it exists")

	// The bit on its own, with nothing else. Granted to the plain member, who manages nothing.
	auditor := f.newRole(ctx, f.guildID, 2, roles.PermViewAuditLog)
	f.grantRole(ctx, f.guildID, f.plain, auditor)

	// Something to read. The fixture builds its guild by direct insert rather than through the service,
	// so nothing has written an entry yet — asserting a non-empty log without this passes only by
	// accident of how the fixture happens to be built.
	name := "renamed"
	_, err = f.svc.Update(ctx, userActor(f.owner), f.guildID, UpdateGuildInput{Name: &name})
	require.NoError(t, err)

	entries, err := f.svc.ListAuditLog(ctx, userActor(f.plain), f.guildID, ListAuditLogInput{})
	require.NoError(t, err, "the bit is sufficient on its own")
	require.Len(t, entries, 1, "and it reads what the owner just did")
	require.Equal(t, ActionGuildUpdate, entries[0].Action)
}

// TestTheAuditLogIsNewestFirstAndPagesWithoutLosingAnEntry covers the ordering and the cursor together,
// over an odd number of entries paged in twos so that a boundary falls inside the run rather than at its
// end.
//
// It does *not* try to manufacture a created_at collision, and the reason is worth keeping. The first
// version of this test asserted that two entries shared a timestamp, on the strength of a comment in
// migration 000017 saying same-millisecond bursts were the normal case here. They are not: one mutation
// writes one entry, and now() is the transaction's start time at microsecond resolution — 60 entries, 30
// of them written concurrently, produced 60 distinct values. The comment was corrected rather than the
// assertion weakened.
//
// So the id cursor is right for the reason that survives measurement: created_at carries no uniqueness
// guarantee, and a page boundary that loses an entry rarely is worse than one that loses it every time,
// because nobody will ever reproduce it from a report.
func TestTheAuditLogIsNewestFirstAndPagesWithoutLosingAnEntry(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	for i := 0; i < 7; i++ {
		name := "role-" + string(rune('a'+i))
		_, err := f.svc.CreateRole(ctx, userActor(f.owner), f.guildID, CreateRoleInput{Name: name})
		require.NoError(t, err)
	}

	whole, err := f.svc.ListAuditLog(ctx, userActor(f.owner), f.guildID, ListAuditLogInput{})
	require.NoError(t, err)
	require.Len(t, whole, 7,
		"the fixture inserts its guild directly, so the seven role creations are the whole log")

	for i := 1; i < len(whole); i++ {
		require.Greater(t, whole[i-1].ID, whole[i].ID, "newest first, strictly descending")
	}

	// The same entries, two at a time.
	var paged []AuditLogEntry
	var cursor snowflake.ID
	for {
		page, err := f.svc.ListAuditLog(ctx, userActor(f.owner), f.guildID,
			ListAuditLogInput{Before: cursor, Limit: 2})
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		paged = append(paged, page...)
		cursor = page[len(page)-1].ID
	}

	require.Equal(t, whole, paged, "paging in twos returns every entry exactly once, in the same order")

	// The boundary has to have fallen inside the run for any of the above to mean anything. Seven entries
	// at two per page is four pages, so it did — asserted rather than assumed, because a limit changed
	// later could quietly turn this into a single-page test that still passes.
	require.Greater(t, len(whole), 2, "or there is only one page and the cursor is never exercised")
}

// TestTheAuditLogDoesNotFilterByChannelVisibility pins the milestone's disclosure decision.
//
// A reader holding PermViewAuditLog and denied PermViewChannel on a channel sees that channel's entries,
// by id and by name. It is deliberate — see ListAuditLog's comment and docs/security-ledger.md — and it is
// the kind of decision that looks like a bug to whoever reads this code next, so it is asserted as the
// behavior rather than left to be discovered and "fixed".
func TestTheAuditLogDoesNotFilterByChannelVisibility(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	auditor := f.newRole(ctx, f.guildID, 2, roles.PermViewAuditLog)
	f.grantRole(ctx, f.guildID, f.plain, auditor)

	secret, err := f.svc.CreateChannel(ctx, userActor(f.owner), f.guildID,
		CreateChannelInput{Name: "staff-only", Type: ChannelGuildText})
	require.NoError(t, err)
	f.overwrite(ctx, secret.ID, roles.OverwriteTargetRole, f.everyoneID, 0, roles.PermViewChannel)

	listed, err := f.svc.ListChannels(ctx, userActor(f.plain), f.guildID)
	require.NoError(t, err)
	for _, c := range listed {
		require.NotEqual(t, secret.ID, c.ID, "the channel listing hides it from them")
	}

	entries, err := f.svc.ListAuditLog(ctx, userActor(f.plain), f.guildID, ListAuditLogInput{})
	require.NoError(t, err)

	var found *AuditLogEntry
	for i := range entries {
		if entries[i].Action == ActionChannelCreate && entries[i].TargetID != nil &&
			*entries[i].TargetID == secret.ID {
			found = &entries[i]
			break
		}
	}
	require.NotNil(t, found, "and the audit log shows it to them anyway; the permission is the boundary")

	var changes map[string]any
	require.NoError(t, json.Unmarshal(found.Changes, &changes))
	require.Equal(t, "staff-only", changes["name"], "including the name of a channel they cannot open")
}

// TestTheAuditLogFiltersNarrowRatherThanWiden covers both optional filters together, because the property
// that matters is shared: neither can return an entry the unfiltered listing would not.
func TestTheAuditLogFiltersNarrowRatherThanWiden(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	f.exec(ctx, `UPDATE roles SET permissions = $1 WHERE id = $2`,
		(roles.PermViewChannel | roles.PermManageChannels | roles.PermViewAuditLog).Int64(),
		int64(f.modRole))

	_, err := f.svc.CreateChannel(ctx, userActor(f.mod), f.guildID,
		CreateChannelInput{Name: "by-the-mod", Type: ChannelGuildText})
	require.NoError(t, err)
	_, err = f.svc.CreateRole(ctx, userActor(f.owner), f.guildID, CreateRoleInput{Name: "by-the-owner"})
	require.NoError(t, err)

	all, err := f.svc.ListAuditLog(ctx, userActor(f.mod), f.guildID, ListAuditLogInput{})
	require.NoError(t, err)

	byAction, err := f.svc.ListAuditLog(ctx, userActor(f.mod), f.guildID,
		ListAuditLogInput{Action: ActionChannelCreate})
	require.NoError(t, err)
	require.NotEmpty(t, byAction)
	require.Less(t, len(byAction), len(all), "a filter that returns everything is not filtering")
	for _, e := range byAction {
		require.Equal(t, ActionChannelCreate, e.Action)
	}

	byActor, err := f.svc.ListAuditLog(ctx, userActor(f.mod), f.guildID,
		ListAuditLogInput{ActorID: f.mod})
	require.NoError(t, err)
	require.NotEmpty(t, byActor)
	for _, e := range byActor {
		require.Equal(t, f.mod, e.ActorID)
	}

	both, err := f.svc.ListAuditLog(ctx, userActor(f.mod), f.guildID,
		ListAuditLogInput{Action: ActionChannelCreate, ActorID: f.owner})
	require.NoError(t, err)
	require.Empty(t, both, "the owner created no channel, so the two filters compose as AND")
}

// TestTheAuditLogLimitIsClampedNotRefused mirrors the member listing, whose comment argues the case.
func TestTheAuditLogLimitIsClampedNotRefused(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	for i := 0; i < maxAuditLogPageSize+5; i++ {
		_, err := f.svc.CreateRole(ctx, userActor(f.owner), f.guildID,
			CreateRoleInput{Name: "r" + string(rune('a'+i%26)) + string(rune('a'+i/26))})
		require.NoError(t, err)
	}

	page, err := f.svc.ListAuditLog(ctx, userActor(f.owner), f.guildID, ListAuditLogInput{Limit: 5000})
	require.NoError(t, err, "an oversized limit is not an error worth failing a request over")
	require.Len(t, page, maxAuditLogPageSize, "it is clamped to the ceiling")

	defaulted, err := f.svc.ListAuditLog(ctx, userActor(f.owner), f.guildID, ListAuditLogInput{})
	require.NoError(t, err)
	require.Len(t, defaulted, defaultAuditLogPageSize)
}

// TestAnUnknownActionFilterIsRefused is not input hygiene.
//
// An action nobody writes is a query matching nothing, which is indistinguishable from a guild that has
// taken no such action — so a typo would read as evidence of absence. The vocabulary is closed and this
// codebase owns every value in it, so the refusal is available and free.
func TestAnUnknownActionFilterIsRefused(t *testing.T) {
	t.Parallel()

	require.NoError(t, refuseUnknownAuditAction(ActionOverwriteDelete))
	for _, bad := range []string{"channel.created", "CHANNEL.CREATE", "guild.nuke", " channel.create"} {
		require.ErrorIs(t, refuseUnknownAuditAction(bad), httpx.ErrBadRequest, bad)
	}
}

// TestEveryAuditActionIsReachable is the coverage test model.go promised M14 would owe.
//
// It drives every mutation the package exposes and requires each to leave exactly the entry it claims.
// The value is in the direction nothing else checks: a constant that no writer produces looks identical to
// one whose writer was removed, and both look fine to a reader of model.go. Iterating AllAuditActions is
// what turns "we think these all fire" into a red build when one stops.
//
// guild.delete is the one action this endpoint cannot observe, and that is a property rather than a gap:
// audit_log_entries cascades from guilds, so a guild's own deletion entry is written inside the
// transaction — rule 2 is satisfied — and removed with everything else. Asserted below as the state it is.
func TestEveryAuditActionIsReachable(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()
	owner := userActor(f.owner)

	// guild.create, and the reads that follow all use this guild.
	name := "renamed"
	_, err := f.svc.Update(ctx, owner, f.guildID, UpdateGuildInput{Name: &name})
	require.NoError(t, err, "guild.update")

	ch, err := f.svc.CreateChannel(ctx, owner, f.guildID,
		CreateChannelInput{Name: "a-channel", Type: ChannelGuildText})
	require.NoError(t, err, "channel.create")

	_, err = f.svc.UpdateChannel(ctx, owner, ch.ID, UpdateChannelInput{Name: &name})
	require.NoError(t, err, "channel.update")

	doomedChannel, err := f.svc.CreateChannel(ctx, owner, f.guildID,
		CreateChannelInput{Name: "doomed", Type: ChannelGuildText})
	require.NoError(t, err)
	require.NoError(t, f.svc.DeleteChannel(ctx, owner, doomedChannel.ID), "channel.delete")

	role, err := f.svc.CreateRole(ctx, owner, f.guildID, CreateRoleInput{Name: "a-role"})
	require.NoError(t, err, "role.create")

	_, err = f.svc.UpdateRole(ctx, owner, f.guildID, role.ID, UpdateRoleInput{Name: &name})
	require.NoError(t, err, "role.update")

	_, err = f.svc.ReorderRoles(ctx, owner, f.guildID, []RolePosition{{ID: role.ID, Position: 1}})
	require.NoError(t, err, "role.reorder")

	_, err = f.svc.AssignRole(ctx, owner, f.guildID, f.plain, role.ID)
	require.NoError(t, err, "member.role_add")

	_, err = f.svc.UnassignRole(ctx, owner, f.guildID, f.plain, role.ID)
	require.NoError(t, err, "member.role_remove")

	nick := "nicknamed"
	_, err = f.svc.UpdateMember(ctx, owner, f.guildID, f.plain, UpdateMemberInput{Nickname: &nick})
	require.NoError(t, err, "member.update")

	_, err = f.svc.SetOverwrite(ctx, owner, SetOverwriteInput{
		ChannelID: ch.ID, TargetType: roles.OverwriteTargetRole, TargetID: f.everyoneID,
		Deny: roles.PermSendMessages,
	})
	require.NoError(t, err, "overwrite.set")

	require.NoError(t, f.svc.DeleteOverwrite(
		ctx, owner, ch.ID, roles.OverwriteTargetRole, f.everyoneID), "overwrite.delete")

	doomedRole, err := f.svc.CreateRole(ctx, owner, f.guildID, CreateRoleInput{Name: "doomed"})
	require.NoError(t, err)
	require.NoError(t, f.svc.DeleteRole(ctx, owner, f.guildID, doomedRole.ID), "role.delete")

	require.NoError(t, f.svc.RemoveMember(ctx, owner, f.guildID, f.plain), "member.remove")

	// Every entry, in pages, because there are more than the default page size by now.
	seen := map[string]int{}
	var cursor snowflake.ID
	for {
		page, err := f.svc.ListAuditLog(ctx, owner, f.guildID,
			ListAuditLogInput{Before: cursor, Limit: maxAuditLogPageSize})
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			seen[e.Action]++
		}
		cursor = page[len(page)-1].ID
	}

	// guild.create and guild.delete need a guild of their own, because the fixture inserts its one
	// directly and a guild cannot be created twice. They are also the pair that demonstrates the table's
	// one asymmetry, so they are worth doing together.
	ownGuild, err := f.svc.Create(ctx, owner, CreateGuildInput{Name: "made by the service"})
	require.NoError(t, err)

	created, err := f.svc.ListAuditLog(ctx, owner, ownGuild.ID, ListAuditLogInput{})
	require.NoError(t, err)
	require.Len(t, created, 1, "creating a guild writes exactly one entry")
	require.Equal(t, ActionGuildCreate, created[0].Action)
	seen[ActionGuildCreate]++

	for _, action := range AllAuditActions {
		if action == ActionGuildDelete {
			continue
		}
		require.NotZero(t, seen[action],
			"%s is in AllAuditActions and no mutation in this test produced one — either a writer was "+
				"removed, or this test stopped exercising the path that writes it", action)
	}

	// The exception, asserted rather than skipped: a deleted guild takes its own entries with it.
	require.NoError(t, f.svc.Delete(ctx, owner, ownGuild.ID), "guild.delete")

	var remaining int
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log_entries WHERE guild_id = $1`,
		int64(ownGuild.ID)).Scan(&remaining))
	require.Zero(t, remaining,
		"the entry is written in the transaction (rule 2) and cascades with the guild; the durable record "+
			"of an instance-level action is rule 14's instance_audit_log, not this table")
}
