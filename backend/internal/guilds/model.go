// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// Audit actions, as written to audit_log_entries.action.
//
// A stable string vocabulary rather than a Postgres enum, so adding one is not a migration — see
// migration 000016. `domain.verb`, lower-case, because these are read by an operator scanning a log and
// grouped by prefix more often than they are read individually.
//
// Nothing consumes these until M14 builds the audit-log endpoint. They are defined here anyway rather
// than as literals at each call site, because the coverage test M14 owes — every mutation type produces
// exactly one entry — needs a list to iterate, and a list assembled from string literals scattered across
// four handler groups is a list that silently misses the one somebody typed differently.
const (
	ActionGuildCreate   = "guild.create"
	ActionGuildUpdate   = "guild.update"
	ActionGuildDelete   = "guild.delete"
	ActionChannelCreate = "channel.create"
	ActionChannelUpdate = "channel.update"
	ActionChannelDelete = "channel.delete"
	ActionRoleCreate    = "role.create"
	ActionRoleUpdate    = "role.update"
	ActionRoleDelete    = "role.delete"
	// One entry for a whole reorder rather than one per row moved: the operation is a single atomic
	// rearrangement and splitting it would make an operator reading the log reconstruct which entries
	// belonged together.
	ActionRoleReorder  = "role.reorder"
	ActionMemberUpdate = "member.update"
	ActionMemberRemove = "member.remove"

	// Assignment is its own pair of verbs rather than a shape of member.update, because what an operator
	// reads this log for is "who was given what", and burying that in a changes field makes it a scan.
	ActionMemberRoleAdd    = "member.role_add"
	ActionMemberRoleRemove = "member.role_remove"

	// One action for writing an overwrite rather than separate create and update verbs, because the
	// endpoint is a PUT and does not distinguish them either. What changed is in the entry's `changes`.
	ActionOverwriteSet    = "overwrite.set"
	ActionOverwriteDelete = "overwrite.delete"
)

// AllAuditActions is every verb above, which is the list the block's own comment says M14 would need.
//
// Two callers and they want it for opposite reasons. The listing endpoint validates an `action` filter
// against it, because an unknown verb is a query matching nothing and therefore indistinguishable from a
// guild that has taken no such action — a typo that reads as evidence. And the coverage test walks it to
// assert every verb is actually reachable, which is the half a list of literals scattered across four
// handler groups cannot give: a constant nothing writes looks identical to one whose writer was removed.
//
// Appending a constant above without appending it here is what the coverage test exists to catch.
//
// Unexported, and read through [AuditActions], because an exported slice is not a closed vocabulary: any
// importer could append to it or nil it, and since knownAuditAction ranges over it, doing so would change
// what the endpoint accepts process-wide — a nil would refuse every action filter.
var allAuditActions = []string{
	ActionGuildCreate,
	ActionGuildUpdate,
	ActionGuildDelete,
	ActionChannelCreate,
	ActionChannelUpdate,
	ActionChannelDelete,
	ActionRoleCreate,
	ActionRoleUpdate,
	ActionRoleDelete,
	ActionRoleReorder,
	ActionMemberUpdate,
	ActionMemberRemove,
	ActionMemberRoleAdd,
	ActionMemberRoleRemove,
	ActionOverwriteSet,
	ActionOverwriteDelete,
}

// AuditActions returns every action this build writes.
//
// A copy, so a caller ranging over it for a test table cannot reorder or truncate the vocabulary the
// listing endpoint validates against.
func AuditActions() []string { return slices.Clone(allAuditActions) }

// Channel types, as stored in channels.type.
//
// The reserved values are present and must stay (rule 10): GUILD_STAGE_VOICE and GUILD_ANNOUNCEMENT are
// deferred-but-seamed, and removing either renumbers nothing — these are explicit values rather than iota
// — but does delete the seam. M12 accepts only the three a guild can actually contain today.
const (
	ChannelGuildText         int16 = 0
	ChannelDM                int16 = 1
	ChannelGuildVoice        int16 = 2
	ChannelGroupDM           int16 = 3
	ChannelGuildCategory     int16 = 4
	ChannelGuildAnnouncement int16 = 5
	ChannelGuildStageVoice   int16 = 6
	ChannelPublicMatchmaking int16 = 7
)

