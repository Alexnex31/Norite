// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

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
