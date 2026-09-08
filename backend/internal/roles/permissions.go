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
// permissions. Stated here rather than discovered at bit 64; there are nineteen today.
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
)

// permAll is every defined bit, and what an owner or an administrator resolves to.
//
// Deliberately not ^Permission(0). A member who short-circuits would otherwise hold every one of the 64
// bit positions, including the 45 nothing has defined — so a later milestone defining bit 20 would find it
// already granted to owners in a way no code says out loud, and Has would answer true for a permission
// that did not exist when the check was written. This value is derived from the last defined constant, so
// adding one at the end extends it and adding one in the middle is still the renumbering hazard above.
const permAll = Permission(PermModerateMembers<<1 - 1)

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
