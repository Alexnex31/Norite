// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A file the way somebody who cares about it writes one: a banner, aligned values, a comment after a
// value, a comment above a key, odd spacing, a blank line inside a table, a commented-out setting.
const dense = `# ~/.config/norite/config.toml
#   mine. hands off the formatting.

[shared]   # everything shares this
clock    =   "24h"     # I think in 24h

[tui.colors]
# the accent is the cursor row
accent = 6
warn="#ffaa00"# no space, on purpose

danger =	1
# dim = 8   <- trying without

[tui]
theme = """
norite-dark"""   # not read yet

[tui.keys]
"C-x b" = "buffers"
`

func mustSet(t *testing.T, data string, path []string, literal string) string {
	t.Helper()
	out, err := setRaw([]byte(data), path, literal)
	require.NoError(t, err)
	return string(out)
}

// The property the milestone is named for: outside the value that changed, every byte is the one that was
// there. Asserted on the bytes, not on "the comments are still present".
func TestSetChangesTheValueAndNoOtherByte(t *testing.T) {
	cases := []struct {
		name     string
		path     []string
		literal  string
		old, new string
	}{
		{"a value with a trailing comment", []string{"shared", "clock"}, `"12h"`, `"24h"     # I think`, `"12h"     # I think`},
		{"a number", []string{"tui", "colors", "accent"}, `208`, "accent = 6\n", "accent = 208\n"},
		{"no spaces round the equals", []string{"tui", "colors", "warn"}, `3`, `warn="#ffaa00"# no`, `warn=3# no`},
		{"a tab before the value", []string{"tui", "colors", "danger"}, `"#ff0000"`, "danger =\t1\n", "danger =\t\"#ff0000\"\n"},
		{"a multi-line string", []string{"tui", "theme"}, `"plain"`, "theme = \"\"\"\nnorite-dark\"\"\"   # not", "theme = \"plain\"   # not"},
		{"a quoted key", []string{"tui", "keys", "C-x b"}, `"switcher"`, `"C-x b" = "buffers"`, `"C-x b" = "switcher"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, 1, strings.Count(dense, tc.old), "the fixture must hold the old text exactly once")
			want := strings.Replace(dense, tc.old, tc.new, 1)
			assert.Equal(t, want, mustSet(t, dense, tc.path, tc.literal))
		})
	}
}

func TestSettingTheValueItAlreadyHasChangesNothing(t *testing.T) {
	assert.Equal(t, dense, mustSet(t, dense, []string{"tui", "colors", "accent"}, "6"))
}

// A new key goes after its table's last key: above the commented-out line and the blank line that follow,
// since those sit between this table and the next and nothing says whose they are.
func TestANewKeyJoinsItsTable(t *testing.T) {
	out := mustSet(t, dense, []string{"tui", "colors", "bright"}, "15")
	want := strings.Replace(dense, "danger =\t1\n", "danger =\t1\nbright = 15\n", 1)
	assert.Equal(t, want, out)

	out = mustSet(t, dense, []string{"shared", "newer"}, `"x"`)
	want = strings.Replace(dense, "# I think in 24h\n", "# I think in 24h\nnewer = \"x\"\n", 1)
	assert.Equal(t, want, out)
}

func TestANewKeyInATableWithOnlyAHeader(t *testing.T) {
	in := "[shared] # mine\n\n[tui]\ntheme = \"a\"\n"
	assert.Equal(t, "[shared] # mine\nclock = \"12h\"\n\n[tui]\ntheme = \"a\"\n",
		mustSet(t, in, []string{"shared", "clock"}, `"12h"`))
}

// [tui] with `colors.warn = 3` has already defined tui.colors. A [tui.colors] header would define it
// again, which is not TOML, so the new key is written the way its sibling is.
func TestANewKeyBesideDottedSiblingsIsWrittenDotted(t *testing.T) {
	in := "[tui]\ncolors.warn = 3 # mine\ntheme = \"a\"\n\n[shared]\nclock = \"12h\"\n"
	out := mustSet(t, in, []string{"tui", "colors", "accent"}, "9")
	assert.Equal(t, "[tui]\ncolors.warn = 3 # mine\ntheme = \"a\"\ncolors.accent = 9\n\n[shared]\nclock = \"12h\"\n", out)

	c, err := Parse([]byte(out), TUI)
	require.NoError(t, err)
	assert.Equal(t, Color("9"), c.Color(KeyColorAccent))
	assert.Equal(t, Color("3"), c.Color(KeyColorWarn))
}

func TestANewTableGoesAtTheEnd(t *testing.T) {
	cases := map[string][2]string{
		"an empty file":          {"", "[shared]\nclock = \"12h\"\n"},
		"comments only":          {"# mine\n", "# mine\n\n[shared]\nclock = \"12h\"\n"},
		"no final newline":       {"[tui]\ntheme = \"a\"", "[tui]\ntheme = \"a\"\n\n[shared]\nclock = \"12h\"\n"},
		"windows line endings":   {"[tui]\r\ntheme = \"a\"\r\n", "[tui]\r\ntheme = \"a\"\r\n\r\n[shared]\r\nclock = \"12h\"\r\n"},
		"a key on the last line": {"[tui.colors]\naccent = 1", "[tui.colors]\naccent = 1\n\n[shared]\nclock = \"12h\"\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc[1], mustSet(t, tc[0], []string{"shared", "clock"}, `"12h"`))
		})
	}
}

func TestAKeyAddedToTheLastTableOfAFileWithNoFinalNewline(t *testing.T) {
	assert.Equal(t, "[tui.colors]\naccent = 1\nwarn = 2\n",
		mustSet(t, "[tui.colors]\naccent = 1", []string{"tui", "colors", "warn"}, "2"))
}

// Reaching into `colors = { accent = 6 }` would mean re-serializing the inline table, which is the thing
// this file does not do. Appending a [tui.colors] table instead would define it twice.
func TestAKeyInsideAnInlineTableIsRefusedAndNothingChanges(t *testing.T) {
	for _, in := range []string{
		"[tui]\ncolors = { accent = 6 } # compact\n",
		"tui = { colors = { accent = 6 } }\n",
		"[[tui]]\ncolors.accent = 6\n",
	} {
		_, err := setRaw([]byte(in), []string{"tui", "colors", "accent"}, "9")
		require.ErrorIs(t, err, ErrInlineTable, in)
		_, err = unsetRaw([]byte(in), []string{"tui", "colors", "accent"})
		require.ErrorIs(t, err, ErrInlineTable, in)
	}
}

func TestAnEditToAFileThatIsNotTOMLIsRefused(t *testing.T) {
	_, err := setRaw([]byte("[shared\nclock = 1\n"), []string{"shared", "clock"}, `"12h"`)
	var pe *ParseError
	require.ErrorAs(t, err, &pe)
}

// A literal that is not a value must not reach the file. The check is the ordinary decoder reading the
// result back, so it holds whatever this file's own parser thought.
func TestALiteralThatBreaksTheFileIsRefused(t *testing.T) {
	for _, literal := range []string{"", "\"unterminated", "1\n[injected]\nx = 1", "6 # then\nextra = "} {
		_, err := setRaw([]byte(dense), []string{"shared", "clock"}, literal)
		require.Error(t, err, "%q", literal)
	}
}

func TestUnsetRemovesTheLineAndItsOwnComment(t *testing.T) {
	out, err := unsetRaw([]byte(dense), []string{"tui", "colors", "warn"})
	require.NoError(t, err)
	assert.Equal(t, strings.Replace(dense, "warn=\"#ffaa00\"# no space, on purpose\n", "", 1), string(out))

	// The comment above a key stays: nothing says it was that key's.
	out, err = unsetRaw([]byte(dense), []string{"tui", "colors", "accent"})
	require.NoError(t, err)
	assert.Equal(t, strings.Replace(dense, "accent = 6\n", "", 1), string(out))
	assert.Contains(t, string(out), "# the accent is the cursor row")

	out, err = unsetRaw([]byte(dense), []string{"tui", "theme"})
	require.NoError(t, err)
	assert.Equal(t, strings.Replace(dense, "theme = \"\"\"\nnorite-dark\"\"\"   # not read yet\n", "", 1), string(out))
}

func TestUnsetOfAKeyThatIsNotSetIsNoChange(t *testing.T) {
	out, err := unsetRaw([]byte(dense), []string{"tui", "colors", "bright"})
	require.NoError(t, err)
	assert.Equal(t, dense, string(out))
}

func TestUnsetOnTheLastLineWithoutANewline(t *testing.T) {
	out, err := unsetRaw([]byte("[shared]\nclock = \"12h\""), []string{"shared", "clock"})
	require.NoError(t, err)
	assert.Equal(t, "[shared]\n", string(out))
}

// Every string this file writes must read back as the string it was given, control characters and all.
func TestAStringSurvivesBeingWritten(t *testing.T) {
	for _, s := range []string{"plain", `back\slash`, `"quoted"`, "tab\there", "line\nbreak", "esc\x1b[2J", "\u202eover", "é漢🎉", "\x7f"} {
		var got struct{ V string }
		require.NoError(t, toml.Unmarshal([]byte("V = "+formatString(s)+"\n"), &got), "%q", s)
		assert.Equal(t, s, got.V)
		assert.NotContains(t, formatString(s), "\x1b", "a control character is written as an escape, never raw")
	}
}

func TestAKeyIsQuotedOnlyWhenItMustBe(t *testing.T) {
	assert.Equal(t, "accent", formatKey("accent"))
	assert.Equal(t, `"C-x b"`, formatKey("C-x b"))
	assert.Equal(t, `"a.b"`, formatKey("a.b"))
	assert.Equal(t, `""`, formatKey(""))
}

func TestLiteralSpellsWhatWasTypedAsTheFileHoldsIt(t *testing.T) {
	clock, _ := lookup(Shared, KeyClock)
	accent, _ := lookup(TUI, KeyColorAccent)
	theme, _ := lookup(TUI, "theme")
	keysTable, _ := lookup(TUI, "keys")

	for _, tc := range []struct {
		key   Key
		input string
		want  string
	}{
		{clock, "12h", `"12h"`},
		{accent, "208", "208"},
		{accent, "#1E90FF", `"#1e90ff"`},
		{theme, `say "hi"`, `"say \"hi\""`},
	} {
		got, err := Literal(tc.key, tc.input)
		require.NoError(t, err, tc.input)
		assert.Equal(t, tc.want, got)
	}
	for _, tc := range []struct {
		key   Key
		input string
	}{{clock, "13h"}, {accent, "256"}, {accent, "red"}, {accent, ""}, {keysTable, "x"}} {
		_, err := Literal(tc.key, tc.input)
		require.Error(t, err, tc.input)
	}
}