// Guild is the wire representation of a guild.
type Guild struct {
	ID          snowflake.ID `json:"id"`
	Name        string       `json:"name"`
	OwnerID     snowflake.ID `json:"owner_id"`
	IconHash    *string      `json:"icon_hash"`
	Description *string      `json:"description"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

func guildFromRow(row db.Guild) Guild {
	// Field by field rather than a struct conversion, matching auth's newTokenPairResponse: a conversion
	// compiles only while the two types keep identical fields in identical order, and starts silently
	// mis-assigning the moment either gains one.
	return Guild{
		ID:          snowflake.ID(row.ID),
		Name:        row.Name,
		OwnerID:     snowflake.ID(row.OwnerID),
		IconHash:    row.IconHash,
		Description: row.Description,
		CreatedAt:   row.CreatedAt.Time,
		UpdatedAt:   row.UpdatedAt.Time,
	}
}

// Role is the wire representation of a role.
type Role struct {
	ID      snowflake.ID `json:"id"`
	GuildID snowflake.ID `json:"guild_id"`
	Name    string       `json:"name"`
	Color   int32        `json:"color"`
	// A quoted decimal string on the wire, not a number — see roles.Permission.MarshalJSON.
	Permissions roles.Permission `json:"permissions"`
	Position    int32            `json:"position"`
	Hoist       bool             `json:"hoist"`
	Mentionable bool             `json:"mentionable"`
	IsDefault   bool             `json:"is_default"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
}

func roleFromRow(row db.Role) Role {
	return Role{
		ID:          snowflake.ID(row.ID),
		GuildID:     snowflake.ID(row.GuildID),
		Name:        row.Name,
		Color:       row.Color,
		Permissions: roles.PermissionFromInt64(row.Permissions),
		Position:    row.Position,
		Hoist:       row.Hoist,
		Mentionable: row.Mentionable,
		IsDefault:   row.IsDefault,
		CreatedAt:   row.CreatedAt.Time,
		UpdatedAt:   row.UpdatedAt.Time,
	}
}

