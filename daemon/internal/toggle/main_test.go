// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package toggle

import (
	"fmt"
	"os"
	"testing"
)

// An unsplit writes config.toml through daemon/config, whose lock is in the user's state directory. Every
// root that directory and the config's are derived from is pointed at a throwaway one.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "norite-toggle-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, name := range []string{"XDG_STATE_HOME", "XDG_CONFIG_HOME", "HOME", "APPDATA", "LOCALAPPDATA", "USERPROFILE"} {
		_ = os.Setenv(name, dir)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
