// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package configcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/cli/internal/daemontest"
	"github.com/Alexnex31/Norite/daemon/config"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/statefile"
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

// testDaemon is how the commands under test reach a daemon. Nil is none, which is every verb but two.
var testDaemon Connector

// norite runs `norite [--json] config args...` with nothing on stdin, which is not a terminal.
func norite(t *testing.T, asJSON bool, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := &cli.Command{
		Name: "norite", Writer: &out, ErrWriter: &errOut, Reader: strings.NewReader(""),
		Flags:          []cli.Flag{&cli.BoolFlag{Name: "json"}},
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
		Commands:       []*cli.Command{Command(testDaemon)},
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
		_, _, err = config.ImportFor(config.TUI, data, false)
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

// fakeDaemon answers the two toggle requests as scripted, and records what it was asked.
type fakeDaemon struct {
	res   ipc.Result
	err   error
	asked []string
	// dir is where this daemon says it keeps configs; empty is where the test's own shell does.
	dir string
}

func (f *fakeDaemon) Do(_ context.Context, method, path string, _ any) (ipc.Result, error) {
	if method == "GET" && path == ipc.PathConfig {
		dir := f.dir
		if dir == "" {
			dir, _ = config.Dir()
		}
		body, _ := json.Marshal(ipc.ConfigLocation{Dir: dir})
		return ipc.Result{Status: 200, Body: body}, nil
	}
	f.asked = append(f.asked, method+" "+path)
	return f.res, f.err
}

func withDaemon(t *testing.T, f *fakeDaemon) {
	t.Helper()
	testDaemon = func(context.Context) (daemonclient.Caller, func(), error) { return f, func() {}, nil }
	t.Cleanup(func() { testDaemon = nil })
}

// split writes the state file as the daemon would after a split.
func split(t *testing.T) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "norite")
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		t.Skip("the state directory is laid out differently here; the toggle's lookup is tested in daemon/config")
	}
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(statefile.PathIn(dir), []byte(`{"version":1,"config_split":true}`), 0o600))
}

// The two verbs ask the daemon, by POST at its own paths, and print what it says it did: as text, and as
// JSON matching the contract.
func TestSplitAndUnsplitAskTheDaemonAndReportWhatItDid(t *testing.T) {
	home(t)
	d := &fakeDaemon{res: ipc.Result{Status: 200, Body: []byte(
		`{"split":true,"files":["/c/config.tui.toml","/c/config.gui.toml"],"base":"","merged":[],"kept":[],"skipped":[],"backups":[]}`)}}
	withDaemon(t, d)

	out, _, err := norite(t, false, "split")
	require.NoError(t, err)
	assert.Contains(t, out, "split: the terminal client and the GUI now read a config file each")
	assert.Contains(t, out, "/c/config.tui.toml")
	out, _, err = norite(t, true, "split")
	require.NoError(t, err)
	daemontest.MatchesCLISchema(t, out, "config.schema.json", "toggled")

	d.res.Body = []byte(`{"split":false,"files":["/c/config.toml"],"base":"/c/config.gui.toml",` +
		`"merged":["tui.colors.dim"],"kept":["shared.clock"],"skipped":["tui.x: not a key this version of Norite knows"],` +
		`"backups":["/c/config.tui.toml.before-unsplit","/c/config.gui.toml.before-unsplit"]}`)
	out, _, err = norite(t, false, "unsplit")
	require.NoError(t, err)
	for _, want := range []string{
		"unsplit: both clients read one file again", "started from /c/config.gui.toml",
		"take     tui.colors.dim", "keep     shared.clock", "skip     tui.x", "nothing was deleted",
		"/c/config.tui.toml.before-unsplit",
	} {
		assert.Contains(t, out, want)
	}
	out, _, err = norite(t, true, "unsplit")
	require.NoError(t, err)
	daemontest.MatchesCLISchema(t, out, "config.schema.json", "toggled")

	assert.Equal(t, []string{
		"POST " + ipc.PathConfigSplit, "POST " + ipc.PathConfigSplit,
		"POST " + ipc.PathConfigUnsplit, "POST " + ipc.PathConfigUnsplit,
	}, d.asked)
}

