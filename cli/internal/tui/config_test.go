// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/daemon/config"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/statefile"
)

// What a color looks like once drawn: lipgloss writes an exact color as 38;2;r;g;b and the terminal's own
// sixteen by their SGR number, so the default accent, ANSI 6, is 36.
const (
	sgrRed       = "38;2;255;0;0"
	sgrGreen     = "38;2;0;255;0"
	sgrBlue      = "38;2;0;0;255"
	sgrANSI6     = "\x1b[1;36m" // the selected row: accent, bold
	redAccent    = "[tui.colors]\naccent = \"#ff0000\"\n"
	greenBright  = "[tui.colors]\nbright = \"#00ff00\"\n"
	blueAccent   = "[tui.colors]\naccent = \"#0000ff\"\n"
	twelveHour   = "[shared]\nclock = \"12h\"\n"
	notTOMLAtAll = "[tui.colors\naccent = \n"
)

// file stands in for config.toml: a test saves to it as an editor would, and the client reads it through
// the loader the real file goes through.
type file struct {
	mu   sync.Mutex
	text string
	err  error
}

func (f *file) save(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.text, f.err = text, nil
}

func (f *file) read() (*config.Config, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, "config.toml", f.err
	}
	cfg, err := config.Parse([]byte(f.text), config.TUI)
	return cfg, "config.toml", err
}

// untilRaw waits on the frame with its styling, which is where a color is.
func (d *driver) untilRaw(what string, cond func(raw string) bool) {
	d.t.Helper()
	d.until(what, func(string) bool { return cond(d.raw()) })
}

func (d *driver) draws(sgr string) {
	d.t.Helper()
	d.untilRaw("the color "+sgr, func(raw string) bool { return strings.Contains(raw, sgr) })
}

func hintRow(screen string) string {
	rows := strings.Split(screen, "\n")
	return rows[len(rows)-1]
}

// TestAClientDrawsAsItsConfigSays: both live settings reach the screen, on home and in a pane opened from
// it. The selected row takes the accent, a name takes bright, and a message's time is written by the clock.
func TestAClientDrawsAsItsConfigSays(t *testing.T) {
	f := newFixture(t)
	f.held = []apicontract.Message{message("50", "20", "2", "Bob", "earlier")}
	cfg := &file{text: "[shared]\nclock = \"12h\"\n[tui.colors]\naccent = \"#ff0000\"\nbright = \"#00ff00\"\n"}
	c := drive(t, Options{Dial: f.dialer(signedIn("alice"), nil), Config: cfg.read}, 80, 24)

	c.shows("# general")
	assert.Contains(t, c.raw(), sgrRed, "the selected row is drawn in the configured accent")
	assert.NotContains(t, c.raw(), sgrANSI6, "and not in the default one")

	c.press("enter")
	c.shows("earlier")
	assert.Contains(t, c.raw(), sgrGreen, "a pane opened from home draws names as configured")
	assert.Regexp(t, `\d{1,2}:00 [AP]M`, c.screen(), "and writes the time on a twelve-hour clock")
}

// TestWithNoFileTheDefaultsAreDrawn: the contract's defaults, which are the colors and the clock this
// client had before it had a config.
func TestWithNoFileTheDefaultsAreDrawn(t *testing.T) {
	f := newFixture(t)
	f.held = []apicontract.Message{message("50", "20", "2", "Bob", "earlier")}
	c := drive(t, Options{Dial: f.dialer(signedIn("alice"), nil)}, 80, 24)
	c.shows("# general")
	assert.Contains(t, c.raw(), sgrANSI6)
	c.press("enter")
	c.shows("earlier")
	assert.NotRegexp(t, `[AP]M`, c.screen())
}

// TestAPaneOpenedByFlagIsDrawnAsConfigured: --channel builds its pane before anything is attached, on the
// other of the two paths that make one.
func TestAPaneOpenedByFlagIsDrawnAsConfigured(t *testing.T) {
	f := newFixture(t)
	f.held = []apicontract.Message{message("50", "20", "2", "Bob", "earlier")}
	cfg := &file{text: greenBright}
	c := drive(t, Options{Dial: f.dialer(signedIn("alice"), nil), Channel: "20", Config: cfg.read}, 80, 24)
	c.shows("earlier")
	assert.Contains(t, c.raw(), sgrGreen)
}

