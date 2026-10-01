// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package gatewayproto

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseVersionIsStrict(t *testing.T) {
	for _, ok := range []string{"0.1.0", "v0.1.0-alpha", "0.3.0-beta.2", "1.0.0-rc.1", "0.12.0", "1.2.3+build.5"} {
		_, err := ParseVersion(ok)
		assert.NoError(t, err, ok)
	}
	// ADR 0033: always three components, and nothing SemVer itself would refuse.
	for _, bad := range []string{"0.1-alpha", "v1", "1.2", "01.0.0", "0.1.0-", "dev", "", "m20a", "1.2.3.4"} {
		_, err := ParseVersion(bad)
		assert.Error(t, err, bad)
	}

	v, err := ParseVersion("v0.2.0-beta.1+sha.abc")
	require.NoError(t, err)
	assert.Equal(t, Version{Major: 0, Minor: 2, Patch: 0, Pre: "beta.1"}, v, "build metadata is dropped")
}

// Every row of ADR 0033's handshake rule, as settled with the maintainer on 2026-09-30: strict, and "dev"
// compatible with anything.
func TestCheckAppliesTheStrictRule(t *testing.T) {
	cases := []struct {
		server, client string
		compatible     bool
		checked        bool
	}{
		// 0.x: MAJOR.MINOR must match, PATCH may differ.
		{"0.2.0", "0.2.0", true, true},
		{"0.2.1", "0.2.3", true, true},
		{"0.2.0", "0.3.0", false, true},
		{"0.3.0", "0.2.9", false, true},

		// Pre-releases only talk to the identical version.
		{"0.2.0-alpha.1", "0.2.0-alpha.1", true, true},
		{"0.2.0-alpha.1", "0.2.0-alpha.2", false, true},
		{"0.2.0-beta.1", "0.2.0", false, true},
		{"0.2.0", "0.2.0-rc.1", false, true},
		{"1.0.0-rc.1", "1.0.0-rc.1+build", true, true},

		// 1.x: MAJOR must match, the client may be up to MinorWindow MINORs behind, never ahead.
		{"1.4.0", "1.4.7", true, true},
		{"1.4.0", "1.2.0", true, true},
		{"1.4.0", "1.1.0", false, true},
		{"1.4.0", "1.5.0", false, true},
		{"2.0.0", "1.9.0", false, true},
		{"1.0.0", "0.12.0", false, true},

		// A dev build on either side is compatible and unchecked.
		{"dev", "0.2.0", true, false},
		{"0.2.0", "dev", true, false},

		// A garbled version is not "dev": it makes no promise at all. Nor is a missing one, or a client
		// leaving it out would skip the check against every release.
		{"0.2.0", "", false, true},
		{"", "0.2.0", false, true},
		{"0.2.0", "0.2", false, true},
		{"0.2.0", "latest", false, true},
	}
	for _, tc := range cases {
		got := Check(tc.server, tc.client)
		assert.Equal(t, tc.compatible, got.Compatible, "server %q, client %q: %s", tc.server, tc.client, got.Reason)
		assert.Equal(t, tc.checked, got.Checked, "server %q, client %q", tc.server, tc.client)
		if !got.Compatible {
			assert.NotEmpty(t, got.Reason, "a refusal must say why")
		}
	}
}

func TestAMismatchNamesTheSideToUpgrade(t *testing.T) {
	assert.Contains(t, Check("0.2.0", "0.3.0").Reason, "upgrade the server")
	assert.Contains(t, Check("0.3.0", "0.2.0").Reason, "upgrade the client")
	assert.Contains(t, Check("1.5.0", "1.1.0").Reason, "upgrade the client")
}
