// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/internal/paths"
	"github.com/Alexnex31/Norite/daemon/statefile"
)

// toggled writes the state file as the daemon would, in a state directory of the test's own.
func toggled(t *testing.T, body string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("LOCALAPPDATA", os.Getenv("XDG_STATE_HOME"))
	t.Setenv("HOME", os.Getenv("XDG_STATE_HOME"))
	dir, err := paths.StateDir()
	require.NoError(t, err)
	if body != "" {
		require.NoError(t, os.WriteFile(statefile.PathIn(dir), []byte(body), 0o600))
	}
}

// Which file a client reads is the toggle's to say: one config.toml for both while it is off, which is
// also what no state file means, and each client's own while it is on.
func TestPathForFollowsTheToggle(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("APPDATA", cfg)
	dir, err := Dir()
	require.NoError(t, err)
	files := FilesIn(dir)

	for name, tc := range map[string]struct {
		state    string
		tui, gui string
		split    bool
	}{
		"no state file": {"", files.Shared, files.Shared, false},
		"off":           {`{"version":1,"config_split":false}`, files.Shared, files.Shared, false},
		"on":            {`{"version":1,"config_split":true}`, files.TUI, files.GUI, true},
	} {
		t.Run(name, func(t *testing.T) {
			toggled(t, tc.state)
			path, split, err := PathFor(TUI)
			require.NoError(t, err)
			assert.Equal(t, tc.tui, path)
			assert.Equal(t, tc.split, split)
			path, _, err = PathFor(GUI)
			require.NoError(t, err)
			assert.Equal(t, tc.gui, path)
		})
	}
	assert.Equal(t, "config.tui.toml", filepath.Base(files.TUI))
	assert.Equal(t, "config.gui.toml", filepath.Base(files.GUI))
}

// A state file this build cannot read is an error and not a guess: reading config.toml when the toggle may
// be on would show somebody settings that are not theirs and call it working.
func TestPathForDoesNotGuessPastAStateFileItCannotRead(t *testing.T) {
	toggled(t, `{"version":99,"config_split":true}`)
	_, _, err := PathFor(TUI)
	require.ErrorIs(t, err, statefile.ErrNewer)
}

// Only the two clients have a file of their own.
func TestOnlyAClientHasASplitFile(t *testing.T) {
	_, err := SplitPath(Shared)
	require.Error(t, err)
	_, err = SplitPath(Section("cli"))
	require.Error(t, err)
}

const (
	// How things stood at the split, which both files start as a copy of.
	atSplit = "[shared]\nclock = \"24h\"\n\n[tui]\ntheme = \"old\"\n\n[tui.colors]\naccent = 6\nwarn = 3\n"
)

// The review's case, which the first merge got wrong. While split the terminal client removes one of its
// settings, changes another and adds a third; the GUI's file still holds [tui] as it stood at the split,
// which nobody has read since. Unsplit takes [tui] from the terminal's file whichever file is newer: the
// removed setting stays removed, the changed one stays changed, and the stale copy decides nothing.
func TestEachClientsSectionIsItsOwnFilesWhicheverIsNewer(t *testing.T) {
	tui := "[shared]\nclock = \"24h\"\n\n[tui.colors]\naccent = 5\nwarn = 3\ndim = 244\n"
	gui := "# the window's\n" + atSplit

	want := map[Section]string{
		// The terminal's own bytes: nothing of the GUI's stale [tui] is added to them.
		TUI: tui,
		// The GUI's bytes, with its copy of [tui] brought up to what the terminal's file says.
		GUI: "# the window's\n[shared]\nclock = \"24h\"\n\n[tui]\n\n[tui.colors]\naccent = 5\nwarn = 3\ndim = 244\n",
	}
	for _, newer := range []Section{TUI, GUI} {
		merged, plan, err := MergeClients([]byte(tui), []byte(gui), newer)
		require.NoError(t, err, newer)
		assert.Equal(t, want[newer], string(merged), "with the %s file the newer", newer)
		assert.Empty(t, plan.Kept, "nothing in [shared] differs")

		f, err := Inspect(merged)
		require.NoError(t, err)
		assert.False(t, f.sets(TUI, "theme"), "a setting its owner removed does not come back")
		e, err := f.Get(TUI, "colors.accent")
		require.NoError(t, err)
		assert.Equal(t, "5", e.Value, "a setting its owner changed stays changed")
	}
}

