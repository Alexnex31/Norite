// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package toggle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/config"
	"github.com/Alexnex31/Norite/daemon/internal/paths"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/statefile"
)

type fixture struct {
	t       *testing.T
	h       *Handler
	files   config.Files
	changed int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	// The directories the config package itself resolves, so that a write made the way `norite config
	// set` makes one sees the same toggle and the same files as the handler.
	for _, name := range []string{"XDG_CONFIG_HOME", "APPDATA"} {
		t.Setenv(name, filepath.Join(root, "cfg"))
	}
	for _, name := range []string{"XDG_STATE_HOME", "LOCALAPPDATA", "HOME", "USERPROFILE"} {
		t.Setenv(name, filepath.Join(root, "state"))
	}
	cfgDir, err := config.Dir()
	require.NoError(t, err)
	stateDir, err := paths.StateDir()
	require.NoError(t, err)
	f := &fixture{t: t}
	f.h = &Handler{ConfigDir: cfgDir, StateDir: stateDir, Log: zerolog.Nop(), Changed: func() { f.changed++ }}
	require.NoError(t, os.MkdirAll(f.h.ConfigDir, 0o700))
	f.files = config.FilesIn(f.h.ConfigDir)
	return f
}

func (f *fixture) write(path, text string) {
	f.t.Helper()
	require.NoError(f.t, os.WriteFile(path, []byte(text), 0o600))
}

// writeAt writes a file and dates it, since "more recently written" is the file's own time.
func (f *fixture) writeAt(path, text string, at time.Time) {
	f.t.Helper()
	f.write(path, text)
	require.NoError(f.t, os.Chtimes(path, at, at))
}

func (f *fixture) read(path string) string {
	f.t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(f.t, err)
	return string(data)
}

func (f *fixture) missing(path string) {
	f.t.Helper()
	_, err := os.Stat(path)
	require.ErrorIs(f.t, err, os.ErrNotExist, path)
}

func (f *fixture) do(method, path string) ipc.Response {
	return f.h.Do(context.Background(), ipc.Request{ID: "1", Method: method, Path: path})
}

// ok performs a request that must succeed and returns what it did.
func (f *fixture) ok(path string) ipc.ConfigToggle {
	f.t.Helper()
	resp := f.do("POST", path)
	require.Nil(f.t, resp.Error, "%v", resp.Error)
	require.NotNil(f.t, resp.Status)
	require.Equal(f.t, 200, *resp.Status)
	matchesContract(f.t, resp.Body)
	var out ipc.ConfigToggle
	require.NoError(f.t, json.Unmarshal(resp.Body, &out))
	return out
}

// matchesContract holds an answer to daemon-ipc.schema.json's ConfigToggle, which is strict in both
// directions: every field it names must be present, and no other may be.
func matchesContract(t *testing.T, body []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "daemon-ipc.schema.json"))
	require.NoError(t, err)
	// The definition alone: it refers to nothing else, and the document around it refers to the gateway's.
	var doc struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Contains(t, doc.Defs, "ConfigToggle")
	def, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc.Defs["ConfigToggle"]))
	require.NoError(t, err)
	c := jsonschema.NewCompiler()
	require.NoError(t, c.AddResource("mem://config-toggle.json", def))
	schema, err := c.Compile("mem://config-toggle.json")
	require.NoError(t, err)
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	require.NoError(t, err)
	require.NoError(t, schema.Validate(inst), "%s", body)
}

// refused performs a request that must be understood and not carried out, and returns why.
func (f *fixture) refused(path string) string {
	f.t.Helper()
	resp := f.do("POST", path)
	require.NotNil(f.t, resp.Error)
	require.Nil(f.t, resp.Status, "a status is the instance's to give")
	require.Equal(f.t, ipc.RelayConflict, resp.Error.Code, resp.Error.Message)
	return resp.Error.Message
}

func (f *fixture) isSplit() bool {
	f.t.Helper()
	s, err := statefile.ReadIn(f.h.StateDir)
	require.NoError(f.t, err)
	return s.ConfigSplit
}

