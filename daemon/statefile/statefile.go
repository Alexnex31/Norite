// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package statefile reads state.json, the file the daemon keeps for what only it writes (M21).
//
// config.toml is the user's: hand-edited, commented, exported, kept in a dotfiles repository. This file is
// the other half, in the state directory beside the daemon's lock: never edited by hand, never exported,
// and machine-local by nature. At M21 it holds one thing, whether the terminal client and the GUI read
// separate config files; the voice-channel breadcrumb (M36) and plugin grants and pinned hashes (M89) join
// it.
//
// This package only reads. A client needs to know which config file is its own, with or without a daemon
// running, so reading is public; writing is daemon/internal/statefile, which nothing outside the daemon
// module can import. "The daemon is its only writer" is the compiler's to hold, not a habit.
//
// JSON rather than TOML because nobody edits it, and versioned because a later milestone will change it.
package statefile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Alexnex31/Norite/daemon/internal/paths"
)

// Version is the format this build writes and the newest it reads.
const Version = 1

// FileName is the file's name in the state directory.
const FileName = "state.json"

// MaxSize bounds what is read. The file holds a handful of fields; one larger than this is not ours.
const MaxSize = 64 << 10

// ErrNewer reports a state file written by a newer Norite than this one. It is refused rather than read
// as far as it is understood: a field this build does not know may change what the known ones mean.
var ErrNewer = errors.New("was written by a newer version of Norite than this one")

// State is what the file holds.
type State struct {
	Version int `json:"version"`
	// ConfigSplit is the same-machine toggle: when set, the terminal client reads config.tui.toml and the
	// GUI config.gui.toml, and config.toml is read by neither.
	ConfigSplit bool `json:"config_split"`
}

// PathIn returns where the file is in a state directory.
func PathIn(stateDir string) string { return filepath.Join(stateDir, FileName) }

// Read returns the current user's state. No file is the zero state, which is every default.
func Read() (State, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return State{}, err
	}
	return ReadIn(dir)
}

// ReadIn reads the state file in a state directory.
func ReadIn(stateDir string) (State, error) {
	s, _, err := Load(stateDir)
	return s, err
}

// Load reads the file and also returns every field it holds as written, known to this build or not, which
// is what a writer puts back so that an older daemon does not drop what a newer one stored.
func Load(stateDir string) (State, map[string]json.RawMessage, error) {
	path := PathIn(stateDir)
	// A file, asked before it is opened: a pipe or a device there would hang whoever reads it, and every
	// client reads this to find its config.
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{Version: Version}, map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return State{}, nil, err
	}
	if !info.Mode().IsRegular() {
		return State{}, nil, fmt.Errorf("%s is not a state file Norite wrote: it is not a regular file", path)
	}
	f, err := os.Open(path) //nolint:gosec // a fixed name in the user's own state directory
	if err != nil {
		return State{}, nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxSize+1))
	if err != nil {
		return State{}, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(data) > MaxSize {
		return State{}, nil, fmt.Errorf("%s is larger than %d bytes, which no state file Norite writes is", path, MaxSize)
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return State{}, nil, fmt.Errorf("%s is not a state file Norite wrote: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, nil, fmt.Errorf("%s is not a state file Norite wrote: %w", path, err)
	}
	if s.Version > Version {
		return State{}, nil, fmt.Errorf("%s %w (format %d, and this reads up to %d)", path, ErrNewer, s.Version, Version)
	}
	if s.Version < 1 {
		return State{}, nil, fmt.Errorf("%s is not a state file Norite wrote: it has no version", path)
	}
	return s, raw, nil
}
