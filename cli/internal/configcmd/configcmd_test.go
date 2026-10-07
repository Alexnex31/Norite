// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package configcmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemontest"
	"github.com/Alexnex31/Norite/daemon/config"
)

// home points every directory the commands resolve at a throwaway one, and returns where config.toml is.
func home(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"HOME", "USERPROFILE", "LOCALAPPDATA"} {
		t.Setenv(name, dir)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "cfg"))
	t.Setenv("APPDATA", filepath.Join(dir, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	path, err := config.Path()
	require.NoError(t, err)
	return path
}

func write(t *testing.T, path, contents string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

// norite runs `norite [--json] config args...` with nothing on stdin, which is not a terminal.
func norite(t *testing.T, asJSON bool, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := &cli.Command{
		Name: "norite", Writer: &out, ErrWriter: &errOut, Reader: strings.NewReader(""),
		Flags:          []cli.Flag{&cli.BoolFlag{Name: "json"}},
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
		Commands:       []*cli.Command{Command()},
	}
	argv := []string{"norite"}
	if asJSON {
		argv = append(argv, "--json")
	}
	err = root.Run(context.Background(), append(append(argv, "config"), args...))
	return out.String(), errOut.String(), err
}

func requireUsage(t *testing.T, err error) {
	t.Helper()
	var usage *clierr.UsageError
	require.ErrorAs(t, err, &usage, "a mistake on the command line is exit 2")
}

// Every verb's --json output against its definition in contracts/cli-json/config.schema.json (rule 15).
func TestEveryVerbsJSONMatchesItsSchema(t *testing.T) {
	path := home(t)
	check := func(def string, args ...string) string {
		t.Helper()
		out, _, err := norite(t, true, args...)
		require.NoError(t, err, args)
		daemontest.MatchesCLISchema(t, out, "config.schema.json", def)
		return out
	}

	check("path", "path")
	check("entries", "get")
	check("entry", "set", "tui.colors.accent", "208")
	check("entry", "set", "tui.keys.C-x b", "buffers")
	check("entry", "get", "tui.keys")
	check("entry", "get", "tui.theme")
	check("entry", "get", "tui.clock")
	check("entry", "unset", "tui.colors.accent")
	check("path", "path")

	exported := filepath.Join(t.TempDir(), "exported.toml")
	check("exported", "export", "--output", exported)

	incoming := filepath.Join(t.TempDir(), "in.toml")
	write(t, incoming, "[shared]\nclock = \"12h\"\n[tui]\ntheme = \"a\"\nnewer = 1\n[tui.keys]\n\"C-x b\" = \"other\"\n")
	check("imported", "import", incoming, "--dry-run")
	check("imported", "import", incoming, "--yes")
	check("imported", "import", incoming, "--yes")
	check("imported", "import", incoming, "--yes", "--overwrite")

	write(t, path, "[shared]\nclock = \"13h\"\nunknown = 1\n")
	out := check("entries", "get")
	assert.Contains(t, out, "shared.unknown")
}

func TestSetChangesTheFileAndNothingElseInIt(t *testing.T) {
	path := home(t)
	write(t, path, "# mine\n[shared]   # everything\nclock = \"24h\"   # I think in 24h\n")

	out, _, err := norite(t, false, "set", "shared.clock", "12h")
	require.NoError(t, err)
	assert.Equal(t, "shared.clock = 12h  (file)\n", out)
	assert.Equal(t, "# mine\n[shared]   # everything\nclock = \"12h\"   # I think in 24h\n", read(t, path))

	out, _, err = norite(t, false, "unset", "shared.clock")
	require.NoError(t, err)
	assert.Equal(t, "shared.clock = 24h  (default)\n", out, "what is printed is what is now in force")
}

func TestGetListsEveryKeyAndSaysWhatNothingReadsYet(t *testing.T) {
	home(t)
	out, _, err := norite(t, false, "get")
	require.NoError(t, err)
	for _, want := range []string{
		"shared.clock = 24h  (default)", "tui.colors.accent = 6  (default)",
		"tui.keys is not set  (nothing reads this until M44)",
	} {
		assert.Contains(t, out, want)
	}
	assert.Len(t, strings.Split(strings.TrimSpace(out), "\n"), len(config.Keys()))
}

// A mistake on the command line is exit 2 and writes nothing; a file that is not TOML is not that.
func TestMistakesAreUsageErrorsAndABrokenFileIsNot(t *testing.T) {
	path := home(t)
	for _, args := range [][]string{
		{"set", "tui.colors.acent", "3"},
		{"set", "tui.colors.accent", "red"},
		{"set", "gui.colors.accent", "3"},
		{"set", "cli.clock", "12h"},
		{"set", "clock", "12h"},
		{"set", "tui.keys", "x"},
		{"set", "shared.clock"},
		{"set", "shared.clock", "12h", "extra"},
		{"get", "a", "b"},
		{"get", ".clock"},
		{"unset"},
		{"unset", "tui.nope"},
		{"import"},
		{"import", filepath.Join(t.TempDir(), "absent.toml"), "--yes"},
		{"path", "extra"},
	} {
		_, _, err := norite(t, false, args...)
		require.Error(t, err, args)
		requireUsage(t, err)
	}
	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "none of them created the file")

	write(t, path, "[shared]\nclock = \"24h\"\n[tui.colors\n")
	for _, args := range [][]string{{"get"}, {"set", "shared.clock", "12h"}, {"unset", "shared.clock"}, {"export"}} {
		_, _, err := norite(t, false, args...)
		require.Error(t, err, args)
		var usage *clierr.UsageError
		assert.NotErrorAs(t, err, &usage, "%v: the command line was right; the file needs attention", args)
		assert.Contains(t, err.Error(), "line 3", args)
		assert.Contains(t, err.Error(), path, args)
	}
}

// The file is hand-edited and often somebody else's. What it holds is printed verbatim, so it is cleaned
// on the way to a terminal (rule 19), and escaped in --json.
func TestWhatTheFileHoldsIsInertWhenPrinted(t *testing.T) {
	path := home(t)
	write(t, path, "[tui]\ntheme = \"a\\u001b[2Jb\\u202ec\"\n\"evil\\u001b]0;x\\u0007\" = 1\n"+
		"[tui.keys]\n\"k\\u001b[1m\" = \"v\\u001b[0m\"\n")

	for _, args := range [][]string{{"get"}, {"get", "tui.theme"}, {"get", "tui.keys"}, {"export"}} {
		for _, asJSON := range []bool{false, true} {
			out, errOut, err := norite(t, asJSON, args...)
			require.NoError(t, err, args)
			for _, raw := range []string{"\x1b", "\u202e", "\a"} {
				assert.NotContains(t, out+errOut, raw, "%v json=%v", args, asJSON)
			}
		}
	}
}

func TestExportPrintsADocumentThatImportsElsewhere(t *testing.T) {
	path := home(t)
	write(t, path, "# mine\nfuture = 1\n[shared]\nclock = \"12h\"\n[tui.colors]\naccent = 208\n")
	doc, _, err := norite(t, false, "export")
	require.NoError(t, err)
	assert.NotContains(t, doc, "future")
	assert.NotContains(t, doc, "# mine", "an export is a new document")

	asJSON, _, err := norite(t, true, "export")
	require.NoError(t, err)
	assert.Equal(t, doc, asJSON, "the document is the output whatever --json says")

	// Another machine, with a config of its own.
	exported := filepath.Join(t.TempDir(), "exported.toml")
	_, _, err = norite(t, false, "export", "--output", exported)
	require.NoError(t, err)
	info, err := os.Stat(exported)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	_, _, err = norite(t, false, "export", "--output", exported)
	requireUsage(t, err)
	_, _, err = norite(t, false, "export", "--output", exported, "--force")
	require.NoError(t, err)

	other := home(t)
	write(t, other, "# theirs\n[shared]\nclock = \"24h\" # keep\n\n[tui.colors]\nwarn = 4\n")
	_, _, err = norite(t, false, "import", exported, "--yes")
	require.NoError(t, err)
	assert.Equal(t, "# theirs\n[shared]\nclock = \"24h\" # keep\n\n[tui.colors]\nwarn = 4\naccent = 208\n", read(t, other),
		"the portable keys arrive, and the target's own settings and comments are as they were")
}

func TestImportAsksFirstAndADryRunWritesNothing(t *testing.T) {
	path := home(t)
	before := "[shared]\nclock = \"24h\"\n"
	write(t, path, before)
	incoming := filepath.Join(t.TempDir(), "in.toml")
	write(t, incoming, "[shared]\nclock = \"12h\"\n[tui]\ntheme = \"a\"\n")

	out, _, err := norite(t, false, "import", incoming, "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, out, "add      tui.theme = a")
	assert.Contains(t, out, "keep     shared.clock = 24h")
	assert.Contains(t, out, "nothing was changed")
	assert.Equal(t, before, read(t, path))

	// No terminal and no --yes: there is nobody to ask, so nothing is done.
	_, _, err = norite(t, false, "import", incoming)
	require.ErrorIs(t, err, clierr.ErrNoTerminal)
	assert.Contains(t, err.Error(), "--yes")
	assert.Equal(t, before, read(t, path))

	out, _, err = norite(t, false, "import", incoming, "--yes")
	require.NoError(t, err)
	assert.Contains(t, out, "imported 1 setting(s)")
	assert.Equal(t, "[shared]\nclock = \"24h\"\n\n[tui]\ntheme = \"a\"\n", read(t, path))

	out, _, err = norite(t, false, "import", incoming, "--yes")
	require.NoError(t, err)
	assert.Contains(t, out, "nothing to import")

	out, _, err = norite(t, false, "import", incoming, "--yes", "--overwrite")
	require.NoError(t, err)
	assert.Contains(t, out, "replace  shared.clock = 12h  (was 24h)")
}

// The plan shown and the import are two reads of the file, and somebody else can write between them. What
// is reported is what happened: when the other writer already made every change, nothing was written, and
// the command says so rather than "imported 0 settings" under written: true.
func TestAnImportThatFindsNothingLeftToChangeSaysItWroteNothing(t *testing.T) {
	path := home(t)
	write(t, path, "[shared]\nclock = \"24h\"\n")
	incoming := filepath.Join(t.TempDir(), "in.toml")
	write(t, incoming, "[tui]\ntheme = \"a\"\n")

	raced := false
	beforeImport = func() {
		raced = true
		data, err := os.ReadFile(incoming)
		require.NoError(t, err)
		_, err = config.Import(path, data, false)
		require.NoError(t, err)
	}
	t.Cleanup(func() { beforeImport = func() {} })

	out, _, err := norite(t, true, "import", incoming, "--yes")
	require.NoError(t, err)
	require.True(t, raced)
	daemontest.MatchesCLISchema(t, out, "config.schema.json", "imported")
	assert.JSONEq(t, `{"written":false,"applied":[],"kept":[],"skipped":[],"more_skipped":0}`, out)

	// And the ordinary case still says it wrote.
	beforeImport = func() {}
	write(t, path, "[shared]\nclock = \"24h\"\n")
	out, _, err = norite(t, true, "import", incoming, "--yes")
	require.NoError(t, err)
	assert.Contains(t, out, `"written": true`)
}

func TestAnImportThatIsNotAConfigIsRefused(t *testing.T) {
	path := home(t)
	dir := t.TempDir()

	big := filepath.Join(dir, "big.toml")
	write(t, big, "# "+strings.Repeat("x", config.MaxFileSize))
	_, _, err := norite(t, false, "import", big, "--yes")
	requireUsage(t, err)

	broken := filepath.Join(dir, "broken.toml")
	write(t, broken, "[shared\n")
	_, _, err = norite(t, false, "import", broken, "--yes")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the file to import")

	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr))
}
