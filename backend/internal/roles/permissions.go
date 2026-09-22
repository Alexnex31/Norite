// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package roles resolves what a member of a guild is allowed to do.
//
// # What this package is, and what it deliberately is not
//
// [Resolve] implements layers 2 through 5 of ADR 0008's six-layer authority hierarchy: guild owner, a role
// carrying [PermAdministrator], the OR of every role a member holds, and the permission overwrites on a
// channel. It implements layer 1 — Instance Admin — not at all, and that omission is the ADR's central
// instruction rather than an oversight:
//
//	Instance Admin sits outside any guild's role hierarchy entirely, not resolved via roles.Resolve.
//
// The reason is worth keeping next to the code, because folding it in looks like a simplification. An
// Instance Admin is not a guild member. They hold no roles, appear in no guild_members row, and therefore
// resolve here to exactly zero permissions — which is the correct answer to the question this package
// asks. The question "may this actor perform this action" is a different one, and it is asked by
// guilds.Service.authorize, which checks the instance tier first and calls Resolve second. ADR 0008
// rejected the synthetic "super role" design explicitly.
//
// Layer 6 is not code at all: "guild moderator" is not a separate concept, only any role holding
// [PermManageMessages].
//
// # Deliberately not cached
//
// docs/architecture.md describes Resolve as cached per (guild_id, user_id, channel_id) and invalidated on
// role, overwrite and membership change dispatch. The invalidation signal is a gateway dispatch and the
// gateway does not exist until M18, so the cache would be a permission decision that goes stale with
// nothing to refresh it — a demotion taking effect five minutes late is a security failure, not a slow
// path. One indexed read per check until M18 can invalidate it.
package roles

import (
	"fmt"
	"strconv"
)

// Permission is the bitfield stored in roles.permissions and in the allow/deny columns of
// permission_overwrites.
//
// # The bit order is data, not a detail
//
// These constants are transcribed from docs/architecture.md's "Permission system" block rather than chosen
// here, and the iota order is the part that matters. What the database stores is bit *positions*: a role
// row holding 512 means whatever the tenth constant happens to be. Inserting a constant in the middle, or
// reordering two, silently reassigns every permission every guild on the instance has already granted —
// with no migration to review and no error to catch it. Add new bits at the end, and never renumber.
//
// uint64 in Go against a signed bigint in Postgres, so bit 63 is unavailable and the ceiling is 63
// permissions. Stated here rather than discovered at bit 64; there are twenty-two today.
type Permission uint64

