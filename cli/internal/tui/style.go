// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import "charm.land/lipgloss/v2"

// The palette roles docs/design/tui/TOKENS.md names, on the terminal's own ANSI colors, which is the
// default its theme model ships: a tuned terminal is inherited rather than overridden. text is the
// terminal's own foreground, so it carries no color at all. No document fixes which index each role takes;
// this does, and themes (M45) will make it a choice.
var (
	accent  = lipgloss.Color("6")  // cyan: focus, selection, the cursor row
	warn    = lipgloss.Color("3")  // yellow: hints, the armed prefix
	danger  = lipgloss.Color("1")  // red: errors, a channel that is gone
	dim     = lipgloss.Color("8")  // bright black: timestamps, labels, hints
	bright  = lipgloss.Color("15") // bright white: names, active headers
	sAccent = lipgloss.NewStyle().Foreground(accent)
	sBold   = lipgloss.NewStyle().Foreground(bright).Bold(true)
	sDim    = lipgloss.NewStyle().Foreground(dim)
	sWarn   = lipgloss.NewStyle().Foreground(warn)
	sDanger = lipgloss.NewStyle().Foreground(danger)
	sLabel  = lipgloss.NewStyle().Foreground(dim).Bold(true)
	sSelect = lipgloss.NewStyle().Foreground(accent).Bold(true)
)
