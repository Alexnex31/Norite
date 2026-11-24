// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceipc

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

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
		{"ended by its devices", OpState, State{State: StateEnded, Reason: "r", Devices: &Devices{
			Microphone: Device{State: DeviceNoSession}, Speakers: Device{State: DeviceNoDevice}}}, true},
		{"ended, its connection lost", OpState, State{State: StateEnded, Reason: "r", Lost: true}, true},
		{"devices", OpDevices, Devices{Microphone: Device{State: DeviceRefused},
			Speakers: Device{State: DeviceFailed, Reason: strings.Repeat("r", MaxReason)}}, true},
		{"speaking", OpSpeaking, Speaking{Users: []string{"1", "18446744073709551615"}}, true},
		{"nobody speaking", OpSpeaking, Speaking{Users: []string{}}, true},
		{"the DSP starting", OpDSP, DSP{State: DSPStarting}, true},
		{"the DSP running", OpDSP, DSP{State: DSPRunning, EchoCancellation: true, AutoGain: true}, true},
		{"the DSP failed", OpDSP, DSP{State: DSPFailed, Reason: strings.Repeat("r", MaxReason)}, true},
		{"join", OpJoin, Join{Loopback: true, Input: Input{Mode: InputVoice}}, false},
		{"join by push-to-talk", OpJoin, Join{Input: Input{Mode: InputPushToTalk}}, false},
		{"input", OpInput, Input{Mode: InputPushToTalk, Held: true}, false},
		{"input by voice", OpInput, Input{Mode: InputVoice}, false},
		{"subscribe", OpSubscribe, Subscribe{Drop: []string{"42"}, Cap: new(2)}, false},
		{"subscribe to everybody", OpSubscribe, Subscribe{Drop: []string{}}, false},
		{"processing", OpProcessing, Processing{Processing: ipc.VoiceJoin{NoiseSuppression: true}}, false},
		{"answer", OpAnswer, Answer{SDP: "v=0\r\n", ICEServers: []ICEServer{{URLs: []string{
			"turn:192.0.2.1:3478?transport=tcp", "turns:voice.example:5349"}, Username: "1700000000:42",
			Credential: "c2VjcmV0"}}, Bitrate: 128000, Rights: Rights{Send: true, Hear: true}}, false},
		{"answer with no servers", OpAnswer, Answer{SDP: "v=0\r\n", ICEServers: []ICEServer{}}, false},
		{"rights", OpRights, Rights{Send: false, Hear: true}, false},
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
		{"devices that ended a call still up", OpState, State{State: StateReady, Devices: &Devices{
			Microphone: Device{State: DeviceNoSession}, Speakers: Device{State: DeviceNoSession}}}},
		{"devices that ended a call, working", OpState, State{State: StateEnded, Devices: &Devices{
			Microphone: Device{State: DeviceOpen}, Speakers: Device{State: DeviceNoSession}}}},
		{"devices that ended a call, saying it goes on", OpState, State{State: StateEnded, Devices: &Devices{
			Microphone: Device{State: DeviceNoSession}, Speakers: Device{State: DeviceNoSession}, Decided: true}}},
		{"a connection lost on a call still up", OpState, State{State: StateReady, Lost: true}},
		{"a connection lost on a call its devices ended", OpState, State{State: StateEnded, Lost: true,
			Devices: &Devices{Microphone: Device{State: DeviceNoSession}, Speakers: Device{State: DeviceNoSession}}}},
		{"a microphone in an unknown state", OpDevices, Devices{Microphone: Device{State: "deaf"},
			Speakers: Device{State: DeviceOpen}}},
		{"speakers in an unknown state", OpDevices, Devices{Microphone: Device{State: DeviceOpen}}},
		{"a microphone's reason past the bound", OpDevices, Devices{Speakers: Device{State: DeviceOpen},
			Microphone: Device{State: DeviceFailed, Reason: strings.Repeat("r", MaxReason+1)}}},
		{"the speakers' reason past the bound", OpDevices, Devices{Microphone: Device{State: DeviceOpen},
			Speakers: Device{State: DeviceFailed, Reason: strings.Repeat("r", MaxReason+1)}}},
		{"too many speaking", OpSpeaking, Speaking{Users: slices.Repeat([]string{"1"}, MaxSpeaking+1)}},
		{"a speaker that is not an id", OpSpeaking, Speaking{Users: []string{"1x"}}},
		{"a speaker past an id's digits", OpSpeaking, Speaking{Users: []string{strings.Repeat("9", 21)}}},
		{"the DSP in an unknown state", OpDSP, DSP{State: "humming"}},
		{"a stage running in a DSP starting", OpDSP, DSP{State: DSPStarting, NoiseSuppression: true}},
		{"a stage running in a DSP failed", OpDSP, DSP{State: DSPFailed, Reason: "r", EchoCancellation: true}},
		{"a DSP failed saying nothing", OpDSP, DSP{State: DSPFailed}},
		{"a reason for a DSP running", OpDSP, DSP{State: DSPRunning, Reason: "r"}},
		{"a DSP reason past the bound", OpDSP, DSP{State: DSPFailed, Reason: strings.Repeat("r", MaxReason+1)}},
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
	many := Answer{SDP: "v=0\r\n", ICEServers: make([]ICEServer, MaxICEServers+1)}
	for i := range many.ICEServers {
		many.ICEServers[i] = ICEServer{URLs: []string{"turn:192.0.2.1:3478"}}
	}
	long := Answer{SDP: "v=0\r\n", ICEServers: []ICEServer{{URLs: []string{"turn:" + strings.Repeat("a", MaxURL)}}}}
	secret := Answer{SDP: "v=0\r\n", ICEServers: []ICEServer{{URLs: []string{"turn:a"},
		Credential: strings.Repeat("c", MaxCredential+1)}}}
	for name, sub := range map[string]Subscribe{
		"too many dropped":                 {Drop: slices.Repeat([]string{"1"}, MaxDrop+1)},
		"a dropped user that is not an id": {Drop: []string{""}},
		"no slot":                          {Drop: []string{}, Cap: new(0)},
		"more slots than the worker has":   {Drop: []string{}, Cap: new(MaxSlots + 1)},
	} {
		require.Error(t, sub.Check(), name)
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(frame(t, OpSubscribe, sub)))
		require.NoError(t, err)
		require.Error(t, daemon.Validate(doc), name)
	}
	for name, in := range map[string]Input{
		"no mode":                         {},
		"a mode nobody declared":          {Mode: "whisper"},
		"a key held while input by voice": {Mode: InputVoice, Held: true},
	} {
		require.Error(t, in.Check(), name)
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(frame(t, OpInput, in)))
		require.NoError(t, err)
		require.Error(t, daemon.Validate(doc), name)
	}
	for name, a := range map[string]Answer{"too many servers": many, "a URL past its bound": long,
		"a credential past its bound": secret, "no answer": {ICEServers: []ICEServer{}}} {
		require.Error(t, a.Check(), name)
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(frame(t, OpAnswer, a)))
		require.NoError(t, err)
		require.Error(t, daemon.Validate(doc), name)
	}
}

