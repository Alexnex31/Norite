// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