const shared = "# mine\n[shared]\nclock = \"12h\" # as I like it\n\n[tui.colors]\naccent = 6\n"

// Split gives each client a copy of config.toml, comments and all, leaves config.toml where it is, records
// the toggle and tells the clients.
func TestSplitCopiesTheSharedFileToBothClients(t *testing.T) {
	f := newFixture(t)
	f.write(f.files.Shared, shared)

	out := f.ok(ipc.PathConfigSplit)
	assert.True(t, out.Split)
	assert.Equal(t, []string{f.files.TUI, f.files.GUI}, out.Files)
	assert.Equal(t, shared, f.read(f.files.TUI))
	assert.Equal(t, shared, f.read(f.files.GUI))
	assert.Equal(t, shared, f.read(f.files.Shared), "config.toml is left as it was")
	assert.True(t, f.isSplit())
	assert.Equal(t, 1, f.changed, "every attached client reads its config again")
}

// With no config.toml at all there is nothing to copy, and each client still gets a file of its own.
func TestSplitWithNoConfigMakesTwoEmptyFiles(t *testing.T) {
	f := newFixture(t)
	f.ok(ipc.PathConfigSplit)
	assert.Empty(t, f.read(f.files.TUI))
	assert.Empty(t, f.read(f.files.GUI))
	f.missing(f.files.Shared)
}

// A config kept as a link into a dotfiles repository is read through the link, and the two copies are
// plain files beside it: the repository gains nothing it did not put there.
func TestSplitReadsThroughALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links")
	}
	f := newFixture(t)
	repo := t.TempDir()
	f.write(filepath.Join(repo, "config.toml"), shared)
	require.NoError(t, os.Symlink(filepath.Join(repo, "config.toml"), f.files.Shared))

	f.ok(ipc.PathConfigSplit)
	assert.Equal(t, shared, f.read(f.files.TUI))
	entries, err := os.ReadDir(repo)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	info, err := os.Lstat(f.files.Shared)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "the link is still a link")
}

// Asked twice, the second is refused and changes nothing; so is an unsplit of what is not split.
func TestTheToggleIsRefusedWhereItAlreadyIs(t *testing.T) {
	f := newFixture(t)
	f.write(f.files.Shared, shared)
	assert.Contains(t, f.refused(ipc.PathConfigUnsplit), "not split")
	f.missing(statefile.PathIn(f.h.StateDir))

	f.ok(ipc.PathConfigSplit)
	f.write(f.files.TUI, "[tui.colors]\naccent = 1\n")
	assert.Contains(t, f.refused(ipc.PathConfigSplit), "already split")
	assert.Equal(t, "[tui.colors]\naccent = 1\n", f.read(f.files.TUI), "what the client has since changed is not copied over")
	assert.Equal(t, 1, f.changed, "a refusal announces nothing")
}

// A file already where a copy would go is somebody's, and is not overwritten. One that is already the
// copy is a split that was interrupted, and carries on.
func TestSplitDoesNotOverwriteAFileItDidNotMake(t *testing.T) {
	f := newFixture(t)
	f.write(f.files.Shared, shared)
	f.write(f.files.GUI, "[gui.colors]\naccent = 3\n")

	msg := f.refused(ipc.PathConfigSplit)
	assert.Contains(t, msg, "config.gui.toml")
	assert.Equal(t, "[gui.colors]\naccent = 3\n", f.read(f.files.GUI))
	assert.False(t, f.isSplit())

	f.write(f.files.GUI, shared)
	f.ok(ipc.PathConfigSplit)
	assert.True(t, f.isSplit())
}