const (
	PermViewChannel Permission = 1 << iota
	PermSendMessages
	PermManageMessages
	PermManageChannels
	PermManageRoles
	PermKickMembers
	PermBanMembers
	PermCreateInvite
	PermManageGuild
	// PermAdministrator grants everything within one guild and short-circuits resolution (ADR 0008
	// layer 3). It is scoped to the guild that granted it and confers nothing anywhere else — that is the
	// distinction from layer 1, which is an instance-wide tier.
	PermAdministrator
	PermConnectVoice
	PermSpeakVoice
	// PermVideoVoice is reserved and must not be removed (CLAUDE.md rule 10). Video and screen-share are
	// the one deferred-but-seamed piece of the voice stack: nothing grants this bit yet, and deleting it
	// would renumber every constant below it — see the note on bit order above for what that costs.
	PermVideoVoice
	PermMuteMembers
	PermDeafenMembers
	PermMentionEveryone
	PermManageWebhooks
	PermManageEmojis
	// PermModerateMembers is M74's: time a member out without suspending the account. Defined here
	// because the bit order is fixed by the specification and leaving a gap to fill later is the
	// renumbering this comment warns about.
	PermModerateMembers
	// PermViewAuditLog is M14's, and it is a bit of its own rather than a shape of PermManageGuild.
	//
	// Discord makes the same split, and the reason is the same: the log names every moderation action
	// and who took it, which is exactly what an instance's moderators need to read and exactly what
	// nobody else should. Folding it into PermManageGuild would mean the only way to let somebody audit
	// the guild is to let them rename it, change its settings and edit its invites — the shape M12's
	// UpdateMember had to be corrected out of, where a permission only ever OR'd into a base is not a
	// grantable permission at all.
	PermViewAuditLog

	// PermReadMessageHistory is M15's, and it is a separate bit from PermViewChannel for the reason
	// Discord separates them: seeing that a channel exists and reading what was said in it before you
	// arrived are different grants, and a guild that wants the second without the first has nothing to
	// configure if they are one bit.
	//
	// The three message bits compose rather than nest. PermViewChannel puts the channel in your sidebar;
	// PermSendMessages lets you post into it; this one lets you read the backlog. An announcements
	// channel that everybody may read and nobody may post in is view+history without send. A channel
	// whose history is private to the people who were there — a support thread, a moderation room opened
	// to a reporter — is view+send without history: you can see it and take part, and the conversation
	// that happened before you were added is not yours to read.
	//
	// Added at the end, like every bit before it. The order is data: `roles.permissions` stores bit
	// positions, so inserting in the middle reassigns every permission every guild has already granted,
	// with no migration and no compile error to notice it.
	PermReadMessageHistory

	// PermViewMessageAudit is M16b's: read the log a guild produces when its owner switches recording on.
	//
	// It is a bit of its own for M14's reason one surface later, and the three bits it could have reused
	// are each wrong in a different way — which is the useful part, because "audit" in the name makes the
	// first of them look obvious.
	//
	// **Not PermViewAuditLog.** That bit reads moderation *metadata*: who kicked whom, which permission
	// changed, what a nickname used to be. This reads the conversations themselves. The ledger entry M14
	// wrote for that surface ends with its own reopening condition — "the log ever carries message
	// content, where the exposure stops being metadata about moderation and becomes the conversations
	// themselves" — and reusing the bit here would answer the letter of it (this is a different table)
	// while doing exactly the thing it names. Somebody granted the bit to see who kicked whom would find
	// they had been granted every private channel in the guild.
	//
	// **Not PermManageMessages.** That is M16a's gate and it means "delete somebody else's message",
	// which is moderation with a visible outcome on one message at a time. A guild that wants a moderator
	// able to remove spam has not thereby decided that moderator reads the owner's private channel. M16a's
	// own reach turned out wider than the decision it borrowed from M16; borrowing it again and widening
	// it from one message's prior versions to every message in the guild is that mistake with a much
	// larger radius.
	//
	// **Not folded into PermManageGuild**, which is the owner-delegated settings bit that flips the switch
	// in the first place. Folding it in would mean the only way to let somebody read the log is to let
	// them rename the guild, change its settings and turn the recording off again — verbatim the
	// correction M12's UpdateMember needed, where a permission only ever OR'd into a base is not a
	// grantable permission at all.
	//
	// Granted by default to nobody, implied by nothing, and un-grantable by somebody who does not hold it
	// (refuseEscalation already covers every bit). That is the whole boundary on the widest disclosure
	// this project has taken — see docs/security-ledger.md, which states it rather than leaving it to be
	// inherited from M16's.
	PermViewMessageAudit
)

// permAll is every defined bit, and what an owner or an administrator resolves to.
//
// Deliberately not ^Permission(0). A member who short-circuits would otherwise hold every one of the 64
// bit positions, including the 42 nothing has defined — so a later milestone defining bit 22 would find it
// already granted to owners in a way no code says out loud, and Has would answer true for a permission
// that did not exist when the check was written. This value is derived from the last defined constant, so
// adding one at the end extends it and adding one in the middle is still the renumbering hazard above.
//
// The two numbers in that sentence move every time a bit is added, and they are written out rather than
// computed because the whole point is that somebody has to edit them and notice. M14 moved them for
// PermViewAuditLog, M15 for PermReadMessageHistory, M16b for PermViewMessageAudit.
const permAll = Permission(PermViewMessageAudit<<1 - 1)

