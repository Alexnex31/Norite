// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package toggle

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/config"
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
	f := &fixture{t: t}
	f.h = &Handler{
		ConfigDir: filepath.Join(root, "cfg"), StateDir: filepath.Join(root, "state"),
		Log: zerolog.Nop(), Changed: func() { f.changed++ },
	}
	require.NoError(t, os.MkdirAll(f.h.ConfigDir, 0o700))
	require.NoError(t, os.MkdirAll(f.h.StateDir, 0o700))
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
// its own file while split; unsplit keeps every key either set. Where both set one, the file written more
// recently wins, per key and not per file, so the older file's other settings are not dropped with it.
func TestUnsplitKeepsWhatEachClientSet(t *testing.T) {
	f := newFixture(t)
	f.write(f.files.Shared, shared)
	f.ok(ipc.PathConfigSplit)

	old, recent := time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
	f.writeAt(f.files.TUI, "# the terminal's\n[shared]\nclock = \"12h\"\n\n[tui.colors]\naccent = 208 # orange\ndim = 244\n", old)
	f.writeAt(f.files.GUI, "# the window's\n[shared]\nclock = \"24h\"\n\n[tui.colors]\naccent = 9\n\n[gui]\ntheme = \"paper\"\n", recent)

	out := f.ok(ipc.PathConfigUnsplit)
	assert.False(t, out.Split)
	assert.Equal(t, []string{f.files.Shared}, out.Files)
	assert.Equal(t, f.files.GUI, out.Base, "the more recently written file")
	assert.Equal(t, []string{"tui.colors.dim"}, out.Merged, "what only the older file set")
	assert.ElementsMatch(t, []string{"shared.clock", "tui.colors.accent"}, out.Kept, "where both set a key, the newer file's stays")

	assert.Equal(t, "# the window's\n[shared]\nclock = \"24h\"\n\n[tui.colors]\naccent = 9\ndim = 244\n\n[gui]\ntheme = \"paper\"\n",
		f.read(f.files.Shared), "the base's bytes, with the other file's key added where its table is")
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

// The other way round: the terminal's file is the newer, so it is the base and its values win.
func TestTheMoreRecentlyWrittenFileIsTheBaseWhicheverItIs(t *testing.T) {
	f := newFixture(t)
	f.ok(ipc.PathConfigSplit)
	f.writeAt(f.files.GUI, "[shared]\nclock = \"24h\"\n[tui]\ntheme = \"paper\"\n", time.Now().Add(-2*time.Hour))
	f.writeAt(f.files.TUI, "[shared]\nclock = \"12h\"\n", time.Now().Add(-time.Hour))

	out := f.ok(ipc.PathConfigUnsplit)
	assert.Equal(t, f.files.TUI, out.Base)
	assert.Equal(t, "[shared]\nclock = \"12h\"\n\n[tui]\ntheme = \"paper\"\n", f.read(f.files.Shared))
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

// What the older file holds that this version does not understand cannot be carried over by key. It is
// said, and it is still in the copy set aside.
func TestWhatUnsplitCannotCarryOverIsSaid(t *testing.T) {
	f := newFixture(t)
	f.ok(ipc.PathConfigSplit)
	f.writeAt(f.files.TUI, "[tui]\nfrom_a_newer_norite = 1\n", time.Now().Add(-2*time.Hour))
	f.writeAt(f.files.GUI, "[shared]\nclock = \"24h\"\n", time.Now().Add(-time.Hour))

	out := f.ok(ipc.PathConfigUnsplit)
	require.Len(t, out.Skipped, 1)
	assert.Contains(t, out.Skipped[0], "tui.from_a_newer_norite")
	assert.Contains(t, f.read(f.files.TUI+config.BackupSuffix), "from_a_newer_norite")
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