// The done-when: flipping the toggle on and off preserves both files' customization. Each client changes
// its own file while split. Unsplit takes each client's section from its own file, and [shared] key by
// key with the more recently written file winning where both set one.
//
// Here the GUI's file is the newer, and it still holds [tui] as it stood at the split. That stale copy
// decides nothing: the terminal's accent, changed while split, is the one config.toml ends with.
func TestUnsplitKeepsWhatEachClientSet(t *testing.T) {
	f := newFixture(t)
	f.write(f.files.Shared, shared)
	f.ok(ipc.PathConfigSplit)

	old, recent := time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
	f.writeAt(f.files.TUI, "# the terminal's\n[shared]\nclock = \"12h\"\n\n[tui.colors]\naccent = 208 # orange\ndim = 244\n", old)
	f.writeAt(f.files.GUI, "# the window's\n[shared]\nclock = \"24h\"\n\n[tui.colors]\naccent = 6\n\n[gui]\ntheme = \"paper\"\n", recent)

	out := f.ok(ipc.PathConfigUnsplit)
	assert.False(t, out.Split)
	assert.Equal(t, []string{f.files.Shared}, out.Files)
	assert.Equal(t, f.files.GUI, out.Base, "the more recently written file")
	assert.Equal(t, []string{"tui.colors.accent", "tui.colors.dim"}, out.Merged, "the terminal's own section, from its own file")
	assert.Equal(t, []string{"shared.clock"}, out.Kept, "the one section both read: the newer file's value stays")

	assert.Equal(t, "# the window's\n[shared]\nclock = \"24h\"\n\n[tui.colors]\naccent = 208\ndim = 244\n\n[gui]\ntheme = \"paper\"\n",
		f.read(f.files.Shared), "the newer file's bytes, with [tui] as the terminal's file has it")
	assert.False(t, f.isSplit())
	assert.Equal(t, 2, f.changed)

	// Nothing is deleted: both split files and the config.toml that was replaced are set aside.
	f.missing(f.files.TUI)
	f.missing(f.files.GUI)
	assert.Contains(t, f.read(f.files.TUI+config.BackupSuffix), "accent = 208 # orange")
	assert.Contains(t, f.read(f.files.GUI+config.BackupSuffix), "# the window's")
	assert.Equal(t, shared, f.read(f.files.Shared+config.BackupSuffix))
	assert.ElementsMatch(t, []string{
		f.files.Shared + config.BackupSuffix, f.files.TUI + config.BackupSuffix, f.files.GUI + config.BackupSuffix,
	}, out.Backups)
}

// A setting the terminal client removed while split stays removed. Split copied it into the GUI's file
// too, where nothing reads it, and merging without regard for whose section a key is in brought it back.
func TestASettingRemovedWhileSplitDoesNotComeBack(t *testing.T) {
	f := newFixture(t)
	f.write(f.files.Shared, "[tui]\ntheme = \"old\"\n\n[tui.colors]\naccent = 6\n")
	f.ok(ipc.PathConfigSplit)
	_, err := config.UnsetFor(config.TUI, config.TUI, "theme")
	require.NoError(t, err)
	_, err = config.SetFor(config.TUI, config.TUI, "colors.accent", "5")
	require.NoError(t, err)

	for _, newer := range []string{f.files.TUI, f.files.GUI} {
		now := time.Now()
		require.NoError(t, os.Chtimes(newer, now, now))
		older := f.files.GUI
		if newer == f.files.GUI {
			older = f.files.TUI
		}
		require.NoError(t, os.Chtimes(older, now.Add(-time.Hour), now.Add(-time.Hour)))

		f.ok(ipc.PathConfigUnsplit)
		got := f.read(f.files.Shared)
		assert.NotContains(t, got, "theme", "with %s the newer", filepath.Base(newer))
		assert.Contains(t, got, "accent = 5", "with %s the newer", filepath.Base(newer))

		// Back to split for the other order, from the same two files.
		for _, path := range []string{f.files.TUI, f.files.GUI} {
			backups, err := filepath.Glob(path + config.BackupSuffix + "*")
			require.NoError(t, err)
			require.NoError(t, os.Rename(backups[len(backups)-1], path))
		}
		require.NoError(t, os.WriteFile(statefile.PathIn(f.h.StateDir), []byte(`{"version":1,"config_split":true}`), 0o600))
	}
}