// What the voice server names for a bitrate is clamped to what the SFU carries, and zero is the default.
func TestTheBitrateIsClamped(t *testing.T) {
	require.Equal(t, 64000, ClampBitrate(0, 64000))
	require.Equal(t, MinBitrate, ClampBitrate(8000, 64000))
	require.Equal(t, MaxBitrate, ClampBitrate(510_000, 64000))
	require.Equal(t, 96000, ClampBitrate(96000, 64000))
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
		int(OpDevices): "Devices", int(OpJoin): "Join", int(OpAnswer): "Answer", int(OpLeave): "Leave",
		int(OpRights): "RightsFrame", int(OpSpeaking): "SpeakingFrame", int(OpDSP): "DSPFrame",
		int(OpSubscribe): "SubscribeFrame", int(OpProcessing): "ProcessingFrame", int(OpInput): "InputFrame"}
	require.Equal(t, want, got)
}

// Every state the contract names is one the check accepts, and no other.
func TestTheDeviceStatesAreTheContracts(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "contracts", "voice-worker.schema.json"))
	require.NoError(t, err)
	var doc struct {
		Defs struct {
			Device struct {
				Properties struct {
					State struct {
						Enum []string `json:"enum"`
					} `json:"state"`
				} `json:"properties"`
			} `json:"Device"`
		} `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	states := doc.Defs.Device.Properties.State.Enum
	require.ElementsMatch(t, []string{DeviceOpening, DeviceOpen, DeviceAsking, DeviceRefused, DeviceNoDevice,
		DeviceNoSession, DeviceFailed}, states)
	for _, s := range states {
		require.NoError(t, Devices{Microphone: Device{State: s}, Speakers: Device{State: s}}.Check(), s)
	}
}

// A reason cut to the bound stays within it once encoded, whatever script it is in.
func TestACutReasonStaysWithinTheBound(t *testing.T) {
	for _, r := range []string{"é", "日", "🎙", "a"} {
		cut := CutReason(strings.Repeat(r, MaxReason))
		require.LessOrEqual(t, len(cut), MaxReason)
		require.True(t, utf8.ValidString(cut), "%q", r)
		var buf bytes.Buffer
		require.NoError(t, Write(&buf, OpState, State{State: StateEnded, Reason: cut}))
		f, err := Read(&buf, MaxWorkerFrame)
		require.NoError(t, err)
		var s State
		require.NoError(t, ipc.Decode(f, &s))
		require.NoError(t, s.Check())
	}
}

// The worker clamps to the bounds the gateway's contract states for VOICE_SERVER_UPDATE's bitrate, which
// the backend holds to the channel routes and the coordinator (TestTheBitrateBoundsAgreeAcrossTheContracts):
// one set of bounds, read in three modules that cannot import one another.
func TestTheClampIsTheGatewaysBounds(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "contracts", "gateway-events.schema.json"))
	require.NoError(t, err)
	var gw struct {
		Defs map[string]struct {
			Properties map[string]map[string]any `json:"properties"`
		} `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(raw, &gw))
	b := gw.Defs["VoiceServer"].Properties["bitrate"]
	require.NotNil(t, b, "the gateway's contract names no VoiceServer bitrate")
	require.EqualValues(t, MinBitrate, b["minimum"])
	require.EqualValues(t, MaxBitrate, b["maximum"])
}

