// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// maxName bounds every name this client keeps — a guild's, a channel's, a username, a display name — at the
// longest the instance's own validators accept (100 runes, for guild and channel names). What the client
// holds is bounded against the instance rather than by it: the instance is whatever URL somebody signed in
// to, and its own validation is not this program's bound (M19's rule for the daemon, here for the client).
const maxName = 100

// cut keeps at most n runes of s, marking a cut with "…". A correct instance never sends a value cut here,
// so the mark appears only on one that broke its own bounds.
func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// clip cuts a row to the width, counting display cells and leaving styling intact, so no row wraps and
// shears the frame.
func clip(s string, width int) string {
	return ansi.Truncate(s, max(width, 0), "…")
}

// fit makes a block exactly height rows, cutting from the top: the bottom of a frame is what a person is
// looking at.
func fit(lines []string, height int) string {
	var flat []string
	for _, l := range lines {
		flat = append(flat, strings.Split(l, "\n")...)
	}
	if len(flat) > height {
		flat = flat[len(flat)-height:]
	}
	for len(flat) < height {
		flat = append(flat, "")
	}
	return strings.Join(flat, "\n")
}

// compareIDs orders snowflakes numerically: a shorter decimal string is a smaller number, and two of one
// length compare as text. Every id compared here has passed ops.IsID.
func compareIDs(a, b string) int {
	if len(a) != len(b) {
		return len(a) - len(b)
	}
	return strings.Compare(a, b)
}
