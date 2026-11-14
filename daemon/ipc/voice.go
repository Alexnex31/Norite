// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ipc

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
)

// The requests about voice calls (M28; M34 adds mute, deafen and the rest).
//
// Trust tier (rule 16): the attach socket's, and no other. The port for scripts refuses every path under
// LocalPathPrefix, so a script cannot put the account in a call.
//
// The join's path carries a channel id, which is the user's: a daemon older than these requests would relay
// the path to its instance (the ledger's entry on the prefix). So a command asks PathVoice first, which
// carries nothing, and sends the id only to a daemon that answered it, as M22's enable does.
const (
	// PathVoice is GET, and answers a VoiceStatus.
	PathVoice = LocalPathPrefix + "voice"
	// PathVoiceLeave is POST: end the call, if there is one. It answers a VoiceStatus.
	PathVoiceLeave = PathVoice + "/leave"
	// pathVoiceJoin begins PathVoiceJoin.
	pathVoiceJoin = PathVoice + "/join/"
)

// PathVoiceJoin is POST: join a voice channel, ending any call already joined. It answers a VoiceStatus
// once the call is up, or a refusal saying why it could not be.
func PathVoiceJoin(channel string) string { return pathVoiceJoin + channel }

// VoiceJoinChannel reads the channel out of a PathVoiceJoin path, and reports whether it is one: the id is
// digits, as every id the CLI puts in a path is.
func VoiceJoinChannel(path string) (string, bool) {
	ch, ok := strings.CutPrefix(path, pathVoiceJoin)
	if !ok || ch == "" || len(ch) > 20 {
		return "", false
	}
	if _, err := strconv.ParseUint(ch, 10, 64); err != nil {
		return "", false
	}
	return ch, true
}

// VoiceJoin is the body of a PathVoiceJoin request (M30): which of the DSP's components process the
// microphone, as the client's config.toml says. A join with no body has every component on.
//
// It travels only to a daemon that has answered PathVoice, for the reason the channel id does: a daemon
// older than these requests would relay the body to its instance with the path (the ledger's entry on the
// prefix, reopened at M30 for this body). voiceipc.Join carries it on to the worker.
type VoiceJoin struct {
	EchoCancellation bool `json:"echo_cancellation"`
	NoiseSuppression bool `json:"noise_suppression"`
	AutoGain         bool `json:"auto_gain"`
}

// VoiceAllOn is every component on: a join with no body, and what a client with no settings sends.
func VoiceAllOn() VoiceJoin {
	return VoiceJoin{EchoCancellation: true, NoiseSuppression: true, AutoGain: true}
}

// DecodeVoiceJoin reads a PathVoiceJoin request's body. No body, or null, is every component on. Anything
// else is an object naming the three switches exactly, each once, each a boolean, and nothing after it: a
// switch left out would otherwise read as off, which is not what a client that forgot it meant.
//
// It walks the tokens rather than decoding into a struct, because encoding/json matches a key in any case,
// keeps the last of two with one name, and stops before a stray ] or } after the object, and the contract's
// VoiceJoin refuses all three (M30 /code-review).
func DecodeVoiceJoin(body json.RawMessage) (VoiceJoin, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return VoiceAllOn(), nil
	}
	bad := errors.New("the join's settings must be an object naming echo_cancellation, noise_suppression and " +
		"auto_gain, each once and true or false, and nothing else")
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return VoiceJoin{}, bad
	}
	var out VoiceJoin
	fields := map[string]*bool{"echo_cancellation": &out.EchoCancellation,
		"noise_suppression": &out.NoiseSuppression, "auto_gain": &out.AutoGain}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		key, isKey := tok.(string)
		field, known := fields[key]
		if err != nil || !isKey || !known || seen[key] {
			return VoiceJoin{}, bad
		}
		seen[key] = true
		tok, err = dec.Token()
		value, isBool := tok.(bool)
		if err != nil || !isBool {
			return VoiceJoin{}, bad
		}
		*field = value
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') || len(seen) != len(fields) {
		return VoiceJoin{}, bad
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return VoiceJoin{}, bad
	}
	return out, nil
}

// EventVoice tells a watching client that what PathVoice answers has changed: a join began, a call came up,
// or one ended. It carries an empty object, and the client asks, as for EventUpdate.
const EventVoice = LocalEventPrefix + "VOICE"

// The call's states.
const (
	// VoiceIdle is no call. Reason says why the last one ended, or is "".
	VoiceIdle = "idle"
	// VoiceJoining is a join under way: the worker started, the offer on its way to the voice server.
	VoiceJoining = "joining"
	// VoiceInCall is a call up: connected to the voice server, which has said ready.
	VoiceInCall = "in_call"
)

// VoiceStatus is the answer to every request about voice.
type VoiceStatus struct {
	// State is VoiceIdle, VoiceJoining or VoiceInCall, and Channel the call's channel id while there is one.
	State   string `json:"state"`
	Channel string `json:"channel"`
	// Reason says why the last call ended, or why a join was refused, for a person; "" when nothing has.
	Reason string `json:"reason"`
	// Available is whether this daemon can join a call at all, and Unavailable says why not when it
	// cannot. Before M33 only a build for M26's harness can.
	Available   bool   `json:"available"`
	Unavailable string `json:"unavailable"`
	// Processing is the call's audio processing as its join asked (M30), while there is a call; nil when
	// there is none.
	Processing *VoiceJoin `json:"processing"`
	// Microphone and Speakers are what the call's devices are doing (M32), as its worker last said; nil
	// before it has said. They stay after a call that ended by itself, so a client can say why it ended,
	// and go with a leave or the next join.
	Microphone *VoiceDevice `json:"microphone"`
	Speakers   *VoiceDevice `json:"speakers"`
}

// VoiceDevice is one of a call's devices: a VoiceDevice* state, and the worker's reason, cleaned, or "".
// A client words the state for its OS, a refusal's setting included (ADR 0034 §1).
type VoiceDevice struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
}

// The states of a call's device, voiceipc's words, which this package cannot import: a test in
// daemon/internal/voiceworker holds the two lists equal.
const (
	// VoiceDeviceOpening is being opened, or opened and not yet delivering.
	VoiceDeviceOpening = "opening"
	// VoiceDeviceOpen is delivering audio.
	VoiceDeviceOpen = "open"
	// VoiceDeviceAsking is the OS asking the person whether norite-daemon may use it (macOS).
	VoiceDeviceAsking = "asking"
	// VoiceDeviceRefused is the OS or the person refusing it.
	VoiceDeviceRefused = "refused"
	// VoiceDeviceNoDevice is no device to use.
	VoiceDeviceNoDevice = "no_device"
	// VoiceDeviceNoSession is nobody logged in at the machine, so nothing may use a device (Linux).
	VoiceDeviceNoSession = "no_session"
	// VoiceDeviceFailed is anything else, its reason saying what.
	VoiceDeviceFailed = "failed"
)
