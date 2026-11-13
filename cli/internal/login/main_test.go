// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package login

import (
	"os"
	"testing"
)

// TestMain clears the environment's instance and password, which every command here reads: a developer who
// exports them would otherwise run these tests against their own instance, or with their own password, and
// see failures nobody else does (easier-setup's second /code-review). A test that wants one sets it with
// t.Setenv.
func TestMain(m *testing.M) {
	for _, name := range []string{instanceEnvVar, passwordEnvVar} {
		_ = os.Unsetenv(name)
	}
	os.Exit(m.Run())
}
