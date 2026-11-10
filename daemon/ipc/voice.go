// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ipc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
// prefix, reopened at M30 for this body). The daemon module's voiceipc.Processing has the same fields.
type VoiceJoin struct {
	EchoCancellation bool `json:"echo_cancellation"`
	NoiseSuppression bool `json:"noise_suppression"`
	AutoGain         bool `json:"auto_gain"`
}

// DecodeVoiceJoin reads a PathVoiceJoin request's body. No body, or null, is every component on. Anything
// else must name all three switches and nothing more: a switch left out would otherwise read as off, which
// is not what a client that forgot it meant.
func DecodeVoiceJoin(body json.RawMessage) (VoiceJoin, error) {
	if len(bytes.TrimSpace(body)) == 0 || bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		return VoiceJoin{EchoCancellation: true, NoiseSuppression: true, AutoGain: true}, nil
	}
	var in struct {
		EchoCancellation *bool `json:"echo_cancellation"`
		NoiseSuppression *bool `json:"noise_suppression"`
		AutoGain         *bool `json:"auto_gain"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return VoiceJoin{}, fmt.Errorf("the join's settings do not decode: %w", err)
	}
	if dec.More() {
		return VoiceJoin{}, errors.New("the join's settings are followed by something else")
	}
	if in.EchoCancellation == nil || in.NoiseSuppression == nil || in.AutoGain == nil {
		return VoiceJoin{}, errors.New("the join's settings must name echo_cancellation, noise_suppression and auto_gain")
	}
	return VoiceJoin{EchoCancellation: *in.EchoCancellation, NoiseSuppression: *in.NoiseSuppression,
		AutoGain: *in.AutoGain}, nil
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
}
