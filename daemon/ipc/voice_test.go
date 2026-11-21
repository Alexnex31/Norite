// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ipc

import "testing"

// A user id is the instance's text for one: never zero, never a leading zero, never past 64 bits, since a
// local mute is kept and compared as that text (M34's manual pass).
func TestAUserIDIsTheInstancesText(t *testing.T) {
	for s, want := range map[string]bool{
		"7": true, "381432364246499328": true, "18446744073709551615": true,
		"0": false, "00": false, "07": false, "": false, "18446744073709551616": false,
		"-7": false, "+7": false, "7 ": false, "seven": false,
	} {
		if got := IsUserID(s); got != want {
			t.Errorf("IsUserID(%q) = %t, want %t", s, got, want)
		}
	}
}
