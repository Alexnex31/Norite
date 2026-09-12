// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"context"
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
	var cursor *snowflake.ID
	for {
		page, err := f.svc.ListAuditLog(ctx, userActor(f.owner), f.guildID,
			ListAuditLogInput{Before: cursor, Limit: 2})
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		paged = append(paged, page...)
		cursor = &page[len(page)-1].ID
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
	require.Equal(t, map[string]any{"to": "staff-only"}, changes["name"],
		"including the name of a channel they cannot open")
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
		ListAuditLogInput{ActorID: &f.mod})
	require.NoError(t, err)
	require.NotEmpty(t, byActor)
	for _, e := range byActor {
		require.Equal(t, f.mod, e.ActorID)
	}

	both, err := f.svc.ListAuditLog(ctx, userActor(f.mod), f.guildID,
		ListAuditLogInput{Action: ActionChannelCreate, ActorID: &f.owner})
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

	entries := driveEveryMutation(t, f, ctx)

	seen := map[string]int{}
	for _, e := range entries {
		seen[e.Action]++
	}

	for _, action := range AuditActions() {
		if action == ActionGuildDelete {
			continue
		}
		require.NotZero(t, seen[action],
			"%s is in the action vocabulary and no mutation in this test produced one — either a writer "+
				"was removed, or this test stopped exercising the path that writes it", action)
	}
}