// The other way round: the terminal's file is the newer, so it is the base and its values win.
func TestTheMoreRecentlyWrittenFileIsTheBaseWhicheverItIs(t *testing.T) {
	f := newFixture(t)
	f.ok(ipc.PathConfigSplit)
	f.writeAt(f.files.GUI, "[shared]\nclock = \"24h\"\n[tui]\ntheme = \"paper\"\n", time.Now().Add(-2*time.Hour))
	f.writeAt(f.files.TUI, "[shared]\nclock = \"12h\"\n", time.Now().Add(-time.Hour))

	out := f.ok(ipc.PathConfigUnsplit)
	assert.Equal(t, f.files.TUI, out.Base)
	assert.Equal(t, "[shared]\nclock = \"12h\"\n", f.read(f.files.Shared),
		"its [shared] value wins, and the GUI file's [tui] is a stale copy that adds nothing")
}

// Split and straight back, with nothing changed in between, is where it started: config.toml byte for
// byte, and no copy of it set aside, since nothing replaced it.
func TestSplitAndUnsplitUntouchedIsWhereItStarted(t *testing.T) {
	f := newFixture(t)
	f.write(f.files.Shared, shared)
	f.ok(ipc.PathConfigSplit)
	out := f.ok(ipc.PathConfigUnsplit)
	assert.Equal(t, shared, f.read(f.files.Shared))
	assert.Empty(t, out.Merged)
	assert.Empty(t, out.Kept)
	f.missing(f.files.Shared + config.BackupSuffix)
	assert.Len(t, out.Backups, 2)
}

// A split file that is not TOML has no keys to carry over. Unsplit says which file and where, and changes
// nothing: the toggle, both files and config.toml are as they were.
func TestUnsplitRefusesAFileThatDoesNotParse(t *testing.T) {
	f := newFixture(t)
	f.write(f.files.Shared, shared)
	f.ok(ipc.PathConfigSplit)
	f.write(f.files.GUI, "[gui\ntheme = \n")

	msg := f.refused(ipc.PathConfigUnsplit)
	assert.Contains(t, msg, "config.gui.toml")
	assert.Contains(t, msg, "line 1")
	assert.True(t, f.isSplit())
	assert.Equal(t, shared, f.read(f.files.TUI))
	assert.Equal(t, "[gui\ntheme = \n", f.read(f.files.GUI))
	assert.Equal(t, shared, f.read(f.files.Shared))
}

// A client's own section is carried as its file wrote it, a key from a newer Norite included. What the
// older file's [shared] holds that this version does not understand cannot be merged by key: it is said,
// and it is still in the copy set aside.
func TestWhatUnsplitCannotCarryOverIsSaid(t *testing.T) {
	f := newFixture(t)
	f.ok(ipc.PathConfigSplit)
	f.writeAt(f.files.TUI, "[shared]\nfrom_a_newer_norite = 1\n\n[tui]\nalso_newer = 2\n\n[tui.layout]\nwidth = 100\n", time.Now().Add(-2*time.Hour))
	f.writeAt(f.files.GUI, "[shared]\nclock = \"24h\"\n\n[tui.layout]\nwidth = 80\n", time.Now().Add(-time.Hour))

	out := f.ok(ipc.PathConfigUnsplit)
	require.Len(t, out.Skipped, 1)
	assert.Contains(t, out.Skipped[0], "shared.from_a_newer_norite")
	assert.Contains(t, f.read(f.files.TUI+config.BackupSuffix), "from_a_newer_norite")
	got := f.read(f.files.Shared)
	assert.Contains(t, got, "also_newer = 2")
	assert.Contains(t, got, "width = 100", "a number in the terminal's own table is the terminal's file's")
	assert.NotContains(t, got, "width = 80")
}

// A name in a refusal is the filesystem's text, and the answer is printed by a client.
func TestARefusalNamesFilesInertly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a control character in a directory name")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "cfg\x1b[2J")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	h := &Handler{ConfigDir: dir, StateDir: root, Log: zerolog.Nop()}
	resp := h.Do(context.Background(), ipc.Request{ID: "1", Method: "POST", Path: ipc.PathConfigUnsplit})
	require.NotNil(t, resp.Error)
	assert.NotContains(t, resp.Error.Message, "\x1b")
}