// TestASavedConfigIsDrawnWithoutARestart is the milestone's hot reload, seen from the client: the daemon
// says the file changed, carrying nothing, and the client reads it and draws the open pane with it.
func TestASavedConfigIsDrawnWithoutARestart(t *testing.T) {
	f := newFixture(t)
	f.held = []apicontract.Message{message("50", "20", "2", "Bob", "earlier")}
	cfg := &file{}
	c := drive(t, Options{Dial: f.dialer(signedIn("alice"), nil), Channel: "20", Config: cfg.read}, 80, 24)
	c.shows("earlier")
	require.NotContains(t, c.raw(), sgrGreen)

	cfg.save(greenBright + twelveHour)
	f.d.Dispatch(ipc.EventConfigUpdate, []byte(`{}`))
	c.draws(sgrGreen)
	assert.Regexp(t, `[AP]M`, c.screen())

	// And home, which was not on screen when the file changed, is drawn with it too.
	c.press("esc")
	assert.Contains(t, c.raw(), sgrGreen)
}

// TestAConfigThatDoesNotLoadChangesNothing: an editor saves mid-thought. The colors last read stay, the
// hint row says the file was not applied and goes on saying so past a keypress, and a save that loads
// clears it.
func TestAConfigThatDoesNotLoadChangesNothing(t *testing.T) {
	f := newFixture(t)
	cfg := &file{text: redAccent}
	c := drive(t, Options{Dial: f.dialer(signedIn("alice"), nil), Config: cfg.read}, 80, 24)
	c.shows("# general")
	require.Contains(t, c.raw(), sgrRed)

	cfg.save(notTOMLAtAll)
	f.d.Dispatch(ipc.EventConfigUpdate, []byte(`{}`))
	c.shows("config.toml was not applied")
	assert.Contains(t, hintRow(c.screen()), "line 1", "the hint row says where")
	assert.Contains(t, c.raw(), sgrRed, "what was last read is still what is drawn")
	assert.NotContains(t, c.raw(), sgrANSI6, "rather than the defaults")

	c.press("ctrl+n")
	assert.Contains(t, hintRow(c.screen()), "config.toml was not applied", "a keypress does not dismiss it")

	cfg.save(blueAccent)
	f.d.Dispatch(ipc.EventConfigUpdate, []byte(`{}`))
	c.draws(sgrBlue)
	assert.NotContains(t, c.screen(), "config.toml")
}

// TestAConfigBrokenAtStartDrawsTheDefaultsAndSaysSo: there is no last good one to keep.
func TestAConfigBrokenAtStartDrawsTheDefaultsAndSaysSo(t *testing.T) {
	f := newFixture(t)
	cfg := &file{text: notTOMLAtAll}
	c := drive(t, Options{Dial: f.dialer(signedIn("alice"), nil), Config: cfg.read}, 80, 24)
	c.shows("# general")
	assert.Contains(t, hintRow(c.screen()), "config.toml was not applied")
	assert.Contains(t, c.raw(), sgrANSI6)
}

// TestAConfigErrorIsDrawnInert: the error names a path, and a path holds whatever a filename can (rule
// 19). An escape sequence or a bidi override in it must not reach the terminal.
func TestAConfigErrorIsDrawnInert(t *testing.T) {
	f := newFixture(t)
	cfg := &file{err: errors.New("/home/a\x1b[2J\x1b]0;owned\x07/\u202etxt.lmot: permission denied\nconfig.toml is fine")}
	c := drive(t, Options{Dial: f.dialer(signedIn("alice"), nil), Config: cfg.read}, 200, 24)
	c.shows("config.toml was not applied")
	row := hintRow(c.raw())
	assert.NotContains(t, row, "\x1b[2J")
	assert.NotContains(t, row, "\x1b]0;")
	assert.NotContains(t, row, "\u202e")
	assert.Contains(t, hintRow(c.screen()), "permission denied", "what it says is still said")
	assert.Len(t, strings.Split(c.screen(), "\n"), 24, "and a line break in it does not add a row")
}