// driveEveryMutation exercises every mutation the package exposes and returns the guild's whole audit log.
//
// Shared by the coverage test above and the shape test below, which is not merely deduplication: they had
// each grown their own sequence, the shape test's was four actions short, and the gap was invisible
// because its doc comment offloaded coverage to the other test — which counts actions and never decodes a
// payload. One sequence means a seventeenth action is added in one place and both assertions see it.
//
// guild.create and guild.delete need a guild of their own, because the fixture inserts its one directly
// and a guild cannot be created twice. They are also the pair that demonstrates the table's one
// asymmetry, so the entries from that guild are collected before it is deleted and appended here.
func driveEveryMutation(t *testing.T, f *overwriteFixture, ctx context.Context) []AuditLogEntry {
	t.Helper()
	owner := userActor(f.owner)

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

	// A second PUT over the same row, so overwrite.set is exercised as a replacement as well as a
	// creation — the two produce different payload shapes and only one of them was ever reached.
	_, err = f.svc.SetOverwrite(ctx, owner, SetOverwriteInput{
		ChannelID: ch.ID, TargetType: roles.OverwriteTargetRole, TargetID: f.everyoneID,
		Deny: roles.PermSendMessages | roles.PermMentionEveryone,
	})
	require.NoError(t, err, "overwrite.set, replacing")

	require.NoError(t, f.svc.DeleteOverwrite(
		ctx, owner, ch.ID, roles.OverwriteTargetRole, f.everyoneID), "overwrite.delete")

	doomedRole, err := f.svc.CreateRole(ctx, owner, f.guildID, CreateRoleInput{Name: "doomed"})
	require.NoError(t, err)
	require.NoError(t, f.svc.DeleteRole(ctx, owner, f.guildID, doomedRole.ID), "role.delete")

	require.NoError(t, f.svc.RemoveMember(ctx, owner, f.guildID, f.plain), "member.remove")

	// The guild's whole log, in pages, because there are more than the default page size by now.
	var all []AuditLogEntry
	var cursor *snowflake.ID
	for {
		page, err := f.svc.ListAuditLog(ctx, owner, f.guildID,
			ListAuditLogInput{Before: cursor, Limit: maxAuditLogPageSize})
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		all = append(all, page...)
		cursor = &page[len(page)-1].ID
	}

	// A guild made through the service, for the two actions the fixture's own guild cannot produce.
	ownGuild, err := f.svc.Create(ctx, owner, CreateGuildInput{Name: "made by the service"})
	require.NoError(t, err)

	created, err := f.svc.ListAuditLog(ctx, owner, ownGuild.ID, ListAuditLogInput{})
	require.NoError(t, err)
	require.Len(t, created, 1, "creating a guild writes exactly one entry")
	require.Equal(t, ActionGuildCreate, created[0].Action)
	all = append(all, created...)

	// And the exception, asserted rather than skipped: a deleted guild takes its own entries with it.
	require.NoError(t, f.svc.Delete(ctx, owner, ownGuild.ID), "guild.delete")

	var remaining int
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log_entries WHERE guild_id = $1`,
		int64(ownGuild.ID)).Scan(&remaining))
	require.Zero(t, remaining,
		"the entry is written in the transaction (rule 2) and cascades with the guild; the durable record "+
			"of an instance-level action is rule 14's instance_audit_log, not this table")

	return all
}

// TestTheAuditDiffShapeIsUniform is the rule auditDiff states, enforced across every action.
//
// Two kinds of key and one question per key: a changed field is an object carrying `from`, `to` or both;
// a context field is a scalar. A renderer relies on that split, and a comment saying so is what the
// seventeenth mutation breaks — the sixteenth already nearly did, since `renumbered` and `role_id` are
// both context and both look at a glance like fields somebody forgot to diff.
//
// It reuses the coverage test's fixture work by driving the same mutations, then walks every entry the
// guild produced rather than a chosen few. An action added later and left out of the drive-through is
// caught by TestEveryAuditActionIsReachable, not here; this one asserts the shape of what does arrive.
func TestTheAuditDiffShapeIsUniform(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()

	// The same drive-through the coverage test uses, so the two cannot disagree about which actions
	// exist. They had already drifted: this test ran its own shorter sequence and never reached
	// member.update, member.remove, member.role_remove or guild.create, so four of the sixteen payloads
	// had their shape asserted by nothing — while the doc comment claimed "every action".
	entries := driveEveryMutation(t, f, ctx)
	require.NotEmpty(t, entries)

	diffed := 0
	for _, e := range entries {
		if e.Changes == nil {
			continue
		}

		var payload map[string]any
		require.NoError(t, json.Unmarshal(e.Changes, &payload), e.Action)
		require.NotEmpty(t, payload, "%s: an empty object and null would be two spellings of nothing", e.Action)

		for field, value := range payload {
			object, isObject := value.(map[string]any)
			if !isObject {
				// Context. Asserted as a scalar rather than merely "not an object", so that a future
				// context value which is a list does not quietly become unreadable to the same rule.
				require.Equal(t, "scalar", kindOf(value),
					"%s.%s is context and must be a scalar", e.Action, field)
				continue
			}

			diffed++
			from, hasFrom := object["from"]
			to, hasTo := object["to"]
			require.True(t, hasFrom || hasTo,
				"%s.%s is an object, so it reads as a diff, and carries neither side", e.Action, field)
			for key := range object {
				require.Contains(t, []string{"from", "to"}, key,
					"%s.%s carries %q; a diff has exactly from and to", e.Action, field, key)
			}
			if hasFrom && hasTo {
				// A recorded change whose sides are equal is what a type mismatch inside changed() looks
				// like from out here — see sameValue. It is noise rather than a wrong answer, and noise on
				// the endpoint whose job is to be believed.
				require.NotEqual(t, from, to,
					"%s.%s records a change from a value to itself", e.Action, field)
			}
		}
	}

	require.Greater(t, diffed, 0, "or this walked nothing but context and asserted nothing")
}

// kindOf names a decoded JSON value's shape, for the assertion above.
func kindOf(v any) string {
	switch v.(type) {
	case map[string]any:
		return "map"
	case []any:
		return "slice"
	default:
		return "scalar"
	}
}

// TestAPermissionInAnAuditEntryIsAString is the wire-type rule M12 settled, on the one surface that was
// still breaking it.
//
// Permissions cross the wire as quoted decimal strings because the field is 63 bits and a browser's
// number is a float64. M12 and M13 wrote in.Permissions.Int64() into the audit payload, which rendered it
// as a JSON number — a client reading this log through a browser would have had the same silent rounding
// the representation exists to prevent, on the one endpoint whose purpose is to be believed.
func TestAPermissionInAnAuditEntryIsAString(t *testing.T) {
	t.Parallel()
	f := newOverwriteFixture(t, roles.PermViewChannel)
	ctx := t.Context()
	owner := userActor(f.owner)

	_, err := f.svc.CreateRole(ctx, owner, f.guildID,
		CreateRoleInput{Name: "a-role", Permissions: roles.PermViewChannel | roles.PermSendMessages})
	require.NoError(t, err)

	entries, err := f.svc.ListAuditLog(ctx, owner, f.guildID,
		ListAuditLogInput{Action: ActionRoleCreate})
	require.NoError(t, err)
	require.Len(t, entries, 1)

	// map[string]any rather than map[string]map[string]any: the payload mixes diffs with context, and
	// role.create's `renumbered` is a bool. That is the shape the test above pins, so decoding it as
	// uniformly nested here would be this test disagreeing with that one.
	var payload map[string]any
	require.NoError(t, json.Unmarshal(entries[0].Changes, &payload))

	permissions, ok := payload["permissions"].(map[string]any)
	require.True(t, ok, "permissions is a created field, so it is an object carrying `to`")
	require.Equal(t, "3", permissions["to"],
		"a quoted decimal string, as roles.Permission marshals itself — not a JSON number")
}