// Only these two paths, and only by POST: a GET must not change anything (rule 4's hygiene, on a surface
// that reuses the relay's frames).
func TestOnlyTheTwoRequestsAndOnlyByPost(t *testing.T) {
	f := newFixture(t)
	for _, req := range [][2]string{
		{"GET", ipc.PathConfigSplit}, {"DELETE", ipc.PathConfigUnsplit},
		{"POST", ipc.LocalPathPrefix + "config"}, {"POST", ipc.LocalPathPrefix + "config/split/now"},
	} {
		resp := f.do(req[0], req[1])
		require.NotNil(t, resp.Error, "%v", req)
		assert.Equal(t, ipc.RelayBadRequest, resp.Error.Code, "%v", req)
	}
	assert.False(t, f.isSplit())
	assert.Zero(t, f.changed)
}

// With neither client's file there, removed by hand or never written, there is nothing to fold back.
// Folding nothing onto config.toml would empty it; it is left exactly as it is, and the toggle goes off.
func TestUnsplitWithNeitherFileLeavesTheSharedConfigAlone(t *testing.T) {
	f := newFixture(t)
	f.write(f.files.Shared, shared)
	f.ok(ipc.PathConfigSplit)
	require.NoError(t, os.Remove(f.files.TUI))
	require.NoError(t, os.Remove(f.files.GUI))

	out := f.ok(ipc.PathConfigUnsplit)
	assert.Equal(t, shared, f.read(f.files.Shared))
	assert.Empty(t, out.Backups)
	assert.False(t, f.isSplit())
	f.missing(f.files.Shared + config.BackupSuffix)
}

// With one of the two gone, the other is the whole of what the clients had, and is what config.toml
// becomes.
func TestUnsplitWithOneFileTakesIt(t *testing.T) {
	f := newFixture(t)
	f.write(f.files.Shared, shared)
	f.ok(ipc.PathConfigSplit)
	require.NoError(t, os.Remove(f.files.TUI))
	f.write(f.files.GUI, "[shared]\nclock = \"24h\"\n")

	out := f.ok(ipc.PathConfigUnsplit)
	assert.Equal(t, f.files.GUI, out.Base)
	assert.Equal(t, "[shared]\nclock = \"24h\"\n", f.read(f.files.Shared))
}

// Something where a client's file would go that is not a file is not read and not replaced.
func TestSplitRefusesWhatIsNotAFileWithoutReadingIt(t *testing.T) {
	f := newFixture(t)
	f.write(f.files.Shared, shared)
	require.NoError(t, os.Mkdir(f.files.TUI, 0o700))
	assert.Contains(t, f.refused(ipc.PathConfigSplit), "config.tui.toml")
	assert.False(t, f.isSplit())

}

// The files an unsplit sets aside are moved while it still holds the state's lock. Let go first, a split
// asked for at that moment could find them in place as copies of config.toml, take them as its own, and
// have them moved out from under it: the toggle on, and no file behind either client. A split made from
// inside the move is told to wait instead.
func TestUnsplitSetsItsFilesAsideBeforeLettingAnotherRequestIn(t *testing.T) {
	f := newFixture(t)
	f.write(f.files.Shared, shared)
	f.ok(ipc.PathConfigSplit)

	asked := 0
	f.h.rename = func(from, to string) error {
		asked++
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		resp := f.h.Do(ctx, ipc.Request{ID: "2", Method: "POST", Path: ipc.PathConfigSplit})
		require.NotNil(t, resp.Error, "a split got in while the unsplit was still moving files")
		assert.Contains(t, resp.Error.Message, "another request")
		return os.Rename(from, to)
	}
	f.ok(ipc.PathConfigUnsplit)
	assert.Equal(t, 2, asked)
	assert.False(t, f.isSplit())
	f.missing(f.files.TUI)
}

