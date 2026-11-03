// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ipc

import (
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
}
