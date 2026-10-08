// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"fmt"
	"os"
	"testing"
)

// A client given no config reader reads its user's config.toml, and most tests here give none. Every root
// the config's path is derived from, on all three platforms, is pointed at an empty throwaway directory
// before any test runs, so a test that forgets cannot read the settings of whoever runs it — and cannot
// pass on one machine and fail on another because of them.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "norite-tui-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "HOME", "APPDATA", "LOCALAPPDATA", "USERPROFILE"} {
		_ = os.Setenv(name, dir)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
