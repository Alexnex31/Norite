// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"errors"
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/Alexnex31/Norite/daemon/config"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// ConfigReader reads config.toml as this client sees it.
type ConfigReader func() (*config.Config, error)

// FileConfig reads the user's config.toml. A missing file is the defaults.
func FileConfig() (*config.Config, error) {
	path, err := config.Path()
	if err != nil {
		return nil, err
	}
	return config.Load(path, config.TUI)
}

// configMsg answers one read. seq numbers the reads: the daemon reports every save, each read runs on its
// own goroutine, and an older read finishing last must not put back the settings a newer one replaced.
type configMsg struct {
	seq int
	cfg *config.Config
	err error
}

// reloadConfig reads the config again, off the update loop: the file is on a disk, and may be on a
// network one.
func (m *Model) reloadConfig() tea.Cmd {
	read := m.opts.Config
	if read == nil {
		return nil
	}
	m.configSeq++
	seq := m.configSeq
	return func() tea.Msg {
		cfg, err := read()
		return configMsg{seq: seq, cfg: cfg, err: err}
	}
}

// applyConfig takes what a read found.
//
// A file that does not load changes nothing: an editor saves a file mid-thought, and a client that fell
// back to its defaults on every half-typed line would flash between two palettes while somebody edits.
// What was last read stays in force and the hint row says the file was not applied, until one loads.
//
// A note that differs from the last one is news, and until a key is pressed it is shown over whatever the
// hint row was saying: somebody who has just saved is looking for the result, and a client that is signed
// out or waiting for its daemon shows a status line for as long as that lasts.
func (m *Model) applyConfig(cfg *config.Config, err error) {
	was := m.configNote
	defer func() { m.configNews = m.configNote != "" && m.configNote != was }()
	if err != nil {
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
		m.configNote = termsafe.Text("config.toml was not applied · " + why)
		m.configBroken = true
		return
	}
	m.setLook(newLook(cfg))
	m.configNote, m.configBroken = "", false
	if len(cfg.Warnings) > 0 {
		// One is enough to say the file needs a look; `norite config get` lists them all.
		note := "config.toml: " + cfg.Warnings[0].String()
		if more := len(cfg.Warnings) - 1 + cfg.MoreWarnings; more > 0 {
			note += fmt.Sprintf(" (and %d more; see `norite config get`)", more)
		}
		m.configNote = termsafe.Text(note)
	}
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