// TestAnUnappliedSettingIsSaidUntilAKeyIsPressed: the file loads, one value in it is not a color, and the
// rest applies. That is said once, where somebody who just saved will see it.
func TestAnUnappliedSettingIsSaidUntilAKeyIsPressed(t *testing.T) {
	f := newFixture(t)
	cfg := &file{}
	c := drive(t, Options{Dial: f.dialer(signedIn("alice"), nil), Config: cfg.read}, 120, 24)
	c.shows("# general")

	cfg.save("[tui.colors]\naccent = \"#ff0000\"\ndim = \"grey\"\nwarn = 999\n")
	f.d.Dispatch(ipc.EventConfigUpdate, []byte(`{}`))
	c.shows("config.toml: tui.colors.dim")
	assert.Contains(t, hintRow(c.screen()), "and 1 more")
	assert.Contains(t, c.raw(), sgrRed, "the settings that are right apply")

	c.press("ctrl+n")
	assert.NotContains(t, c.screen(), "config.toml")
}

// TestTheConfigNoteAndAStatusLineShareTheHintRow: a signed-out client shows its status line for as long as
// it is signed out. At start that comes first; a note about a file just saved is shown over it until a key
// is pressed; and attaching again, which reads the same broken file, is not news.
func TestTheConfigNoteAndAStatusLineShareTheHintRow(t *testing.T) {
	f := newFixture(t)
	cfg := &file{text: notTOMLAtAll}
	c := drive(t, Options{
		Dial:   f.dialer(ipc.Ready{Standing: ipc.StandingSignedOut, Guilds: []ipc.GuildSummary{}}, nil),
		Config: cfg.read,
	}, 80, 24)
	c.shows("signed out; run `norite login`")
	c.settle()
	assert.NotContains(t, c.screen(), "config.toml", "at start, and after the read attaching makes")

	cfg.save("[tui.colors]\ndim = \"grey\"\n")
	f.d.Dispatch(ipc.EventConfigUpdate, []byte(`{}`))
	c.shows("config.toml: tui.colors.dim")
	assert.NotContains(t, c.screen(), "signed out")

	c.press("ctrl+n")
	assert.Contains(t, hintRow(c.screen()), "signed out; run `norite login`")
}

// TestAWarningAlreadySeenIsNotRaisedAgainByAttaching: the file is read on every attach. A warning somebody
// pressed a key past stays dismissed when the daemon restarts, and does not come back over the status line
// the attach just set; a different warning, from a save made while detached, is news.
func TestAWarningAlreadySeenIsNotRaisedAgainByAttaching(t *testing.T) {
	f := newFixture(t)
	cfg := &file{text: "[tui.colors]\ndim = \"grey\"\n"}
	var dials atomic.Int64
	sw := &switchable{f: f, ready: signedInAs("alice", "1")}
	c := drive(t, Options{Dial: func(ctx context.Context) (Session, error) {
		dials.Add(1)
		return sw.dial(ctx)
	}, Config: cfg.read}, 120, 24)
	c.shows("# general")
	c.settle()
	require.Contains(t, hintRow(c.screen()), "config.toml: tui.colors.dim")
	c.press("ctrl+n")
	require.NotContains(t, c.screen(), "config.toml")

	sw.set(ipc.Ready{Standing: ipc.StandingSignedOut, Guilds: []ipc.GuildSummary{}})
	f.d.Drop(&ipc.CloseError{Code: ipc.CloseResync, Reason: "resync"})
	c.shows("signed out; run `norite login`")
	c.settle()
	require.EqualValues(t, 2, dials.Load())
	assert.Contains(t, hintRow(c.screen()), "signed out", "the same warning, read again, is not news")

	// Nor does it come back where there is no status line for it to hide behind.
	sw.set(signedInAs("alice", "1"))
	f.d.Drop(&ipc.CloseError{Code: ipc.CloseResync, Reason: "resync"})
	c.shows("# general")
	c.settle()
	assert.NotContains(t, c.screen(), "config.toml", "dismissed is dismissed until the file says something else")

	cfg.save("[tui.colors]\nwarn = \"amber\"\n")
	f.d.Drop(&ipc.CloseError{Code: ipc.CloseResync, Reason: "resync"})
	c.shows("config.toml: tui.colors.warn")
}

