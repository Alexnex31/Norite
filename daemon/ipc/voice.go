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

	// PathVoiceMute, PathVoiceUnmute, PathVoiceDeafen and PathVoiceUndeafen are POST (M34): what the person
	// sets on themselves, in the call and for every call after it until the sign-in ends. Deafening mutes
	// too, and undeafening puts back the mute from before. Each answers a VoiceStatus, and carries nothing.
	PathVoiceMute     = PathVoice + "/mute"
	PathVoiceUnmute   = PathVoice + "/unmute"
	PathVoiceDeafen   = PathVoice + "/deafen"
	PathVoiceUndeafen = PathVoice + "/undeafen"
	// PathVoiceProcessing is POST (M34): how the person wants the microphone processed, the join's body
	// with every switch named, for the call there is and every join after it until the sign-in ends.
	PathVoiceProcessing = PathVoice + "/processing"

	// pathVoiceLocalMute, pathVoiceLocalUnmute and pathVoiceStreams begin the requests that carry an id or
	// a number, sent only to a daemon that answered PathVoice, as the join's channel is.
	pathVoiceLocalMute   = PathVoice + "/local-mute/"
	pathVoiceLocalUnmute = PathVoice + "/local-unmute/"
	pathVoiceStreams     = PathVoice + "/streams/"
)

// MaxLocalMutes is how many people one account may mute for itself (M34): a full room, which is also the
// most the SFU takes in one subscribe.
const MaxLocalMutes = 64

// MaxStreams is the most people a call is heard from at once: the slots the worker receives on.
const MaxStreams = 3

// PathVoiceLocalMute and PathVoiceLocalUnmute are POST (M34): mute user for this account alone, in every
// call until the sign-in ends, or hear them again. A local mute is never a report: the two are separate
// acts (architecture.md §6). Each answers a VoiceStatus.
func PathVoiceLocalMute(user string) string   { return pathVoiceLocalMute + user }
func PathVoiceLocalUnmute(user string) string { return pathVoiceLocalUnmute + user }

// PathVoiceStreams is POST (M34): hear at most n people at once, 1 to MaxStreams, or every one the call has
// room for with 0. It answers a VoiceStatus.
func PathVoiceStreams(n int) string { return pathVoiceStreams + strconv.Itoa(n) }

// VoiceLocalMuteUser reads the user out of a PathVoiceLocalMute or PathVoiceLocalUnmute path, reports
// whether it is one (IsUserID), and whether the path mutes.
func VoiceLocalMuteUser(path string) (user string, mute, ok bool) {
	if u, cut := strings.CutPrefix(path, pathVoiceLocalMute); cut {
		return u, true, IsUserID(u)
	}
	if u, cut := strings.CutPrefix(path, pathVoiceLocalUnmute); cut {
		return u, false, IsUserID(u)
	}
	return "", false, false
}

// IsUserID is whether s is a user's id as the instance writes one: a snowflake other than zero, in decimal
// with no leading zero. A local mute is kept and compared as that text, so 007 would never be user 7, and 0
// would take a place no user can fill (M34's manual pass).
func IsUserID(s string) bool {
	n, err := strconv.ParseUint(s, 10, 64)
	return err == nil && n != 0 && strconv.FormatUint(n, 10) == s
}

// VoiceStreams reads the number out of a PathVoiceStreams path, and reports whether it is 0 to MaxStreams.
func VoiceStreams(path string) (int, bool) {
	n, cut := strings.CutPrefix(path, pathVoiceStreams)
	if !cut || len(n) != 1 || n[0] < '0' || n[0] > '0'+MaxStreams {
		return 0, false
	}
	return int(n[0] - '0'), true
}

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

// DecodeVoiceProcessing reads a PathVoiceProcessing request's body: the join's object, every switch named.
// Unlike a join's, no body is not every component on, since a change that names nothing changes nothing.
func DecodeVoiceProcessing(body json.RawMessage) (VoiceJoin, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return VoiceJoin{}, errors.New("the processing must name echo_cancellation, noise_suppression and " +
			"auto_gain")
	}
	return DecodeVoiceJoin(body)
}

