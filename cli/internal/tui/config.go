// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"errors"
	"fmt"
	"path/filepath"

	tea "charm.land/bubbletea/v2"

	"github.com/Alexnex31/Norite/daemon/config"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// ConfigReader reads the config as this client sees it, and says which file that was by its name alone:
// config.toml, or config.tui.toml while the same-machine toggle has given each client its own. The name
// is what the hint row calls the file when something in it needs a look.
type ConfigReader func() (cfg *config.Config, file string, err error)

// FileConfig reads the config the terminal client reads: the user's config.toml, or config.tui.toml while
// the same-machine toggle has given each client its own. A missing file is the defaults.
func FileConfig() (*config.Config, string, error) {
	path, _, err := config.PathFor(config.TUI)
	if err != nil {
		// Which file could not be worked out, so the one everybody knows is named.
		return nil, "", err
	}
	cfg, err := config.Load(path, config.TUI)
	return cfg, filepath.Base(path), err
}

// configMsg answers one read. seq numbers the reads: the daemon reports every save, each read runs on its
// own goroutine, and an older read finishing last must not put back the settings a newer one replaced.
type configMsg struct {
	seq  int
	cfg  *config.Config
	file string
	err  error
}

// reloadConfig reads the config again, off the update loop: the file is on a disk, and may be on a
// network one.
func (m *Model) reloadConfig() tea.Cmd {
	read := m.opts.Config
	m.configSeq++
	seq := m.configSeq
	return func() tea.Msg {
		cfg, file, err := read()
		return configMsg{seq: seq, cfg: cfg, file: file, err: err}
	}
}

// applyConfig takes what a read found.
//
// A file that does not load changes nothing: an editor saves a file mid-thought, and a client that fell
// back to its defaults on every half-typed line would flash between two palettes while somebody edits.
// What was last read stays in force and the hint row says the file was not applied, until one loads.
//
// A note that differs from the last read's is news, and until a key is pressed it is shown over whatever
// the hint row was saying: somebody who has just saved is looking for the result, and a client that is
// signed out or waiting for its daemon shows a status line for as long as that lasts. The same note again
// is not: the file is read on every attach, and a warning somebody has already pressed a key past would
// otherwise come back over the status line each time the daemon restarted, with nothing saved (M21
// /code-review).
func (m *Model) applyConfig(cfg *config.Config, file string, err error) {
	// The file is named as it is: while split it is config.tui.toml, and a note sending somebody to line 3
	// of config.toml, which is read by nobody then and parses fine, sent them to the wrong file (M21
	// /code-review). The name is a file's and is drawn, so it is made safe like the rest.
	if file == "" {
		file = "config.toml"
	}
	note, broken := "", err != nil
	switch {
	case broken:
		// What happened first: the error names the file by its whole path, and a row cut to the terminal's
		// width should lose that rather than the consequence. The text is the file's and the filesystem's —
		// a path can hold anything a filename can — so it is sanitized as any foreign text is (rule 19).
		why := err.Error()
		var pe *config.ParseError
		if errors.As(err, &pe) {
			// Where in the file, without the file's path in front of it: the row has the name already, and a
			// home directory's worth of path pushed the line number off an 80-column terminal.
			why = pe.Error()
		}
		note = termsafe.Text(file + " was not applied · " + why)
	default:
		m.setLook(newLook(cfg))
		if len(cfg.Warnings) > 0 {
			// One is enough to say the file needs a look; `norite config get` lists them all.
			note = file + ": " + cfg.Warnings[0].String()
			if more := len(cfg.Warnings) - 1 + cfg.MoreWarnings; more > 0 {
				note += fmt.Sprintf(" (and %d more; see `norite config get`)", more)
			}
			note = termsafe.Text(note)
		}
	}
	changed := note != m.configRead
	m.configRead, m.configBroken = note, broken
	if changed || broken {
		m.configNote = note
	}
	m.configNews = changed && note != ""
}

// setLook replaces how everything on screen is drawn. Nothing is laid out here: a frame lays out what it
// draws, so the next one is simply drawn with this.
func (m *Model) setLook(l *look) {
	m.look, m.home.look = l, l
	if m.pane != nil {
		m.pane.look = l
	}
}

// newPane opens a pane drawn as the rest of the client is.
func (m *Model) newPane(guildID, channelID string) *paneModel {
	p := newPane(guildID, channelID)
	p.look = m.look
	return p
}
