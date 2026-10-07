// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config is the client's config.toml: where it is, which keys it may hold, and how it is read.
//
// It is outside internal/ because every client reads the same file through it: the terminal client today,
// the GUI later, and the command tree's `norite config` verbs. The file is a person's, hand-edited and
// often kept in a dotfiles repository, so this package treats it as something it shares rather than owns.
//
// The keys are listed in contracts/client-config.toml, which this file mirrors. A test holds the two equal
// in both directions.
package config

import "strings"

// Section is a top-level table of config.toml.
type Section string

// The three sections. There is no "cli": the command tree has nothing to style (ADR 0026).
const (
	Shared Section = "shared"
	TUI    Section = "tui"
	GUI    Section = "gui"
)

// Kind is what a key's value must be.
type Kind string

// The kinds a key can have, as the contract names them.
const (
	KindEnum   Kind = "enum"
	KindColor  Kind = "color"
	KindString Kind = "string"
	KindTable  Kind = "table"
)

// Key describes one setting.
type Key struct {
	// Name is the key without its section, dotted: "colors.accent".
	Name string
	// Section is where it is set. A Shared key may also be set under a client's section, which overrides.
	Section Section
	Kind    Kind
	// Values lists what an enum accepts.
	Values []string
	// Default is what applies when the key is absent, spelled as the file would spell it. Empty for a
	// table and for a key with no consumer yet.
	Default string
	// Portable keys are exported and may be imported. The rest are machine-local.
	Portable bool
	// Consumer is the milestone that gives the key a reader.
	Consumer string
	// Live is true once that reader exists. A key that is not live is kept and acted on by nothing.
	Live bool
}

// The names of the live keys, for the callers that read them.
const (
	KeyClock       = "clock"
	KeyColorAccent = "colors.accent"
	KeyColorWarn   = "colors.warn"
	KeyColorDanger = "colors.danger"
	KeyColorDim    = "colors.dim"
	KeyColorBright = "colors.bright"
	Clock24h       = "24h"
	Clock12h       = "12h"
)

var keys = []Key{
	{Name: KeyClock, Section: Shared, Kind: KindEnum, Values: []string{Clock24h, Clock12h}, Default: Clock24h,
		Portable: true, Consumer: "M21", Live: true},
	{Name: KeyColorAccent, Section: TUI, Kind: KindColor, Default: "6", Portable: true, Consumer: "M21", Live: true},
	{Name: KeyColorWarn, Section: TUI, Kind: KindColor, Default: "3", Portable: true, Consumer: "M21", Live: true},
	{Name: KeyColorDanger, Section: TUI, Kind: KindColor, Default: "1", Portable: true, Consumer: "M21", Live: true},
	{Name: KeyColorDim, Section: TUI, Kind: KindColor, Default: "8", Portable: true, Consumer: "M21", Live: true},
	{Name: KeyColorBright, Section: TUI, Kind: KindColor, Default: "15", Portable: true, Consumer: "M21", Live: true},

	// Reserved: the format is settled, and nothing reads these until the milestone named.
	{Name: "theme", Section: TUI, Kind: KindString, Portable: true, Consumer: "M45"},
	{Name: "keys", Section: TUI, Kind: KindTable, Portable: true, Consumer: "M44"},
	{Name: "layout", Section: TUI, Kind: KindTable, Portable: true, Consumer: "M41"},
}

// Keys returns every key the file may hold, in the contract's order.
func Keys() []Key {
	out := make([]Key, len(keys))
	copy(out, keys)
	return out
}

// lookup finds the key that name means when written under section.
//
// A Shared key is valid under every section. A client's key is valid under its own only: [gui.colors] is
// not the terminal's colors, and accepting it would make a typo of the section silently do nothing.
// A name inside a table key ("keys.C-x b") resolves to that table key.
func lookup(section Section, name string) (Key, bool) {
	for _, k := range keys {
		if k.Section != Shared && k.Section != section {
			continue
		}
		if k.Name == name || (k.Kind == KindTable && strings.HasPrefix(name, k.Name+".")) {
			return k, true
		}
	}
	return Key{}, false
}

func validSection(s string) bool {
	return s == string(Shared) || s == string(TUI) || s == string(GUI)
}