// A second unsplit does not overwrite what the first set aside. Each copy is the only copy of what that
// unsplit replaced, and the answer says nothing was deleted.
func TestASecondUnsplitKeepsTheFirstOnesCopies(t *testing.T) {
	f := newFixture(t)
	f.write(f.files.Shared, shared)
	f.ok(ipc.PathConfigSplit)
	f.write(f.files.TUI, "[tui.colors]\naccent = 1\n")
	first := f.ok(ipc.PathConfigUnsplit)
	firstTUI := f.read(f.files.TUI + config.BackupSuffix)

	f.ok(ipc.PathConfigSplit)
	f.write(f.files.TUI, "[tui.colors]\naccent = 2\n")
	second := f.ok(ipc.PathConfigUnsplit)

	assert.Equal(t, firstTUI, f.read(f.files.TUI+config.BackupSuffix), "the first copy is as it was")
	assert.Equal(t, "[tui.colors]\naccent = 2\n", f.read(f.files.TUI+config.BackupSuffix+".2"))
	assert.Equal(t, shared, f.read(f.files.Shared+config.BackupSuffix))
	assert.Equal(t, "[tui.colors]\naccent = 1\n\n[shared]\nclock = \"12h\"\n", f.read(f.files.Shared+config.BackupSuffix+".2"),
		"config.toml as the first unsplit left it, which the second replaced")
	for _, name := range first.Backups {
		assert.NotContains(t, second.Backups, name)
	}
}

// An editor takes no lock. A save to a client's file while the unsplit is working on it is not lost into
// a set-aside copy: the files are read again once the work is done, and the work redone on what they say
// now.
func TestASaveDuringAnUnsplitIsInWhatItWrites(t *testing.T) {
	f := newFixture(t)
	f.write(f.files.Shared, shared)
	f.ok(ipc.PathConfigSplit)
	saves := 0
	f.h.saved = func() {
		if saves++; saves == 1 {
			f.write(f.files.TUI, shared+"dim = 244\n")
		}
	}
	f.ok(ipc.PathConfigUnsplit)
	assert.Equal(t, 2, saves, "a second pass, which found nothing changed")
	assert.Equal(t, shared+"dim = 244\n", f.read(f.files.Shared))
	assert.Equal(t, shared, f.read(f.files.Shared+config.BackupSuffix),
		"the copy kept of config.toml is the person's, not the first pass's own work")
}

// And a save to config.toml while a split copies it: the copies are of what it says now.
func TestASaveDuringASplitIsInTheCopies(t *testing.T) {
	f := newFixture(t)
	f.write(f.files.Shared, shared)
	saves := 0
	f.h.saved = func() {
		if saves++; saves == 1 {
			f.write(f.files.Shared, shared+"dim = 244\n")
		}
	}
	f.ok(ipc.PathConfigSplit)
	assert.Equal(t, shared+"dim = 244\n", f.read(f.files.TUI))
	assert.Equal(t, shared+"dim = 244\n", f.read(f.files.GUI))
}

// A file that is saved every time it is looked at is given up on, saying so, with the toggle where it was
// and every file a client reads in place.
func TestAFileThatKeepsChangingIsGivenUpOn(t *testing.T) {
	f := newFixture(t)
	f.write(f.files.Shared, shared)
	f.ok(ipc.PathConfigSplit)
	n := 0
	f.h.saved = func() {
		n++
		f.write(f.files.GUI, fmt.Sprintf("[shared]\nclock = \"24h\" # %d\n", n))
	}
	assert.Contains(t, f.refused(ipc.PathConfigUnsplit), "kept being saved")
	assert.True(t, f.isSplit())
	assert.Equal(t, shared, f.read(f.files.TUI))
}

