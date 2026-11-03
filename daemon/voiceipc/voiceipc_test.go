// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceipc

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/backend/voiceproto"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

const schemaID = "https://norite.example/contracts/voice-worker.schema.json"

// Schema compiles contracts/voice-worker.schema.json, rooted at the definition named, for the tests here
// and the worker's: dir is the repository's root relative to the caller.
func compile(t *testing.T, def string) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "contracts", "voice-worker.schema.json"))
	require.NoError(t, err)
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	require.NoError(t, err)
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	require.NoError(t, c.AddResource(schemaID, doc))
	s, err := c.Compile(schemaID + "#/$defs/" + def)
	require.NoError(t, err)
	return s
}

// frame writes v as op through Write, reads it back through Read, and returns the frame's JSON.
func frame(t *testing.T, op gatewayproto.Opcode, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, Write(&buf, op, v))
	f, err := Read(&buf, MaxWorkerFrame)
	require.NoError(t, err)
	raw, err := ipc.Marshal(f)
	require.NoError(t, err)
	return raw
}

// Every frame either end sends is the contract's, and is the contract's only on the side that sends it: a
// frame the worker sends is no daemon frame, and the other way round.
func TestEveryFrameIsTheContracts(t *testing.T) {
	worker, daemon := compile(t, "WorkerFrame"), compile(t, "DaemonFrame")
	offer := strings.Repeat("a", voiceproto.MaxOffer)
	cases := []struct {
		name     string
		op       gatewayproto.Opcode
		v        any
		byWorker bool
	}{
		{"hello", OpHello, Hello{Version: "0.4.0"}, true},
		{"offer", OpOffer, Offer{SDP: offer}, true},
		{"connecting", OpState, State{State: StateConnecting}, true},
		{"ready", OpState, State{State: StateReady}, true},
		{"ended", OpState, State{State: StateEnded, Reason: strings.Repeat("r", MaxReason)}, true},
		{"join", OpJoin, Join{ICEServers: []ICEServer{{URLs: []string{"turn:192.0.2.1:3478?transport=tcp",
			"turns:voice.example:5349"}, Username: "1700000000:42", Credential: "c2VjcmV0"}}, Loopback: true}, false},
		{"join with no servers", OpJoin, Join{ICEServers: []ICEServer{}}, false},
		{"answer", OpAnswer, Answer{SDP: "v=0\r\n"}, false},
		{"leave", OpLeave, Leave{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(frame(t, c.op, c.v)))
			require.NoError(t, err)
			mine, theirs := daemon, worker
			if c.byWorker {
				mine, theirs = worker, daemon
			}
			require.NoError(t, mine.Validate(doc))
			require.Error(t, theirs.Validate(doc), "a frame valid from both ends could be mistaken for the other's")
		})
	}
}

// The contract refuses what the daemon's checks refuse, so the two cannot drift into one accepting what
// the other was written to stop.
func TestTheContractAndTheChecksRefuseTheSameThings(t *testing.T) {
	worker := compile(t, "WorkerFrame")
	cases := []struct {
		name string
		op   gatewayproto.Opcode
		v    interface{ Check() error }
	}{
		{"an empty offer", OpOffer, Offer{}},
		{"an offer past the bound", OpOffer, Offer{SDP: strings.Repeat("a", voiceproto.MaxOffer+1)}},
		{"an unknown state", OpState, State{State: "dancing"}},
		{"a reason past the bound", OpState, State{State: StateEnded, Reason: strings.Repeat("r", MaxReason+1)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Error(t, c.v.Check())
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(frame(t, c.op, c.v)))
			require.NoError(t, err)
			require.Error(t, worker.Validate(doc))
		})
	}

	daemon := compile(t, "DaemonFrame")
	many := Join{ICEServers: make([]ICEServer, MaxICEServers+1)}
	for i := range many.ICEServers {
		many.ICEServers[i] = ICEServer{URLs: []string{"turn:192.0.2.1:3478"}}
	}
	require.Error(t, many.Check())
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(frame(t, OpJoin, many)))
	require.NoError(t, err)
	require.Error(t, daemon.Validate(doc))
}

// A frame past the bound is refused before its payload is read, and a payload naming a field the type
// does not declare is refused by the strict decoder.
func TestReadingIsBoundedAndDecodingStrict(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, Write(&buf, OpOffer, Offer{SDP: strings.Repeat("a", MaxWorkerFrame)}))
	_, err := Read(&buf, MaxWorkerFrame)
	require.ErrorIs(t, err, ipc.ErrFrameTooLarge)

	buf.Reset()
	require.NoError(t, Write(&buf, OpHello, map[string]any{"version": "dev", "token": "x"}))
	f, err := Read(&buf, MaxWorkerFrame)
	require.NoError(t, err)
	var h Hello
	require.Error(t, ipc.Decode(f, &h))

	_, err = Read(&buf, MaxWorkerFrame)
	require.ErrorIs(t, err, io.EOF, "a pipe closed between frames is the ordinary end")
}

func TestPipesRoundTrip(t *testing.T) {
	r, w, err := ParsePipes(FormatPipes(3, 4))
	require.NoError(t, err)
	require.Equal(t, [2]uintptr{3, 4}, [2]uintptr{r, w})
	for _, bad := range []string{"", "3", "3,3", "a,4", "3,-1", "3,4,5"} {
		_, _, err := ParsePipes(bad)
		require.Error(t, err, "%q", bad)
	}
}

// The ops in the contract are the ops here, each once.
func TestTheOpsAreTheContracts(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "contracts", "voice-worker.schema.json"))
	require.NoError(t, err)
	var doc struct {
		Defs map[string]struct {
			Properties struct {
				Op struct {
					Const *int `json:"const"`
				} `json:"op"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	got := map[int]string{}
	for name, d := range doc.Defs {
		if d.Properties.Op.Const != nil {
			got[*d.Properties.Op.Const] = name
		}
	}
	want := map[int]string{int(OpHello): "Hello", int(OpOffer): "Offer", int(OpState): "State",
		int(OpJoin): "Join", int(OpAnswer): "Answer", int(OpLeave): "Leave"}
	require.Equal(t, want, got)
}