// TestABrokenConfigAtStartDoesNotHideWhatTheClientIsWaitingFor: with no daemon there is no attach to read
// the file a second time, and the retry line is the one thing the person is waiting to learn.
func TestABrokenConfigAtStartDoesNotHideWhatTheClientIsWaitingFor(t *testing.T) {
	cfg := &file{text: notTOMLAtAll}
	c := drive(t, Options{Dial: func(context.Context) (Session, error) {
		return nil, errors.New("the daemon is not running")
	}, Config: cfg.read}, 80, 24)
	c.shows("trying again in 1s")
}

// TestAnOlderReadDoesNotUndoANewerOne: every save is reported and every read runs on its own goroutine, so
// two can finish out of order.
func TestAnOlderReadDoesNotUndoANewerOne(t *testing.T) {
	cfg := &file{text: redAccent}
	m := New(Options{Config: cfg.read})
	older := m.reloadConfig()().(configMsg)
	cfg.save(blueAccent)
	newer := m.reloadConfig()().(configMsg)

	next, _ := m.Update(newer)
	next, _ = next.Update(older)
	got := next.(Model).look.selected.Render("x")
	assert.Contains(t, got, sgrBlue)
	assert.NotContains(t, got, sgrRed)
}

// TestAClientWithNoDaemonStillReadsItsConfig: the file is read when the client starts, whether or not
// anything is there to report a change to it.
func TestAClientWithNoDaemonStillReadsItsConfig(t *testing.T) {
	cfg := &file{text: greenBright}
	c := drive(t, Options{Dial: func(context.Context) (Session, error) {
		return nil, errors.New("the daemon is not running")
	}, Config: cfg.read}, 80, 24)
	c.shows("trying again")
	assert.Contains(t, c.raw(), sgrGreen)
}

// TestWhatWasSavedWhileDetachedIsReadOnAttaching: the daemon reports a change to the clients attached when
// it happens. A client that was not reads the file when it attaches again, having been told nothing.
func TestWhatWasSavedWhileDetachedIsReadOnAttaching(t *testing.T) {
	f := newFixture(t)
	cfg := &file{}
	c := drive(t, Options{Dial: f.dialer(signedIn("alice"), nil), Config: cfg.read}, 80, 24)
	c.shows("# general")
	require.NotContains(t, c.raw(), sgrRed)

	cfg.save(redAccent)
	f.d.Drop(&ipc.CloseError{Code: ipc.CloseResync, Reason: "resync"})
	c.draws(sgrRed)
}

// TestTheUsersOwnFileIsWhatIsRead: FileConfig reads config.toml where the config package puts it, as the
// terminal client sees it — its own section over [shared], and nothing of the GUI's.
func TestTheUsersOwnFileIsWhatIsRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("APPDATA", home)
	t.Setenv("HOME", home)

	cfg, _, err := FileConfig()
	require.NoError(t, err, "no file is the defaults")
	assert.Equal(t, config.Clock24h, cfg.Clock())

	dir := filepath.Join(home, "norite")
	if runtime.GOOS == "windows" {
		dir = filepath.Join(home, "Norite")
	}
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"),
		[]byte("[shared]\nclock = \"12h\"\n[tui.colors]\naccent = 5\n[gui.colors]\naccent = 9\n"), 0o600))

	cfg, _, err = FileConfig()
	require.NoError(t, err)
	assert.Equal(t, config.Clock12h, cfg.Clock())
	assert.EqualValues(t, "5", cfg.Color(config.KeyColorAccent))
}

// TestASyntaxErrorIsNamedByItsLineAndNotItsPath: the real loader puts the file's whole path in front of the
// line, which on an 80-column row leaves no room for it.
func TestASyntaxErrorIsNamedByItsLineAndNotItsPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("APPDATA", home)
	t.Setenv("HOME", home)
	path, err := config.Path()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("[tui.colors]\naccent = 6\ndim = \n"), 0o600))

	m := New(Options{Config: FileConfig})
	assert.True(t, m.configBroken)
	assert.Contains(t, m.configNote, "config.toml was not applied · line 3")
	assert.NotContains(t, m.configNote, home)
}

