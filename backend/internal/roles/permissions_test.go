// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package roles

import "testing"

// TestTheBitOrderIsWhatTheDatabaseAlreadyStores pins every constant to its literal value.
//
// This is the only test in the package that will look like busywork and the only one whose failure is a
// data-loss bug. roles.permissions stores bit positions, so inserting a constant in the middle of the
// block reassigns every permission below it on every guild of every instance already running — with no
// migration to review, no compile error, and no behavioural symptom until somebody notices they can ban
// people. A test asserting "PermBanMembers is bit 6" is the only thing that turns that into a red build.
//
// The values come from docs/architecture.md's "Permission system" block, which is the source of truth.
// Adding a permission means appending a case here with the next bit; changing one that already exists
// means this test is telling you not to.
func TestTheBitOrderIsWhatTheDatabaseAlreadyStores(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  Permission
		bit  int
	}{
		{"PermViewChannel", PermViewChannel, 0},
		{"PermSendMessages", PermSendMessages, 1},
		{"PermManageMessages", PermManageMessages, 2},
		{"PermManageChannels", PermManageChannels, 3},
		{"PermManageRoles", PermManageRoles, 4},
		{"PermKickMembers", PermKickMembers, 5},
		{"PermBanMembers", PermBanMembers, 6},
		{"PermCreateInvite", PermCreateInvite, 7},
		{"PermManageGuild", PermManageGuild, 8},
		{"PermAdministrator", PermAdministrator, 9},
		{"PermConnectVoice", PermConnectVoice, 10},
		{"PermSpeakVoice", PermSpeakVoice, 11},
		{"PermVideoVoice", PermVideoVoice, 12},
		{"PermMuteMembers", PermMuteMembers, 13},
		{"PermDeafenMembers", PermDeafenMembers, 14},
		{"PermMentionEveryone", PermMentionEveryone, 15},
		{"PermManageWebhooks", PermManageWebhooks, 16},
		{"PermManageEmojis", PermManageEmojis, 17},
		{"PermModerateMembers", PermModerateMembers, 18},
	} {
		if want := Permission(1) << tc.bit; tc.got != want {
			t.Errorf("%s = %d, want bit %d (%d) — see the comment above before changing this",
				tc.name, tc.got, tc.bit, want)
		}
	}
}

// TestVideoVoiceStaysReserved is rule 10 as a test.
//
// PermVideoVoice is granted by nothing and checked by nothing, which is exactly what makes it look like
// dead code to a reader tidying up. Removing it renumbers the six constants below it, which is the failure
// the test above describes. Rule 10 forbids it; this asserts it.
func TestVideoVoiceStaysReserved(t *testing.T) {
	if PermVideoVoice != 1<<12 {
		t.Fatalf("PermVideoVoice = %d, want %d", PermVideoVoice, 1<<12)
	}
	if !permAll.Has(PermVideoVoice) {
		t.Error("permAll must include the reserved bit, or an owner would lack a permission the schema defines")
	}
}

// TestPermAllStopsAtTheLastDefinedBit guards the difference between permAll and ^Permission(0).
//
// An owner or an administrator resolves to permAll. If that were every one of the 64 bit positions, a
// milestone defining bit 19 would find it already granted to owners on every existing instance — Has would
// answer true for a permission nobody had written code to grant. Confirmed by removal: replace permAll
// with ^Permission(0) and this fails.
func TestPermAllStopsAtTheLastDefinedBit(t *testing.T) {
	if permAll.Has(1 << 19) {
		t.Error("permAll grants an undefined bit; it must be derived from the last defined constant")
	}
	if !permAll.Has(PermModerateMembers) {
		t.Error("permAll must reach the last defined constant")
	}
	if permAll.Int64() < 0 {
		t.Errorf("permAll = %d as int64; the sign bit must never be set, since Postgres stores a signed bigint",
			permAll.Int64())
	}
}

func TestHasRequiresEveryRequestedBit(t *testing.T) {
	held := PermViewChannel | PermSendMessages

	if !held.Has(PermViewChannel | PermSendMessages) {
		t.Error("both bits held must satisfy a request for both")
	}
	if held.Has(PermViewChannel | PermBanMembers) {
		t.Error("Has must require every requested bit, not any of them")
	}
	if !held.Has(0) {
		t.Error("Has(0) must be true, so an action gated on no particular permission needs no special case")
	}
}

// TestUnknownBitsSurviveARoundTrip covers a schema written by a newer binary and read by an older one.
//
// Masking unknown bits off on read would silently strip a grant the next write persists, which is a
// downgrade quietly deleting data. They are preserved instead.
func TestUnknownBitsSurviveARoundTrip(t *testing.T) {
	const future = int64(1) << 40

	if got := PermissionFromInt64(future).Int64(); got != future {
		t.Errorf("round trip of an undefined bit = %d, want %d", got, future)
	}
	if got := PermissionFromInt64(permAll.Int64()); got != permAll {
		t.Errorf("round trip of permAll = %d, want %d", got, permAll)
	}
}

func TestAddAndRemove(t *testing.T) {
	p := Permission(0).Add(PermViewChannel).Add(PermSendMessages)
	if !p.Has(PermViewChannel | PermSendMessages) {
		t.Fatalf("Add did not set both bits: %d", p)
	}
	if p = p.Remove(PermSendMessages); p.Has(PermSendMessages) {
		t.Errorf("Remove left the bit set: %d", p)
	}
	if !p.Has(PermViewChannel) {
		t.Errorf("Remove cleared a bit it was not asked to: %d", p)
	}
}
