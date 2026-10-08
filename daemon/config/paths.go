// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/Alexnex31/Norite/daemon/statefile"
)

const (
	fileName       = "config.toml"
	xdgDirName     = "norite"
	windowsDirName = "Norite"
)

// Dir returns the directory config.toml lives in, without creating it.
func Dir() (string, error) { return dirFor(runtime.GOOS) }

// Path returns where config.toml is for the current user.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fileName), nil
}

// The two files the clients read instead of config.toml while the same-machine toggle is on.
const (
	tuiFileName = "config.tui.toml"
	guiFileName = "config.gui.toml"
)

// BackupSuffix is added to a file's name for the copy that turning the toggle off keeps of it.
const BackupSuffix = ".before-unsplit"

// Files names the three config files in one directory.
type Files struct {
	// Shared is config.toml, which both clients read while the toggle is off.
	Shared string
	// TUI and GUI are the files each client reads while it is on.
	TUI, GUI string
}

// FilesIn returns the config files' paths in dir.
func FilesIn(dir string) Files {
	return Files{
		Shared: filepath.Join(dir, fileName),
		TUI:    filepath.Join(dir, tuiFileName),
		GUI:    filepath.Join(dir, guiFileName),
	}
}

// SplitPath returns the file a client reads while the same-machine toggle is on: config.tui.toml or
// config.gui.toml, beside config.toml. Only the two clients have one.
func SplitPath(client Section) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	switch client {
	case TUI:
		return filepath.Join(dir, tuiFileName), nil
	case GUI:
		return filepath.Join(dir, guiFileName), nil
	}
	return "", fmt.Errorf("[%s] has no config file of its own: the clients are tui and gui", client)
}

// PathFor returns the config file a client reads right now, and whether that is its own because the
// toggle is on. The toggle is the daemon's to change and anybody's to read, so this works with no daemon
// running.
func PathFor(client Section) (path string, split bool, err error) {
	state, err := statefile.Read()
	if err != nil {
		return "", false, err
	}
	if !state.ConfigSplit {
		path, err = Path()
		return path, false, err
	}
	path, err = SplitPath(client)
	return path, true, err
}

// dirFor resolves the directory for a named GOOS, so all three platforms' rules are tested from any one.
func dirFor(goos string) (string, error) {
	if goos == "windows" {
		// APPDATA, the roaming half, where the state directory uses LOCALAPPDATA: preferences are the
		// thing a roaming profile exists to carry, and a lock file is the thing it must not.
		if dir := os.Getenv("APPDATA"); dir != "" {
			return filepath.Join(dir, windowsDirName), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locating the user's home directory: %w", err)
		}
		return filepath.Join(home, "AppData", "Roaming", windowsDirName), nil
	}

	// Linux and macOS alike. macOS has ~/Library/Application Support, and the state directory uses it,
	// but ~/.config is where somebody who lives in a terminal keeps configuration on a Mac too, and where
	// every document says this file is. Only an absolute XDG_CONFIG_HOME is honored, per the spec.
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, xdgDirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating the user's home directory: %w", err)
	}
	return filepath.Join(home, ".config", xdgDirName), nil
}