// Channel is the wire representation of a channel.
//
// topic_search is absent by construction: the queries do not select it. It is a generated search index,
// not content, and nothing outside M63's search will ever want it.
type Channel struct {
	ID            snowflake.ID  `json:"id"`
	GuildID       *snowflake.ID `json:"guild_id"`
	Type          int16         `json:"type"`
	ParentID      *snowflake.ID `json:"parent_id"`
	Name          *string       `json:"name"`
	Topic         *string       `json:"topic"`
	Position      int32         `json:"position"`
	NSFW          bool          `json:"nsfw"`
	LastMessageID *snowflake.ID `json:"last_message_id"`
	Bitrate       *int32        `json:"bitrate"`
	UserLimit     *int32        `json:"user_limit"`
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`

	// PermissionOverwrites is every overwrite on this channel — ADR 0008 layer 5, as configured rather
	// than as resolved.
	//
	// Embedded rather than served from a route of its own, because a permission editor cannot be drawn
	// from a listing that omits them and there is no GET /channels/{id} to ask instead. The listing
	// already reads every channel in the guild, so one more query answers the whole sidebar; a per-channel
	// fetch would be a request each time somebody opened a tab, which is the chattiness rule 21 asks to be
	// weighed at the time an endpoint is added.
	//
	// Never nil. An omitted array and an empty one would be one schema meaning two things depending on
	// which route produced it, which is the bug M12 shipped on a member's `roles` and had to correct — a
	// client refreshing its cache from a response is entitled to read this as the whole truth.
	PermissionOverwrites []Overwrite `json:"permission_overwrites"`
}

// Member is the wire representation of a guild membership.
type Member struct {
	GuildID  snowflake.ID `json:"guild_id"`
	UserID   snowflake.ID `json:"user_id"`
	Nickname *string      `json:"nickname"`
	JoinedAt time.Time    `json:"joined_at"`
	// Server-side mute and deafen, set by a moderator. Distinct from the self_mute/self_deaf a client sets
	// on itself, which lives in voice_states from M25.
	Deaf  bool           `json:"deaf"`
	Mute  bool           `json:"mute"`
	Roles []snowflake.ID `json:"roles"`
}

func memberFromRow(row db.GuildMember, roleIDs []snowflake.ID) Member {
	if roleIDs == nil {
		// An empty array rather than null. A client that has to handle both writes the check once per
		// field and forgets it somewhere; the same reasoning as the session listing at M11.
		roleIDs = []snowflake.ID{}
	}

	return Member{
		GuildID:  snowflake.ID(row.GuildID),
		UserID:   snowflake.ID(row.UserID),
		Nickname: row.Nickname,
		JoinedAt: row.JoinedAt.Time,
		Deaf:     row.Deaf,
		Mute:     row.Mute,
		Roles:    roleIDs,
	}
}

// AuditLogEntry is one entry of a guild's audit log, on the wire.
//
// # No guild_id, deliberately
//
// Every other payload in this package carries one and this does not. The column is nullable — §2 keeps it
// that way so an instance-scoped action could share the table, though rule 14 gives those their own — and
// the route is already guild-scoped, so the field would be the path parameter repeated with a null the
// client must handle and can never receive. Discord's audit-log entry object omits it for the same reason.
//
// # changes is raw
//
// It is passed through as stored rather than decoded and re-encoded. The shape is per-action and adding a
// field to one writer must not mean teaching this type about it — that coupling is what would make the
// diffing work in A3 a change to the read path as well as the write path.
type AuditLogEntry struct {
	ID       snowflake.ID  `json:"id"`
	ActorID  snowflake.ID  `json:"actor_id"`
	Action   string        `json:"action"`
	TargetID *snowflake.ID `json:"target_id"`
	// Changes is the per-action detail, or null. json.RawMessage so it crosses the wire as written.
	Changes   json.RawMessage `json:"changes"`
	CreatedAt time.Time       `json:"created_at"`
}

func auditLogEntryFromRow(row db.AuditLogEntry) AuditLogEntry {
	// An empty non-nil slice is the one value that would emit invalid JSON: json.RawMessage marshals nil
	// as null and anything else verbatim, so a zero-length slice writes zero bytes where a value belongs
	// and corrupts the whole response rather than this field. writeAudit leaves the column NULL when there
	// is nothing to record, so this is unreachable today — which is exactly the sort of thing that stops
	// being true when a second writer appears.
	changes := json.RawMessage(row.Changes)
	if len(changes) == 0 {
		changes = nil
	}

	return AuditLogEntry{
		ID:        snowflake.ID(row.ID),
		ActorID:   snowflake.ID(row.ActorID),
		Action:    row.Action,
		TargetID:  idPtr(row.TargetID),
		Changes:   changes,
		CreatedAt: row.CreatedAt.Time,
	}
}

// idPtr converts a nullable bigint from the database into a nullable snowflake for the wire.
func idPtr(v *int64) *snowflake.ID {
	if v == nil {
		return nil
	}
	id := snowflake.ID(*v)
	return &id
}

// Overwrite is the wire representation of one channel permission overwrite.
//
// Three of its four values are 64-bit and every one of them marshals as a quoted string. target_id is a
// snowflake (ADR 0003); allow and deny are permission bitfields, which M12 settled as quoted decimal for
// the same reason — a 63-bit field and a float64 do not mix above 2^53, and a client that silently drops
// the top bits of a permission set is worse than one that fails to parse.
type Overwrite struct {
	ChannelID snowflake.ID     `json:"channel_id"`
	Type      int16            `json:"type"`
	TargetID  snowflake.ID     `json:"target_id"`
	Allow     roles.Permission `json:"allow"`
	Deny      roles.Permission `json:"deny"`
}

func overwriteFromRow(row db.PermissionOverwrite) Overwrite {
	return Overwrite{
		ChannelID: snowflake.ID(row.ChannelID),
		Type:      row.TargetType,
		TargetID:  snowflake.ID(row.TargetID),
		Allow:     roles.PermissionFromInt64(row.Allow),
		Deny:      roles.PermissionFromInt64(row.Deny),
	}
}
