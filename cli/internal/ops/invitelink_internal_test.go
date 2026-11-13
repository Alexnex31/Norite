// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/Alexnex31/Norite/cli/internal/daemontest"
)

// TestTheCodeShapeIsTheContracts: the shape IssuedCode checks is written here and in the backend, which
// may not import each other, so both are held to the contract instead: openapi.yaml's GuildInvite.code and
// GuildInvitePreview.code, and the CLI's own guild-invite.schema.json. The backend's half is
// TestAnInviteCodeIsWhatTheContractSays in cmd/server.
func TestTheCodeShapeIsTheContracts(t *testing.T) {
	want := "^[" + inviteCodeAlphabet + "]{" + strconv.Itoa(inviteCodeLength) + "}$"

	raw, err := os.ReadFile(filepath.Join(daemontest.Contracts(), "openapi.yaml"))
	require.NoError(t, err)
	var api struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Pattern string `yaml:"pattern"`
				} `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &api))
	for _, schema := range []string{"GuildInvite", "GuildInvitePreview"} {
		assert.Equal(t, want, api.Components.Schemas[schema].Properties["code"].Pattern, schema)
	}

	raw, err = os.ReadFile(filepath.Join(daemontest.Contracts(), "cli-json", "guild-invite.schema.json"))
	require.NoError(t, err)
	var cliJSON struct {
		Defs map[string]struct {
			Properties map[string]struct {
				Pattern string `json:"pattern"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(raw, &cliJSON))
	for _, def := range []string{"invite", "preview"} {
		assert.Equal(t, want, cliJSON.Defs[def].Properties["code"].Pattern, def)
	}
	assert.Equal(t, "^https?://[^/?#]+(/[^?#]*)?/join#"+want[1:], cliJSON.Defs["invite"].Properties["link"].Pattern)
}
