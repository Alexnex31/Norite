package instanceadmin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Reading contracts/cli-json/instance-invite.schema.json, so the --json tests check the code against the
// contract rather than against themselves.
//
// A test that hardcodes the expected field list is a second copy of the schema, and the two drift in the
// direction the code went — which is exactly the drift rule 15 exists to prevent. Parsing the real file
// means renaming a field in the code without touching the contract fails here, which is the point.

// inviteSchema is the slice of the schema these tests read.
type inviteSchema struct {
	Defs struct {
		Invite struct {
			Required   []string `json:"required"`
			Properties struct {
				Code struct {
					Pattern string `json:"pattern"`
				} `json:"code"`
			} `json:"properties"`
		} `json:"invite"`
	} `json:"$defs"`
}

func loadInviteSchema(t *testing.T) inviteSchema {
	t.Helper()

	path := filepath.Join("..", "..", "..", "contracts", "cli-json", "instance-invite.schema.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "the CLI JSON contract must be readable from the cli module")

	var schema inviteSchema
	require.NoError(t, json.Unmarshal(raw, &schema), "the schema must be valid JSON")
	require.NotEmpty(t, schema.Defs.Invite.Required, "the schema declares no required fields")
	return schema
}

func requiredInviteKeys(t *testing.T) []string {
	t.Helper()
	return loadInviteSchema(t).Defs.Invite.Required
}

func schemaCodePattern(t *testing.T) string {
	t.Helper()
	pattern := loadInviteSchema(t).Defs.Invite.Properties.Code.Pattern
	require.NotEmpty(t, pattern, "the schema must constrain what an invite code looks like")
	return pattern
}