// What the daemon says about a toggle is text with file names in it, and is printed inert.
func TestAToggleAnswerIsPrintedInert(t *testing.T) {
	home(t)
	withDaemon(t, &fakeDaemon{res: ipc.Result{Status: 200, Body: []byte(
		`{"split":true,"files":["/c\u001b[2J/config.tui.toml"],"base":"","merged":[],"kept":[],"skipped":[],"backups":[]}`)}})
	out, _, err := norite(t, false, "split")
	require.NoError(t, err)
	assert.NotContains(t, out, "\x1b")
}

// The daemon refusing is exit 4 in its own words; no daemon is whatever attaching said, which is exit 3.
// Neither is a usage error, and neither touches a file.
func TestAToggleTheDaemonRefusesOrCannotBeAskedFor(t *testing.T) {
	home(t)
	withDaemon(t, &fakeDaemon{err: &ipc.RelayError{Code: ipc.RelayConflict, Message: "the config is already split"}})
	_, _, err := norite(t, false, "split")
	var refused *clierr.RefusedError
	require.ErrorAs(t, err, &refused)
	assert.Contains(t, err.Error(), "already split")

	testDaemon = func(context.Context) (daemonclient.Caller, func(), error) {
		return nil, nil, clierr.Unavailable("the daemon is not running; start it with `norite daemon start`")
	}
	_, _, err = norite(t, false, "unsplit")
	var unavailable *clierr.UnavailableError
	require.ErrorAs(t, err, &unavailable)

	_, _, err = norite(t, false, "split", "now")
	requireUsage(t, err)
}

// While split, these commands mean the terminal client's own file, and --client gui means the GUI's. With
// no daemon running: the toggle is read from the state file.
func TestWhileSplitTheCommandsMeanOneClientsFile(t *testing.T) {
	shared := home(t)
	split(t)
	dir := filepath.Dir(shared)
	tui, gui := filepath.Join(dir, "config.tui.toml"), filepath.Join(dir, "config.gui.toml")
	write(t, shared, "[shared]\nclock = \"24h\"\n")
	write(t, tui, "[shared]\nclock = \"24h\"\n")
	write(t, gui, "[shared]\nclock = \"24h\"\n")

	out, _, err := norite(t, true, "path")
	require.NoError(t, err)
	daemontest.MatchesCLISchema(t, out, "config.schema.json", "path")
	assert.Contains(t, out, `config.tui.toml`)
	assert.Contains(t, out, `"split": true`)
	out, _, err = norite(t, false, "--client", "gui", "path")
	require.NoError(t, err)
	assert.Contains(t, out, "config.gui.toml")
	assert.Contains(t, out, "the GUI's own file")

	// A shared setting is one client's while split: it goes in the file that was meant, and only there.
	_, _, err = norite(t, false, "set", "shared.clock", "12h")
	require.NoError(t, err)
	_, _, err = norite(t, false, "set", "tui.colors.accent", "208")
	require.NoError(t, err)
	assert.Equal(t, "[shared]\nclock = \"12h\"\n\n[tui.colors]\naccent = 208\n", read(t, tui))
	assert.Equal(t, "[shared]\nclock = \"24h\"\n", read(t, gui))
	assert.Equal(t, "[shared]\nclock = \"24h\"\n", read(t, shared), "config.toml is read by nobody and written by nothing")

	out, _, err = norite(t, false, "--client", "gui", "get", "shared.clock")
	require.NoError(t, err)
	assert.Contains(t, out, "24h")
	out, _, err = norite(t, false, "get", "shared.clock")
	require.NoError(t, err)
	assert.Contains(t, out, "12h")

	// A terminal-client setting in the GUI's file would be read by nobody. Refused, naming the flag, for
	// every verb that takes a key; nothing is written.
	for _, args := range [][]string{
		{"--client", "gui", "set", "tui.colors.accent", "9"},
		{"--client", "gui", "get", "tui.colors.accent"},
		{"--client", "gui", "unset", "tui.colors.accent"},
	} {
		_, _, err = norite(t, false, args...)
		requireUsage(t, err)
		assert.Contains(t, err.Error(), "--client tui", "%v", args)
	}
	assert.Equal(t, "[shared]\nclock = \"24h\"\n", read(t, gui))
}