// Known reports whether p sets only bits this build defines.
//
// permAll is unexported because nothing outside this package should be able to name "everything"; this is
// the question a caller legitimately has, asked on the *input* path. An owner or an administrator resolving
// to permAll is fine — it stops at the last defined bit by construction — but a value arriving from a
// request has not been through that, and UnmarshalJSON deliberately preserves unknown high bits so a row
// written by a newer schema survives a round trip through an older binary.
//
// Preserving an unknown bit on read and *accepting* one on write are different decisions. Storing one
// means a later milestone defining that bit finds it already granted, in a way no code says out loud —
// verbatim the failure permAll's comment rejects `^Permission(0)` to prevent.
func (p Permission) Known() bool { return p&^permAll == 0 }

// Has reports whether every bit in need is present in p.
//
// Every bit, not any: a call site asking for two permissions means both. Has(0) is true, which is what
// makes an action that requires no particular permission expressible without a special case.
func (p Permission) Has(need Permission) bool { return p&need == need }

// Add returns p with every bit in other set.
func (p Permission) Add(other Permission) Permission { return p | other }

// Remove returns p with every bit in other cleared.
func (p Permission) Remove(other Permission) Permission { return p &^ other }

// Int64 converts to the representation Postgres stores, for a bigint column.
//
// Lossless in both directions for every defined bit, because bit 63 is never set: permAll stops at the
// last defined constant and nothing else constructs a Permission with the top bit. The conversion would
// otherwise produce a negative bigint, which sorts and compares in ways no caller expects.
func (p Permission) Int64() int64 { return int64(p) }

// PermissionFromInt64 converts a value read out of Postgres.
//
// Bits with no constant are preserved rather than masked off. A row written by a future version of this
// schema and read by an older binary keeps its unknown grants intact instead of having them silently
// stripped on the next write — the same reasoning that makes Remove explicit about what it clears.
func PermissionFromInt64(v int64) Permission { return Permission(v) }

// MarshalJSON renders the bitfield as a quoted decimal string.
//
// The same decision ADR 0003 makes for snowflakes, for the same reason: this is a 63-bit value and
// JavaScript's number type is a float64, so anything above 2^53 loses precision silently on the way
// through a browser. Twenty-one bits are defined today and the hazard is years away — which is exactly when
// it is cheap to fix, because changing the wire type later is a breaking change across four codegen'd
// clients. Discord made this change under load rather than ahead of it.
//
// Decimal rather than hex, matching how the value appears in Postgres and in every log line.
func (p Permission) MarshalJSON() ([]byte, error) {
	return []byte(`"` + strconv.FormatUint(uint64(p), 10) + `"`), nil
}

// UnmarshalJSON accepts the quoted decimal string MarshalJSON produces.
//
// A bare JSON number is refused rather than accepted leniently. Accepting both would mean a client that
// sends a number works until the day a permission bit above 2^53 is defined and its values start arriving
// silently rounded — a bug that appears years after the code that caused it, in a different client, as a
// wrong permission rather than as an error.
func (p *Permission) UnmarshalJSON(data []byte) error {
	if len(data) < 2 || data[0] != '"' || data[len(data)-1] != '"' {
		return fmt.Errorf("roles: permissions must be a quoted decimal string, got %s", data)
	}

	v, err := strconv.ParseUint(string(data[1:len(data)-1]), 10, 64)
	if err != nil {
		return fmt.Errorf("roles: invalid permissions value: %w", err)
	}

	// The sign bit is unavailable because Postgres stores this as a signed bigint, so a value that would
	// round-trip as negative is refused here rather than at the INSERT.
	//
	// This is the *only* condition. An earlier version read `v > uint64(permAll) && v>>63 != 0`, whose
	// first clause is implied by the second — v >= 2^63 is unconditionally greater than 2^19-1 — so it
	// changed nothing while reading as a second rule. Left as it was, the obvious "fix" is to make it an
	// `||`, which would start rejecting exactly the unknown high bits PermissionFromInt64 promises to
	// preserve across a round trip.
	if v>>63 != 0 {
		return fmt.Errorf("roles: permissions value %d does not fit a signed bigint", v)
	}

	*p = Permission(v)
	return nil
}
