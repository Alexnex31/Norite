// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oneAtATime is what setAll was before it planned: each edit made against the document the last one left.
// It is the definition of what a batch must produce, byte for byte.
func oneAtATime(t *testing.T, doc string, edits []edit) string {
	t.Helper()
	out := []byte(doc)
	for _, e := range edits {
		var err error
		out, err = setRaw(out, e.path, e.literal)
		require.NoError(t, err, strings.Join(e.path, "."))
	}
	return string(out)
}

func set(path, literal string) edit { return edit{strings.Split(path, "/"), literal} }

// Planned against one read, a batch writes exactly the file that making the same edits one at a time
// writes: the same lines in the same places, whichever way each key has to be added.
func TestABatchOfEditsWritesWhatOneAtATimeWrites(t *testing.T) {
	everyWay := []edit{
		set("tui/colors/accent", `"#ff0000"`), // replaces a value under a header
		set("tui/colors/dim", "244"),          // joins a header's table
		set("tui/colors/warn", "3"),           // and a second, after the first
		set("shared/clock", `"12h"`),          // replaces a value
		set("tui/keys/C-x b", `"buffers"`),    // a table nothing mentions: a new header
		set("gui/colors/accent", "9"),         // another new header, asked for between the first one's keys
		set("tui/keys/C-x k", `"kill"`),       // which still goes under its own
		set("tui/theme", `"dark"`),            // a parent that exists only as a prefix of other headers
	}
	for name, doc := range map[string]string{
		"an empty file":       "",
		"headers":             "# mine\n[shared]\nclock = \"24h\" # as I like it\n\n[tui.colors]\naccent = 6\n\n[gui]\n# nothing yet\n",
		"no final newline":    "[shared]\nclock = \"24h\"\n[tui.colors]\naccent = 6",
		"windows line ends":   "[shared]\r\nclock = \"24h\"\r\n[tui.colors]\r\naccent = 6\r\n",
		"dotted under a root": "shared.clock = \"24h\"\ntui.colors.accent = 6\n",
		"dotted in a table":   "[tui]\ncolors.accent = 6\ncolors.bright = 15\n[shared]\nclock = \"24h\"\n",
		"a byte-order mark":   "\xef\xbb\xbf[shared]\nclock = \"24h\"\n",
		"a multi-line value":  "[tui]\nlayout = { a = \"\"\"x\ny\"\"\" }\n[shared]\nclock = \"24h\"\n[tui.colors]\naccent = 6\n",
	} {
		t.Run(name, func(t *testing.T) {
			want := oneAtATime(t, doc, everyWay)
			got, err := setAll([]byte(doc), everyWay)
			require.NoError(t, err)
			assert.Equal(t, want, string(got))
		})
	}
}

// Two edits to one path are the later one, as they would be one after the other.
func TestTheLaterOfTwoEditsToOnePathWins(t *testing.T) {
	edits := []edit{set("tui/colors/accent", "1"), set("tui/colors/dim", "2"), set("tui/colors/accent", "3")}
	for _, doc := range []string{"", "[tui.colors]\naccent = 6\n"} {
		got, err := setAll([]byte(doc), edits)
		require.NoError(t, err)
		assert.Equal(t, oneAtATime(t, doc, edits), string(got))
		assert.Contains(t, string(got), "accent = 3")
	}
}

// One bad edit in a batch writes none of it.
func TestABatchWithOneRefusedEditChangesNothing(t *testing.T) {
	doc := "[tui]\ncolors = { accent = 6 }\n"
	_, err := setAll([]byte(doc), []edit{set("shared/clock", `"12h"`), set("tui/colors/dim", "2")})
	require.ErrorIs(t, err, ErrInlineTable)
	_, err = setAll([]byte(doc), []edit{set("shared/clock", "\"12h\"\n[injected]\nx = 1")})
	require.Error(t, err)
}

// manyKeys is a [tui.keys] table as large as the size bound allows: about five thousand short entries.
func manyKeys(t *testing.T) []byte {
	t.Helper()
	var b strings.Builder
	b.WriteString("[tui.keys]\n")
	for i := 0; b.Len() < MaxFileSize-64; i++ {
		fmt.Fprintf(&b, "k%04d = \"v\"\n", i)
	}
	return []byte(b.String())
}

// An import is bounded by the file's size, and so must its cost be. Made one key at a time, a file inside
// the bound took two minutes to import, with the config's lock held and every other `norite config`
// refused for as long (M21 /code-review); planned, it is a fraction of a second. The limit here is far
// above that and far below what it replaced, so it fails on the cubic shape and not on a slow machine.
func TestAnImportAtTheSizeBoundIsNotCubic(t *testing.T) {
	incoming := manyKeys(t)
	path := filepath.Join(t.TempDir(), "config.toml")

	start := time.Now()
	plan, err := importAt(path, incoming, false)
	require.NoError(t, err)
	took := time.Since(start)
	assert.Greater(t, len(plan.Apply), 4000)
	assert.Less(t, took, 10*time.Second, "importing %d keys", len(plan.Apply))

	written, err := ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(incoming), string(written), "one header, and every key under it in order")

	// And the way back out: an export of that file is the same work.
	f, err := Inspect(written)
	require.NoError(t, err)
	start = time.Now()
	_, err = f.Export()
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 10*time.Second, "exporting them")
}

// A table's entry that is not a string is shown as a string, since that is the type an entry's value is
// promised to be, and is not mistaken for the string it is spelled like: `a = 5` and an incoming `a = "5"`
// differ, so the import says it kept this machine's, or replaces it when told to.
func TestATableEntryThatIsNotAStringIsNotTheStringItLooksLike(t *testing.T) {
	current := "[tui.keys]\na = 5\nb = \"x\"\n"
	e, err := mustInspect(t, current).Get(TUI, "keys.a")
	require.NoError(t, err)
	assert.Equal(t, "5", e.Value)
	assert.Equal(t, FromFile, e.Source)

	incoming := []byte("[tui.keys]\na = \"5\"\nb = \"x\"\n")
	plan, err := PlanImport([]byte(current), incoming, false)
	require.NoError(t, err)
	assert.Empty(t, plan.Apply)
	require.Len(t, plan.Kept, 1, "the integer is not the string, so it is a difference that was kept")
	assert.Equal(t, "tui.keys.a", plan.Kept[0].Key())

	plan, err = PlanImport([]byte(current), incoming, true)
	require.NoError(t, err)
	require.Len(t, plan.Apply, 1)
	assert.True(t, plan.Apply[0].Replace)
}

// TOML allows an empty key. One in a table is skipped with a reason, on the way out and on the way in,
// rather than failing everything else in the file.
func TestAnEmptyKeyInATableIsSkippedAndTheRestGoesThrough(t *testing.T) {
	doc := "[tui.keys]\n\"\" = \"x\"\n\"C-x b\" = \"buffers\"\n"

	out, err := mustInspect(t, doc).Export()
	require.NoError(t, err)
	assert.Contains(t, string(out), `"C-x b" = "buffers"`)
	assert.NotContains(t, string(out), `"" =`)

	plan, err := PlanImport(nil, []byte(doc), false)
	require.NoError(t, err)
	require.Len(t, plan.Apply, 1)
	assert.Equal(t, "tui.keys.C-x b", plan.Apply[0].Key())
	require.Len(t, plan.Skipped, 1)
	assert.Contains(t, plan.Skipped[0].Problem, "empty name")

	path := filepath.Join(t.TempDir(), "config.toml")
	_, err = importAt(path, []byte(doc), false)
	require.NoError(t, err)
}