// [shared] is the section both clients read, so it is where "last write wins" is asked, per key: a key
// only one file sets is kept, and where both set one the newer file's value stays and is reported.
func TestSharedIsMergedByKeyAndTheNewerFileWinsAConflict(t *testing.T) {
	was := keys
	t.Cleanup(func() { keys = was })
	keys = append(append([]Key{}, was...),
		Key{Name: "locale", Section: Shared, Kind: KindString, Portable: true},
		Key{Name: "editor", Section: Shared, Kind: KindString, Portable: false})

	tui := "[shared]\nclock = \"12h\"\nlocale = \"fr\"\n"
	gui := "[shared]\nclock = \"24h\"\neditor = \"nvim\"\n"

	merged, plan, err := MergeClients([]byte(tui), []byte(gui), GUI)
	require.NoError(t, err)
	assert.Equal(t, "[shared]\nclock = \"24h\"\neditor = \"nvim\"\nlocale = \"fr\"\n", string(merged))
	require.Len(t, plan.Kept, 1)
	assert.Equal(t, "shared.clock", plan.Kept[0].Key())

	// The other way round, the machine-local key is the one carried over: an import refuses such a key,
	// since its file is a stranger's, and a merge of this machine's own two files must not lose it.
	merged, _, err = MergeClients([]byte(tui), []byte(gui), TUI)
	require.NoError(t, err)
	assert.Equal(t, "[shared]\nclock = \"12h\"\nlocale = \"fr\"\neditor = \"nvim\"\n", string(merged))
	_, err = PlanImport(nil, []byte(gui), false)
	require.ErrorIs(t, err, ErrMachineLocal)
}

// Merged again with the same two files there is nothing left to do, and two untouched copies merge to
// themselves byte for byte.
func TestMergingClientsIsAFixedPoint(t *testing.T) {
	merged, plan, err := MergeClients([]byte(atSplit), []byte(atSplit), TUI)
	require.NoError(t, err)
	assert.Equal(t, atSplit, string(merged))
	assert.Empty(t, plan.Apply)

	tui := "[tui.colors]\naccent = 5\n[tui.keys]\n\"C-x b\" = \"buffers\"\n"
	once, _, err := MergeClients([]byte(tui), []byte(atSplit), GUI)
	require.NoError(t, err)
	twice, plan, err := MergeClients([]byte(tui), once, GUI)
	require.NoError(t, err)
	assert.Equal(t, string(once), string(twice))
	assert.Empty(t, plan.Apply)
}

// Either file failing to parse is an error naming where, and no result.
func TestMergingClientsRefusesAFileThatDoesNotParse(t *testing.T) {
	_, _, err := MergeClients([]byte("[shared]\nclock = \"24h\"\n"), []byte("[tui\n"), TUI)
	var pe *ParseError
	require.ErrorAs(t, err, &pe)
	assert.NotContains(t, err.Error(), "to import", "neither file is somebody else's")
	_, _, err = MergeClients([]byte("[tui\n"), nil, TUI)
	require.ErrorAs(t, err, &pe)
}

// What the stale copy of a section holds that this version does not understand is not reported: nobody
// was going to carry it. What the owner's own file holds that it does not understand is.
func TestOnlyTheOwnersUnknownKeysAreReportedSkipped(t *testing.T) {
	tui := "[tui]\nfrom_a_newer_norite = 1\n"
	gui := "[tui]\nstale_and_unknown = 2\n"
	_, plan, err := MergeClients([]byte(tui), []byte(gui), GUI)
	require.NoError(t, err)
	require.Len(t, plan.Skipped, 1)
	assert.Equal(t, "tui.from_a_newer_norite", plan.Skipped[0].Key)

	_, plan, err = MergeClients([]byte(tui), []byte(gui), TUI)
	require.NoError(t, err)
	assert.Empty(t, plan.Skipped)
}

