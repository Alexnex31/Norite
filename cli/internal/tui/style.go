// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"charm.land/lipgloss/v2"

	"github.com/Alexnex31/Norite/daemon/config"
)

// look is how the client draws: the palette roles docs/design/tui/TOKENS.md names, as styles, and the
// layout a time of day is written in. text is the terminal's own foreground, so it carries no color at all.
//
// It is built once for each time config.toml is read (M21) and shared by home and the pane, never per
// frame: a frame is drawn on every keystroke, and seven styles rebuilt for each would be paid there. The
// defaults are the terminal's own ANSI colors, so a tuned terminal is inherited rather than overridden;
// `[tui.colors]` moves a role to another index or to an exact color, and themes (M45) will name sets of
// them.
type look struct {
	accent   lipgloss.Style // focus, the resolved-invite mark
	bold     lipgloss.Style // names, active headers
	dim      lipgloss.Style // timestamps, hints
	warn     lipgloss.Style // hints that matter, the armed prefix
	danger   lipgloss.Style // errors, a channel that is gone
	label    lipgloss.Style // section labels
	selected lipgloss.Style // the cursor row

	clock string // time.Format's layout for a time of day
}

// defaultLook is what a client draws with before, or without, a config: the contract's defaults.
var defaultLook = newLook(config.Defaults())

func newLook(c *config.Config) *look {
	color := func(key string) lipgloss.Style {
		// Validated by the loader as an ANSI index or #rrggbb, the two forms lipgloss reads.
		return lipgloss.NewStyle().Foreground(lipgloss.Color(string(c.Color(key))))
	}
	accent, dim := color(config.KeyColorAccent), color(config.KeyColorDim)
	l := &look{
		accent:   accent,
		bold:     color(config.KeyColorBright).Bold(true),
		dim:      dim,
		warn:     color(config.KeyColorWarn),
		danger:   color(config.KeyColorDanger),
		label:    dim.Bold(true),
		selected: accent.Bold(true),
		clock:    "15:04",
	}
	if c.Clock() == config.Clock12h {
		l.clock = "3:04 PM"
	}
	return l
}