// Not split, both clients read config.toml, so every section is at home in it and --client changes
// nothing. A client that is not one is a usage error either way.
func TestNotSplitEveryCommandMeansTheOneFile(t *testing.T) {
	shared := home(t)
	_, _, err := norite(t, false, "--client", "gui", "set", "tui.colors.accent", "9")
	require.NoError(t, err)
	assert.Equal(t, "[tui.colors]\naccent = 9\n", read(t, shared))
	out, _, err := norite(t, true, "--client", "gui", "path")
	require.NoError(t, err)
	assert.Contains(t, out, `"split": false`)
	assert.Contains(t, out, "config.toml")

	_, _, err = norite(t, false, "--client", "cli", "path")
	requireUsage(t, err)
	assert.Contains(t, err.Error(), "tui or gui")
}

// A daemon whose environment names another config directory than this shell's would split a directory no
// client here reads, and every client would then find its own file missing. The command asks first, and
// when the two differ it asks for nothing: it says which is which and how to make them agree.
func TestAToggleIsNotAskedOfADaemonThatMeansAnotherDirectory(t *testing.T) {
	home(t)
	d := &fakeDaemon{dir: filepath.Join(t.TempDir(), "elsewhere", "norite")}
	withDaemon(t, d)
	for _, verb := range []string{"split", "unsplit"} {
		_, _, err := norite(t, false, verb)
		var unavailable *clierr.UnavailableError
		require.ErrorAs(t, err, &unavailable)
		assert.Contains(t, err.Error(), "elsewhere")
		assert.Contains(t, err.Error(), "norite daemon install")
	}
	assert.Empty(t, d.asked, "nothing was asked of it")

	// The same directory spelled another way is the same directory.
	mine, err := config.Dir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(mine, 0o700))
	d.dir = filepath.Join(mine, "..", filepath.Base(mine)) + string(filepath.Separator)
	if runtime.GOOS != "windows" {
		// Or reached through a link, as a dotfiles manager lays a config directory out.
		d.dir = filepath.Join(t.TempDir(), "linked")
		require.NoError(t, os.Symlink(mine, d.dir))
	}
	d.res = ipc.Result{Status: 200, Body: []byte(`{"split":true,"files":[],"base":"","merged":[],"kept":[],"skipped":[],"backups":[]}`)}
	_, _, err = norite(t, false, "split")
	require.NoError(t, err)
}

// While split, an import means one client's file, and the other client's section is not read from it.
// It is left out and said, where `set` on the same key is refused: before, it was written and counted as
// imported.
func TestAnImportWhileSplitLeavesTheOtherClientsSectionOut(t *testing.T) {
	shared := home(t)
	split(t)
	gui := filepath.Join(filepath.Dir(shared), "config.gui.toml")
	incoming := filepath.Join(t.TempDir(), "in.toml")
	write(t, incoming, "[shared]\nclock = \"12h\"\n[tui.colors]\naccent = 5\n")

	out, _, err := norite(t, false, "--client", "gui", "import", incoming, "--yes")
	require.NoError(t, err)
	assert.Contains(t, out, "add      shared.clock = 12h")
	assert.Contains(t, out, "skip     tui.colors.accent")
	assert.Contains(t, out, "--client tui")
	assert.Contains(t, out, "imported 1 setting(s)")
	assert.Equal(t, "[shared]\nclock = \"12h\"\n", read(t, gui))

	out, _, err = norite(t, true, "--client", "gui", "import", incoming, "--dry-run")
	require.NoError(t, err)
	daemontest.MatchesCLISchema(t, out, "config.schema.json", "imported")

	// And what is shown before anything is written says the same as what is then done.
	out, _, err = norite(t, false, "--client", "gui", "import", incoming, "--dry-run", "--overwrite")
	require.NoError(t, err)
	assert.Contains(t, out, "skip     tui.colors.accent")
	assert.NotContains(t, out, "add      tui.colors.accent")
}