// The longest TURN URL an instance builds, from a host name of DNS's 253 bytes, is within the bound: one
// past it failed every join on that instance (M33's /code-review).
func TestTheLongestHostNamesTURNURLIsAdmitted(t *testing.T) {
	host := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." +
		strings.Repeat("d", 61)
	require.Len(t, host, 253)
	a := Answer{SDP: "v=0\r\n", ICEServers: []ICEServer{{URLs: []string{
		"turns:" + host + ":65535?transport=tcp", "turn:" + host + ":65535?transport=udp"}}}}
	require.NoError(t, a.Check())
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(frame(t, OpAnswer, a)))
	require.NoError(t, err)
	require.NoError(t, compile(t, "DaemonFrame").Validate(doc), "the contract refuses what the check admits")
}

// Every state the contract names for the DSP is one the check accepts, and no other.
func TestTheDSPStatesAreTheContracts(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "contracts", "voice-worker.schema.json"))
	require.NoError(t, err)
	var doc struct {
		Defs struct {
			DSP struct {
				Properties struct {
					State struct {
						Enum []string `json:"enum"`
					} `json:"state"`
				} `json:"properties"`
			} `json:"DSP"`
		} `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.ElementsMatch(t, []string{DSPStarting, DSPRunning, DSPFailed}, doc.Defs.DSP.Properties.State.Enum)
	require.NoError(t, DSP{State: DSPStarting}.Check())
	require.NoError(t, DSP{State: DSPRunning}.Check())
	require.NoError(t, DSP{State: DSPFailed, Reason: "r"}.Check())
}