// EventVoice tells a watching client that what PathVoice answers has changed: a join began, a call came up,
// or one ended, and since M34 who speaks, and what the person or the processing changed. It carries an
// empty object, and the client asks, as for EventUpdate.
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
	// EndedInCall is whether Reason is why a call that was up ended, rather than why a join failed before it
	// was (M33): a client words the two apart. False while there is a call, and before any.
	EndedInCall bool `json:"ended_in_call"`
	// Available is whether this daemon can join a call at all, and Unavailable says why not when it
	// cannot. Before M33 only a build for M26's harness could.
	Available   bool   `json:"available"`
	Unavailable string `json:"unavailable"`
	// Processing is the call's audio processing as its join asked (M30), while there is a call; nil when
	// there is none.
	Processing *VoiceJoin `json:"processing"`
	// Microphone and Speakers are what the call's devices are doing (M32), as its worker last said; nil
	// before it has said. They stay after a call they ended, both failing, so a client can say why it
	// ended, and go after any other end, a leave and the next join.
	Microphone *VoiceDevice `json:"microphone"`
	Speakers   *VoiceDevice `json:"speakers"`
	// Rights is what the account may do in the call (M33), as the instance last said: send its microphone,
	// and hear the call. A moderator's server mute or a channel without SPEAK takes the first, a server
	// deafen the second, and this client applies them. Nil while there is no call answered.
	Rights *VoiceRights `json:"rights"`

	// Self is what the person has set on themselves (M34), in a call or not: it goes with the next join, and
	// lasts until the sign-in ends. LocalMutes is whom they muted for themselves alone, sorted, and
	// MaxStreams how many people they hear at once, nil for every one the call has room for; both last as
	// long. None of the three is the instance's, which hears of Self alone.
	Self       VoiceSelf `json:"self"`
	LocalMutes []string  `json:"local_mutes"`
	MaxStreams *int      `json:"max_streams"`
	// Speaking is who the voice server says is speaking now, as user ids, while there is a call, local
	// mutes included: the indicator is how somebody finds whom to mute or report. [] with no call.
	Speaking []string `json:"speaking"`
	// DSP is what the call's audio processing is doing, as its worker last said, where Processing is what
	// was asked: nil before it has said and with no call.
	DSP *VoiceDSP `json:"dsp"`
}

// VoiceSelf is what the person has set on themselves.
type VoiceSelf struct {
	Mute bool `json:"mute"`
	Deaf bool `json:"deaf"`
}

// VoiceDSP is what the call's audio processing is doing: VoiceDSPStarting while it compiles,
// VoiceDSPRunning with the stages that process the microphone, or VoiceDSPFailed for the rest of the call,
// Reason, the worker's words cleaned, saying why.
type VoiceDSP struct {
	State            string `json:"state"`
	Reason           string `json:"reason"`
	EchoCancellation bool   `json:"echo_cancellation"`
	NoiseSuppression bool   `json:"noise_suppression"`
	AutoGain         bool   `json:"auto_gain"`
}

// The DSP's states, voiceipc's words, held equal by a test in daemon/internal/voiceworker.
const (
	VoiceDSPStarting = "starting"
	VoiceDSPRunning  = "running"
	VoiceDSPFailed   = "failed"
)

// VoiceRights is what the account may do in its call.
type VoiceRights struct {
	Send bool `json:"send"`
	Hear bool `json:"hear"`
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
	// VoiceDeviceNoSession is the account not logged in at the machine, whoever else is, so nothing it runs
	// may use a device (Linux).
	VoiceDeviceNoSession = "no_session"
	// VoiceDeviceFailed is anything else, its reason saying what.
	VoiceDeviceFailed = "failed"
)
