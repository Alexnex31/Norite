// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ipc

import (
	"strings"
	"testing"
)

// A user id is the instance's text for one: never zero, never a leading zero, never past a signed 64 bits,
// since a local mute is kept and compared as that text (M34's manual pass) and the voice server refuses an
// id past what a snowflake holds (M34's /code-review).
func TestAUserIDIsTheInstancesText(t *testing.T) {
	for s, want := range map[string]bool{
		"7": true, "381432364246499328": true, "9223372036854775807": true,
		"0": false, "00": false, "07": false, "": false, "9223372036854775808": false,
		"18446744073709551615": false,
		"-7":                   false, "+7": false, "7 ": false, "seven": false,
	} {
		if got := IsUserID(s); got != want {
			t.Errorf("IsUserID(%q) = %t, want %t", s, got, want)
		}
	}
}

// A push-to-talk key is written one way, whatever way it was typed, and a key that cannot be registered on
// any OS, or that typing would hold, is refused with the default named (M35).
func TestAPushToTalkKeyIsWrittenOneWay(t *testing.T) {
	for in, want := range map[string]string{
		"ctrl+shift+space": "ctrl+shift+space",
		"Shift+Ctrl+Space": "ctrl+shift+space",
		"super+alt+p":      "alt+super+p",
		"ctrl+7":           "ctrl+7",
		"f13":              "f13",
		"shift+F24":        "shift+f24",
		"alt+f1":           "alt+f1",
	} {
		got, err := CanonicalKey(in)
		if err != nil || got != want {
			t.Errorf("%q: %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "space", "shift+a", "a", "ctrl+", "+ctrl+a", "ctrl+ctrl+a", "ctrl+hyper+a", "ctrl+f0", "ctrl+f25",
		"ctrl+f01", "ctrl+enter", "ctrl+a+b", "ctrl+é", strings.Repeat("ctrl+", 20) + "a",
	} {
		if got, err := CanonicalKey(in); err == nil || !strings.Contains(err.Error(), DefaultPushToTalkKey) {
			t.Errorf("%q: %q, %v; want refused, naming the default", in, got, err)
		}
	}
}
