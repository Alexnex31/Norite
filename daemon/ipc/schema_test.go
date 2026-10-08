// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ipc

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"
)

const (
	ipcSchemaID     = "https://norite.example/contracts/daemon-ipc.schema.json"
	gatewaySchemaID = "https://norite.example/contracts/gateway-events.schema.json"
)

var (
	schemaOnce                 sync.Once
	daemonSchema, clientSchema *jsonschema.Schema
	schemaErr                  error
)

func contractPath(name string) string {
	return filepath.Join("..", "..", "contracts", name)
}

func loadDoc(name string) (any, error) {
	raw, err := os.ReadFile(contractPath(name))
	if err != nil {
		return nil, err
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(raw))
}

// schemas compiles the attach socket's contract, with the gateway's beside it for the frames they share.
func schemas(t *testing.T) (daemon, client *jsonschema.Schema) {
	t.Helper()
	schemaOnce.Do(func() {
		ipcDoc, err := loadDoc("daemon-ipc.schema.json")
		if err != nil {
			schemaErr = err
			return
		}
		gwDoc, err := loadDoc("gateway-events.schema.json")
		if err != nil {
			schemaErr = err
			return
		}
		c := jsonschema.NewCompiler()
		c.AssertFormat()
		if schemaErr = c.AddResource(ipcSchemaID, ipcDoc); schemaErr != nil {
			return
		}
		if schemaErr = c.AddResource(gatewaySchemaID, gwDoc); schemaErr != nil {
			return
		}
		if daemonSchema, schemaErr = c.Compile(ipcSchemaID + "#/$defs/DaemonFrame"); schemaErr != nil {
			return
		}
		clientSchema, schemaErr = c.Compile(ipcSchemaID + "#/$defs/ClientFrame")
	})
	require.NoError(t, schemaErr)
	return daemonSchema, clientSchema
}

// conforms reports whether data matches s, failing the test with the reason when it does not. It does not
// stop the test: the fakes call it from their own goroutines, where FailNow would leave the other end
// waiting for a frame that never comes.
func conforms(t *testing.T, s *jsonschema.Schema, data []byte, what string) bool {
	t.Helper()
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Errorf("%s is not JSON: %v", what, err)
		return false
	}
	if err := s.Validate(inst); err != nil {
		t.Errorf("%s does not match the contract: %s\n%v", what, data, err)
		return false
	}
	return true
}

// TestEveryGatewayDispatchButTheDaemonsOwnIsForwarded holds the socket's contract to the gateway's: a new
// dispatch type the gateway gains must be declared forwardable here too, or a client validating the socket
// against its contract would refuse the first one it sees. READY and RESUMED are the daemon's own.
func TestEveryGatewayDispatchButTheDaemonsOwnIsForwarded(t *testing.T) {
	refs := func(name, def string) []string {
		raw, err := os.ReadFile(contractPath(name))
		require.NoError(t, err)
		var doc struct {
			Defs map[string]struct {
				OneOf []struct {
					Ref string `json:"$ref"`
				} `json:"oneOf"`
			} `json:"$defs"`
		}
		require.NoError(t, json.Unmarshal(raw, &doc))
		var out []string
		for _, r := range doc.Defs[def].OneOf {
			out = append(out, r.Ref[strings.LastIndex(r.Ref, "/")+1:])
		}
		sort.Strings(out)
		return out
	}

	var want []string
	for _, name := range refs("gateway-events.schema.json", "Dispatch") {
		if name != "ReadyDispatch" && name != "ResumedDispatch" {
			want = append(want, name)
		}
	}
	require.NotEmpty(t, want)
	require.Equal(t, want, refs("daemon-ipc.schema.json", "ForwardedDispatch"))
}

// A local dispatch is told from a forwarded one by its name alone, so the gateway must never use the
// prefix, and the schema's list of local types must be the ones this package defines.
func TestLocalDispatchTypesAreTheirOwnNamespace(t *testing.T) {
	raw, err := os.ReadFile(contractPath("gateway-events.schema.json"))
	require.NoError(t, err)
	names := regexp.MustCompile(`"(?:const|enum)":\s*\[?\s*"([A-Z][A-Z0-9_]+)"`).FindAllSubmatch(raw, -1)
	require.Greater(t, len(names), 10, "the pattern must be finding the gateway's dispatch names, or this checks nothing")
	for _, m := range names {
		require.False(t, strings.HasPrefix(string(m[1]), LocalEventPrefix),
			"the gateway schema names %s, which is the daemon's own namespace", m[1])
	}

	raw, err = os.ReadFile(contractPath("daemon-ipc.schema.json"))
	require.NoError(t, err)
	var doc struct {
		Defs struct {
			LocalDispatch struct {
				Properties struct {
					T struct {
						Enum []string `json:"enum"`
					} `json:"t"`
				} `json:"properties"`
			} `json:"LocalDispatch"`
		} `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Equal(t, []string{EventConfigUpdate}, doc.Defs.LocalDispatch.Properties.T.Enum)
	for _, name := range doc.Defs.LocalDispatch.Properties.T.Enum {
		require.True(t, strings.HasPrefix(name, LocalEventPrefix))
	}
}

// TestTheSchemaRefusesWhatTheProtocolForbids checks the contract can see the two properties that matter
// most, rather than trusting that it validates: a token in IDENTIFY, and a response carrying both a status
// and an error.
func TestTheSchemaRefusesWhatTheProtocolForbids(t *testing.T) {
	daemon, client := schemas(t)

	refuse := func(s *jsonschema.Schema, frame string) {
		t.Helper()
		inst, err := jsonschema.UnmarshalJSON(strings.NewReader(frame))
		require.NoError(t, err)
		require.Error(t, s.Validate(inst), "the contract accepted %s", frame)
	}

	refuse(client, `{"op":2,"d":{"token":"eyJ","properties":{"os":"linux","client":"norite","version":"dev"},`+
		`"events":false},"s":null,"t":null}`)
	refuse(daemon, `{"op":101,"d":{"id":"1","status":200,"body":null,`+
		`"error":{"code":"refused","message":"no"}},"s":null,"t":null}`)
	refuse(daemon, `{"op":101,"d":{"id":"1","status":null,"body":null,"error":null},"s":null,"t":null}`)
	refuse(daemon, `{"op":0,"d":{"session_id":"x","user":{},"guilds":[]},"s":1,"t":"RESUMED"}`)
	refuse(client, `{"op":100,"d":{"id":"1","method":"TRACE","path":"/guilds","body":null},"s":null,"t":null}`)
}
