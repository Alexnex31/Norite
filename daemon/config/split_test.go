// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"testing"

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

// Merge is the toggle going off: the base's bytes stay, comments and all, a key only the other file sets is
// added, and where both set one the base's value stays and is reported as kept.
func TestMergeAddsWhatTheBaseLacksAndKeepsWhatItHas(t *testing.T) {
	base := "# newer\n[shared]\nclock = \"24h\" # mine\n\n[tui.colors]\naccent = 9\n"
	other := "# older\n[shared]\nclock = \"12h\"\n\n[tui.colors]\naccent = 208\ndim = 244\n\n[tui.keys]\n\"C-x b\" = \"buffers\"\n"

	merged, plan, err := Merge([]byte(base), []byte(other))
	require.NoError(t, err)
	assert.Equal(t, "# newer\n[shared]\nclock = \"24h\" # mine\n\n[tui.colors]\naccent = 9\ndim = 244\n\n[tui.keys]\n\"C-x b\" = \"buffers\"\n",
		string(merged))
	var applied, kept []string
	for _, c := range plan.Apply {
		applied = append(applied, c.Key())
	}
	for _, c := range plan.Kept {
		kept = append(kept, c.Key())
	}
	assert.Equal(t, []string{"tui.colors.dim", "tui.keys.C-x b"}, applied)
	assert.Equal(t, []string{"shared.clock", "tui.colors.accent"}, kept)

	// Merged again with the same file, there is nothing left to add: it is a fixed point.
	again, plan, err := Merge(merged, []byte(other))
	require.NoError(t, err)
	assert.Equal(t, string(merged), string(again))
	assert.Empty(t, plan.Apply)
}

// Either file failing to parse is an error naming where, and no result.
func TestMergeRefusesAFileThatDoesNotParse(t *testing.T) {
	_, _, err := Merge([]byte("[shared]\nclock = \"24h\"\n"), []byte("[tui\n"))
	var pe *ParseError
	require.ErrorAs(t, err, &pe)
	assert.NotContains(t, err.Error(), "to import", "neither file is somebody else's")
	_, _, err = Merge([]byte("[tui\n"), nil)
	require.ErrorAs(t, err, &pe)
}

// An import refuses a machine-local key, since the file is a stranger's. A merge of this machine's own
// two files must carry one over: it is exactly the kind of setting that would otherwise be lost.
func TestMergeCarriesAMachineLocalKeyThatAnImportRefuses(t *testing.T) {
	was := keys
	t.Cleanup(func() { keys = was })
	keys = append(append([]Key{}, was...), Key{Name: "editor", Section: TUI, Kind: KindString, Portable: false})

	other := []byte("[tui]\neditor = \"nvim\"\n")
	_, err := PlanImport(nil, other, false)
	require.ErrorIs(t, err, ErrMachineLocal)

	merged, _, err := Merge(nil, other)
	require.NoError(t, err)
	assert.Equal(t, "[tui]\neditor = \"nvim\"\n", string(merged))
}