// TOML has inf and nan and JSON does not. One such value in a table nothing reads yet must not fail every
// script that lists the config.
func TestANumberJSONCannotWriteDoesNotFailTheListing(t *testing.T) {
	path := home(t)
	write(t, path, "[tui.keys]\na = inf\nb = -inf\nc = nan\nd = \"x\"\n")
	for _, args := range [][]string{{"get"}, {"get", "tui.keys"}} {
		out, _, err := norite(t, true, args...)
		require.NoError(t, err, "%v", args)
		assert.Contains(t, out, `"a": "inf"`)
		assert.Contains(t, out, `"b": "-inf"`)
		assert.Contains(t, out, `"c": "nan"`)
	}
	out, _, err := norite(t, true, "get", "tui.keys")
	require.NoError(t, err)
	daemontest.MatchesCLISchema(t, out, "config.schema.json", "entry")
}

// While split, the listing and the export are of the file that was meant, for the sections it is read
// for. The terminal client's file still holds [gui] as it stood at the split: listed as set, or exported
// to be applied on another machine, it is a setting in force nowhere, shown beside a `get` of the same key
// that refuses to answer.
func TestWhileSplitTheListingAndTheExportLeaveTheStaleSectionOut(t *testing.T) {
	shared := home(t)
	split(t)
	dir := filepath.Dir(shared)
	write(t, filepath.Join(dir, "config.tui.toml"), "[shared]\nclock = \"24h\"\n[tui.colors]\naccent = 5\n[gui]\nclock = \"12h\"\n")
	write(t, filepath.Join(dir, "config.gui.toml"), "[shared]\nclock = \"24h\"\n[tui.colors]\naccent = 6\n[gui]\nclock = \"24h\"\n")

	out, _, err := norite(t, false, "get")
	require.NoError(t, err)
	assert.Contains(t, out, "tui.colors.accent = 5")
	assert.NotContains(t, out, "gui.clock")
	out, _, err = norite(t, false, "--client", "gui", "get")
	require.NoError(t, err)
	assert.Contains(t, out, "gui.clock = 24h")
	assert.NotContains(t, out, "tui.colors.accent")

	out, _, err = norite(t, false, "export")
	require.NoError(t, err)
	assert.Contains(t, out, "accent = 5")
	assert.NotContains(t, out, "[gui]")
	out, _, err = norite(t, false, "--client", "gui", "export")
	require.NoError(t, err)
	assert.Contains(t, out, "[gui]")
	assert.NotContains(t, out, "accent")
}

// A key that does not exist is the command line's mistake whatever state the file is in. With the file
// mid-edit, `get` read it first and reported the file, exit 1, where `set` and `unset` said exit 2.
func TestAMistypedKeyIsAUsageErrorEvenWhenTheFileDoesNotParse(t *testing.T) {
	path := home(t)
	write(t, path, "[tui.colors\naccent = \n")
	for _, key := range []string{"nope", "tui.typo", "cli.colors.accent"} {
		_, _, err := norite(t, false, "get", key)
		requireUsage(t, err)
	}
	// A key that does exist still reports the file, which is what is wrong.
	_, _, err := norite(t, false, "get", "tui.colors.accent")
	require.Error(t, err)
	var usage *clierr.UsageError
	assert.False(t, errors.As(err, &usage))
	assert.Contains(t, err.Error(), "line 1")
}