// `norite config set` takes the lock of the file it writes, and the unsplit holds that lock until the file
// has been set aside. A set that was waiting gets the lock, asks which file is its own now, and writes
// config.toml: it does not land in the file just moved away, and does not bring that file back.
func TestASetWaitingOnAnUnsplitLandsInTheSharedFile(t *testing.T) {
	f := newFixture(t)
	f.write(f.files.Shared, shared)
	f.ok(ipc.PathConfigSplit)

	type result struct {
		path string
		err  error
	}
	done := make(chan result, 1)
	started := false
	// While the unsplit holds the files and the toggle still says split: the set asks which file is its
	// own, is told the terminal client's, and waits for it.
	f.h.saved = func() {
		if started {
			return
		}
		started = true
		go func() {
			path, err := config.SetFor(config.TUI, config.TUI, "colors.dim", "244")
			done <- result{path, err}
		}()
		time.Sleep(200 * time.Millisecond)
	}
	f.ok(ipc.PathConfigUnsplit)
	r := <-done
	require.NoError(t, r.err)
	assert.Equal(t, f.files.Shared, r.path)
	assert.Equal(t, shared+"dim = 244\n", f.read(f.files.Shared))
	f.missing(f.files.TUI)
}

// What stops a toggle because of a file as it stands is a refusal the person can act on, not a failure of
// the daemon: a read-only config.toml, a setting inside an inline table.
func TestAFileTheToggleCannotWriteIsARefusalNotAFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes")
	}
	f := newFixture(t)
	f.write(f.files.Shared, shared)
	f.ok(ipc.PathConfigSplit)
	f.write(f.files.TUI, "[tui.colors]\naccent = 5\n")
	require.NoError(t, os.Chmod(f.files.Shared, 0o400))
	msg := f.refused(ipc.PathConfigUnsplit)
	assert.Contains(t, msg, "config.toml")
	assert.True(t, f.isSplit())
	require.NoError(t, os.Chmod(f.files.Shared, 0o600))

	// [shared] written as an inline table in the newer file, and a shared key only the older file sets:
	// placing it would mean rewriting that table.
	f.write(f.files.TUI, "[shared]\nclock = \"12h\"\n")
	f.writeAt(f.files.GUI, "shared = {}\n", time.Now().Add(time.Hour))
	msg = f.refused(ipc.PathConfigUnsplit)
	assert.Contains(t, msg, "inline table")
	assert.True(t, f.isSplit())

	// A client's own section written that way in the other file's stale copy is no obstacle: the stale
	// copy goes before the owner's is written.
	f.write(f.files.TUI, "[tui.colors]\naccent = 5\n")
	f.writeAt(f.files.GUI, "[tui]\ncolors = { accent = 6 }\n", time.Now().Add(time.Hour))
	f.ok(ipc.PathConfigUnsplit)
	assert.Contains(t, f.read(f.files.Shared), "accent = 5")
	assert.NotContains(t, f.read(f.files.Shared), "accent = 6")
}

// Where the daemon keeps configs and how the toggle stands, asked with GET and changing nothing. A command
// asks before a toggle, to find a daemon that means another directory than the shell does.
func TestTheDaemonSaysWhereItKeepsConfigs(t *testing.T) {
	f := newFixture(t)
	ask := func() ipc.ConfigLocation {
		resp := f.do("GET", ipc.PathConfig)
		require.Nil(t, resp.Error)
		var out ipc.ConfigLocation
		require.NoError(t, json.Unmarshal(resp.Body, &out))
		return out
	}
	assert.Equal(t, ipc.ConfigLocation{Dir: f.h.ConfigDir, Split: false}, ask())
	f.ok(ipc.PathConfigSplit)
	assert.Equal(t, ipc.ConfigLocation{Dir: f.h.ConfigDir, Split: true}, ask())
	assert.Equal(t, 1, f.changed, "asking announces nothing")

	resp := f.do("POST", ipc.PathConfig)
	require.NotNil(t, resp.Error)
	assert.Equal(t, ipc.RelayBadRequest, resp.Error.Code)
}

