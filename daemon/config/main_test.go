// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"fmt"
	"os"
	"testing"
)

// The config's lock lives in the state directory, so every test here would otherwise take locks in the
// real one of whoever runs them. All three platforms' roots are pointed at a throwaway directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "norite-config-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, name := range []string{"XDG_STATE_HOME", "HOME", "LOCALAPPDATA", "USERPROFILE"} {
		_ = os.Setenv(name, dir)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
