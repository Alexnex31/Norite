// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonproc

import (
	"fmt"
	"os"
	"testing"
)

// Every daemon Run starts watches the user's config directory, creating it when it is missing (M21). Most
// tests here start one and none of them is about that, so the directory is pointed somewhere empty before
// any test runs: without this each run made ~/.config/norite in the home of whoever ran the suite, and
// put a watch on it (M21 /code-review). XDG_CONFIG_HOME decides it on Linux and macOS, APPDATA on Windows.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "norite-daemonproc-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, name := range []string{"XDG_CONFIG_HOME", "APPDATA"} {
		_ = os.Setenv(name, dir)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