// TestWhileSplitTheClientReadsItsOwnFile: the same-machine toggle gives the terminal client
// config.tui.toml, and config.toml is then read by neither client. The toggle is the daemon's to change
// and is read here from its state file, so this holds with no daemon running.
func TestWhileSplitTheClientReadsItsOwnFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("writes the state file where Linux keeps it; the lookup itself is tested in daemon/config")
	}
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("XDG_STATE_HOME", home)
	dir := filepath.Join(home, "norite")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(redAccent), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.tui.toml"), []byte(blueAccent), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.gui.toml"), []byte(greenBright), 0o600))

	m := New(Options{})
	require.Contains(t, m.look.selected.Render("x"), sgrRed, "not split: config.toml")

	require.NoError(t, os.WriteFile(statefile.PathIn(dir), []byte(`{"version":1,"config_split":true}`), 0o600))
	next, _ := m.Update(m.reloadConfig()())
	got := next.(Model)
	assert.Contains(t, got.look.selected.Render("x"), sgrBlue, "split: the terminal client's own file")
	assert.NotContains(t, got.look.bold.Render("x"), sgrGreen, "and nothing of the GUI's")

	// A state file the client cannot read is said, and the settings in use stay: guessing which file is
	// its own would draw somebody else's.
	require.NoError(t, os.WriteFile(statefile.PathIn(dir), []byte(`{"version":99}`), 0o600))
	next, _ = got.Update(got.reloadConfig()())
	got = next.(Model)
	assert.True(t, got.configBroken)
	assert.Contains(t, got.configNote, "newer version")
	assert.Contains(t, got.look.selected.Render("x"), sgrBlue)

	// And a mistake in its own file is said of its own file. config.toml is read by nobody while split
	// and parses fine: named in the note, it is where somebody would go looking for line 2.
	require.NoError(t, os.WriteFile(statefile.PathIn(dir), []byte(`{"version":1,"config_split":true}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.tui.toml"), []byte("[tui.colors]\naccent = \n"), 0o600))
	next, _ = got.Update(got.reloadConfig()())
	got = next.(Model)
	assert.Contains(t, got.configNote, "config.tui.toml was not applied · line 2")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.tui.toml"), []byte("[tui.colors]\ndim = \"grey\"\n"), 0o600))
	next, _ = got.Update(got.reloadConfig()())
	assert.Contains(t, next.(Model).configNote, "config.tui.toml: tui.colors.dim")
}

// TestAClientGivenNoReaderReadsTheUsersFile: the default is the real file, so a caller that names no reader
// still runs a client that honors its user's settings. TestMain is why that is safe to be the default
// here.
func TestAClientGivenNoReaderReadsTheUsersFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("APPDATA", home)
	t.Setenv("HOME", home)
	path, err := config.Path()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(blueAccent), 0o600))

	m := New(Options{})
	assert.Contains(t, m.look.selected.Render("x"), sgrBlue, "read at start")

	require.NoError(t, os.WriteFile(path, []byte(redAccent), 0o600))
	next, _ := m.Update(m.reloadConfig()())
	assert.Contains(t, next.(Model).look.selected.Render("x"), sgrRed, "and read again on a change")
}

// TestALookIsBuiltOncePerReadAndNotPerFrame: a frame is drawn on every keystroke, and the styles are the
// same value from one to the next until the config is read again.
func TestALookIsBuiltOncePerReadAndNotPerFrame(t *testing.T) {
	f := newFixture(t)
	cfg := &file{text: redAccent}
	c := drive(t, Options{Dial: f.dialer(signedIn("alice"), nil), Channel: "20", Config: cfg.read}, 80, 24)
	c.shows("No messages yet")
	c.settle()
	before := c.m.(Model).look
	c.typeText("hello")
	_ = c.raw()
	after := c.m.(Model)
	assert.Same(t, before, after.look)
	assert.Same(t, before, after.pane.look)
	assert.Same(t, before, after.home.look)
}