// config.toml is read by nobody while split, and somebody may save it all the same, even while the unsplit
// is on its second pass. The copy kept is of what they last saved, not of what the first pass wrote.
func TestTheCopyKeptOfTheSharedConfigIsThePersonsLatest(t *testing.T) {
	f := newFixture(t)
	f.write(f.files.Shared, shared)
	f.ok(ipc.PathConfigSplit)
	f.write(f.files.TUI, shared+"dim = 244\n")
	saves := 0
	f.h.saved = func() {
		if saves++; saves == 1 {
			f.write(f.files.Shared, "# saved while the unsplit worked\n")
			f.write(f.files.GUI, shared+"warn = 3\n")
		}
	}
	f.ok(ipc.PathConfigUnsplit)
	assert.Equal(t, "# saved while the unsplit worked\n", f.read(f.files.Shared+config.BackupSuffix))
	assert.Contains(t, f.read(f.files.Shared), "dim = 244")
}

// And when nobody saved config.toml in between, a second pass finds the first pass's own work there. That
// is not the person's: the copy kept is still of the file as the unsplit found it.
func TestASecondPassDoesNotKeepTheFirstPassesWorkAsThePersons(t *testing.T) {
	f := newFixture(t)
	f.write(f.files.Shared, shared)
	f.ok(ipc.PathConfigSplit)
	f.write(f.files.Shared, "# edited while split, read by nobody\n")
	saves := 0
	f.h.saved = func() {
		if saves++; saves == 1 {
			// A change to [shared], which is the GUI's to make: its copy of [tui] would decide nothing.
			f.write(f.files.GUI, strings.Replace(shared, "12h", "24h", 1))
		}
	}
	f.ok(ipc.PathConfigUnsplit)
	assert.Equal(t, 2, saves)
	assert.Contains(t, f.read(f.files.Shared), `clock = "24h"`, "the second pass wrote something else")
	assert.Equal(t, "# edited while split, read by nobody\n", f.read(f.files.Shared+config.BackupSuffix))
}

// A refused unsplit leaves nothing behind. The copy of config.toml is made inside the write that replaces
// it, and when that write does not happen the copy is of a file that was not replaced: each refused
// attempt left another, and after a hundred the unsplit refused for want of a name.
func TestARefusedUnsplitLeavesNoCopyBehind(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes")
	}
	f := newFixture(t)
	f.write(f.files.Shared, shared)
	f.ok(ipc.PathConfigSplit)
	f.write(f.files.TUI, "[tui.colors]\naccent = 5\n")
	require.NoError(t, os.Chmod(f.files.Shared, 0o400))
	for range 3 {
		f.refused(ipc.PathConfigUnsplit)
	}
	left, err := filepath.Glob(f.files.Shared + config.BackupSuffix + "*")
	require.NoError(t, err)
	assert.Empty(t, left)

	require.NoError(t, os.Chmod(f.files.Shared, 0o600))
	out := f.ok(ipc.PathConfigUnsplit)
	assert.Contains(t, out.Backups, f.files.Shared+config.BackupSuffix, "and the one that works takes the first name")
}

// Whether a file is there is not read off its time. Two files restored from an archive can both carry the
// epoch, and were taken for two missing ones: the toggle went off with no merge and both were set aside.
func TestFilesDatedAtTheEpochAreStillThere(t *testing.T) {
	f := newFixture(t)
	f.write(f.files.Shared, shared)
	f.ok(ipc.PathConfigSplit)
	epoch := time.Unix(0, 0)
	f.writeAt(f.files.TUI, "[tui.colors]\naccent = 5\n", epoch)
	f.writeAt(f.files.GUI, "[shared]\nclock = \"24h\"\n", epoch)

	out := f.ok(ipc.PathConfigUnsplit)
	assert.Equal(t, f.files.TUI, out.Base, "a tie is the terminal client's")
	assert.Equal(t, "[tui.colors]\naccent = 5\n\n[shared]\nclock = \"24h\"\n", f.read(f.files.Shared))

	// And a file that is there is newer than one that is not, whatever it is dated.
	f.ok(ipc.PathConfigSplit)
	require.NoError(t, os.Remove(f.files.TUI))
	f.writeAt(f.files.GUI, "[shared]\nclock = \"12h\"\n", epoch)
	out = f.ok(ipc.PathConfigUnsplit)
	assert.Equal(t, f.files.GUI, out.Base)
}