// Removing many keys reads the document once, and removes exactly what removing them one at a time
// removes.
func TestUnsettingManyKeysIsWhatOneAtATimeDoes(t *testing.T) {
	doc := "# mine\n[shared]\nclock = \"24h\" # as I like it\n\n[tui.colors]\naccent = 6\nwarn = 3\ndim = 8\n\n[tui]\ntheme = \"x\"\n"
	paths := [][]string{{"tui", "colors", "accent"}, {"tui", "theme"}, {"tui", "colors", "missing"}, {"tui", "colors", "dim"}}
	want := []byte(doc)
	for _, p := range paths {
		var err error
		want, err = unsetRaw(want, p)
		require.NoError(t, err)
	}
	got, err := unsetAll([]byte(doc), paths)
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got))
	assert.Equal(t, "# mine\n[shared]\nclock = \"24h\" # as I like it\n\n[tui.colors]\nwarn = 3\n\n[tui]\n", string(got))
}

// While split, an import into one client's file leaves the other client's section out and says so, as
// `set` refuses it: nothing reads [tui] from the GUI's file. Into config.toml, which both read, it is all
// taken.
func TestAnImportIntoOneClientsFileLeavesTheOthersSectionOut(t *testing.T) {
	incoming := []byte("[shared]\nclock = \"12h\"\n[tui.colors]\naccent = 5\n[tui]\ntheme = \"x\"\n")

	plan, err := PlanImportInto(nil, incoming, false, GUI)
	require.NoError(t, err)
	require.Len(t, plan.Apply, 1)
	assert.Equal(t, "shared.clock", plan.Apply[0].Key())
	require.Len(t, plan.Skipped, 2)
	assert.Equal(t, "tui.colors.accent", plan.Skipped[0].Key)
	assert.Contains(t, plan.Skipped[0].Problem, "--client tui")

	for _, whose := range []Section{"", TUI} {
		plan, err = PlanImportInto(nil, incoming, false, whose)
		require.NoError(t, err)
		assert.Len(t, plan.Apply, 3, "into %q", whose)
		assert.Empty(t, plan.Skipped)
	}
}

// What an import skips is listed in the keys' order, the same every run: a Go map was ranged over, so the
// list changed from one run to the next, and with more than fifty so did which fifty were named.
func TestWhatIsSkippedIsListedInTheSameOrderEveryRun(t *testing.T) {
	var b strings.Builder
	b.WriteString("[tui.keys]\n")
	for i := range 80 {
		fmt.Fprintf(&b, "k%02d = %d\n", i, i)
	}
	first, err := PlanImport(nil, []byte(b.String()), false)
	require.NoError(t, err)
	require.Len(t, first.Skipped, MaxWarnings)
	assert.Equal(t, 30, first.MoreSkipped)
	assert.Equal(t, "tui.keys.k00", first.Skipped[0].Key)
	assert.Equal(t, "tui.keys.k49", first.Skipped[MaxWarnings-1].Key)
	for range 20 {
		again, err := PlanImport(nil, []byte(b.String()), false)
		require.NoError(t, err)
		require.Equal(t, first.Skipped, again.Skipped)
	}
}

// A write that was waiting on a file's lock while the toggle was flipped asks, once it has the lock,
// which file is its client's now, and goes there. Otherwise it would land in a file just set aside and
// report success.
func TestAWriteThatWaitedThroughAToggleGoesToTheFileNowRead(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("APPDATA", cfg)
	toggled(t, `{"version":1,"config_split":true}`)
	dir, err := Dir()
	require.NoError(t, err)
	files := FilesIn(dir)
	stateDir, err := paths.StateDir()
	require.NoError(t, err)

	// The daemon, mid-unsplit: it holds the terminal client's file.
	unlock, err := Lock(files.TUI)
	require.NoError(t, err)
	type result struct {
		path string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		path, err := SetFor(TUI, TUI, "colors.accent", "5")
		done <- result{path, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("the write did not wait for the lock: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}
	require.NoError(t, os.WriteFile(statefile.PathIn(stateDir), []byte(`{"version":1,"config_split":false}`), 0o600))
	unlock()

	r := <-done
	require.NoError(t, r.err)
	assert.Equal(t, files.Shared, r.path)
	data, err := os.ReadFile(files.Shared)
	require.NoError(t, err)
	assert.Equal(t, "[tui.colors]\naccent = 5\n", string(data))
	_, err = os.Stat(files.TUI)
	assert.ErrorIs(t, err, os.ErrNotExist, "nothing was written to the file that was set aside")
}
