// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ipc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
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
	// with every switch named, for the call there is. Without a call it changes nothing: the next join
	// carries its own, from the client's config.toml, which the client writes. It said "every join after it
	// until the sign-in ends", which no daemon did (M34's second /code-review): a client that turns a switch
	// off for good writes the file as well, as `norite voice processing` does.
	PathVoiceProcessing = PathVoice + "/processing"
	// PathVoiceMode is POST (M35): how the call's microphone is let through, by voice or by push-to-talk and
	// with which key, as VoiceInput names them, for the call there is. Without a call it changes nothing: the
	// next join carries its own, from the client's config.toml, which the client writes. It answers a
	// VoiceStatus, and is sent only to a daemon that answered PathVoice, as the processing's body is.
	PathVoiceMode = PathVoice + "/mode"

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
// would take a place no user can fill (M34's manual pass). A snowflake is a signed 64-bit number, which is
// what the voice server parses: past it, the server refused the whole subscribe and closed every call the
// mute was sent with (M34's /code-review).
func IsUserID(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == s
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
	if !ok || !IsChannelID(ch) {
		return "", false
	}
	return ch, true
}

// IsChannelID is whether s is a channel id as a join's path carries one: digits, at most twenty, an unsigned
// 64-bit number. The breadcrumb's channel is held to it as the path is (M36).
func IsChannelID(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	_, err := strconv.ParseUint(s, 10, 64)
	return err == nil
}

// VoiceJoin is which of the DSP's components process the microphone (M30), as the client's config.toml
// says: the first half of a PathVoiceJoin request's body, and the whole of a PathVoiceProcessing one.
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

// VoiceInput is how the microphone is let through (M35): by voice, RNNoise deciding, or by push-to-talk,
// Key the key held to talk, in CanonicalKey's spelling. The second half of a PathVoiceJoin request's body,
// and the whole of a PathVoiceMode one. The key goes to the daemon, which registers it; the worker hears
// only whether it is down.
type VoiceInput struct {
	Mode string `json:"input_mode"`
	Key  string `json:"push_to_talk_key"`
}

// The input modes, config.toml's words for voice.input_mode, and the key push-to-talk holds unless
// voice.push_to_talk_key names another (the author's choice at M35's planning).
const (
	VoiceInputVoice      = "voice"
	VoiceInputPushToTalk = "push_to_talk"
	DefaultPushToTalkKey = "ctrl+shift+space"
)

// DefaultVoiceInput is input by voice, with the default key: a join with no body.
func DefaultVoiceInput() VoiceInput {
	return VoiceInput{Mode: VoiceInputVoice, Key: DefaultPushToTalkKey}
}

// VoiceJoinRequest is the body of a PathVoiceJoin request: the processing and the input, side by side in
// one object.
type VoiceJoinRequest struct {
	VoiceJoin
	VoiceInput
}

// The modifiers a key may carry, in the order CanonicalKey writes them.
var keyModifiers = []string{"ctrl", "alt", "shift", "super"}

// MaxKey bounds a key as written.
const MaxKey = 64

// CanonicalKey reads a push-to-talk key, "ctrl+shift+space", and writes it the one way the daemon and every
// client compare it: lowercase, its modifiers in keyModifiers' order, each once, then the key. The key is
// a letter, a digit, space or f1 to f24. A key other than a function key needs ctrl, alt or super, since
// a letter or shift and a letter alone would be taken from typing everywhere while a call lasts (ADR 0034
// §3). macOS adds its own condition, Control or Command, which its backend says when it refuses.
func CanonicalKey(s string) (string, error) {
	bad := func(why string) (string, error) {
		return "", fmt.Errorf("a push-to-talk key %s, as in %s", why, DefaultPushToTalkKey)
	}
	if s == "" || len(s) > MaxKey {
		return bad("is a few modifiers and a key joined by +")
	}
	parts := strings.Split(strings.ToLower(s), "+")
	key, mods := parts[len(parts)-1], parts[:len(parts)-1]
	held := map[string]bool{}
	for _, m := range mods {
		if !slices.Contains(keyModifiers, m) || held[m] {
			return bad("takes each of ctrl, alt, shift and super at most once, before the key")
		}
		held[m] = true
	}
	function := false
	switch {
	case key == "space":
	case len(key) == 1 && (key[0] >= 'a' && key[0] <= 'z' || key[0] >= '0' && key[0] <= '9'):
	case len(key) >= 2 && len(key) <= 3 && key[0] == 'f':
		n, err := strconv.Atoi(key[1:])
		if err != nil || n < 1 || n > 24 || strconv.Itoa(n) != key[1:] {
			return bad("ends with a letter, a digit, space or f1 to f24")
		}
		function = true
	default:
		return bad("ends with a letter, a digit, space or f1 to f24")
	}
	if !function && !held["ctrl"] && !held["alt"] && !held["super"] {
		return bad("other than a function key needs ctrl, alt or super, or typing would hold it")
	}
	out := make([]string, 0, len(parts))
	for _, m := range keyModifiers {
		if held[m] {
			out = append(out, m)
		}
	}
	return strings.Join(append(out, key), "+"), nil
}

// field is one key of a body decodeExactly reads: a boolean or a string, written where it points.
type field struct {
	b *bool
	s *string
}

// decodeExactly reads an object naming every one of fields exactly once, each of its type, and nothing
// after it. It walks the tokens rather than decoding into a struct, because encoding/json matches a key in
// any case, keeps the last of two with one name, and stops before a stray ] or } after the object, and the
// contract refuses all three (M30 /code-review).
func decodeExactly(body []byte, fields map[string]field) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		key, isKey := tok.(string)
		f, known := fields[key]
		if err != nil || !isKey || !known || seen[key] {
			return false
		}
		seen[key] = true
		tok, err = dec.Token()
		if err != nil {
			return false
		}
		switch v := tok.(type) {
		case bool:
			if f.b == nil {
				return false
			}
			*f.b = v
		case string:
			if f.s == nil {
				return false
			}
			*f.s = v
		default:
			return false
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') || len(seen) != len(fields) {
		return false
	}
	_, err := dec.Token()
	return errors.Is(err, io.EOF)
}

// absent is a body that says nothing: none, or null.
func absent(body json.RawMessage) bool {
	trimmed := bytes.TrimSpace(body)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func (j *VoiceJoin) fields() map[string]field {
	return map[string]field{"echo_cancellation": {b: &j.EchoCancellation},
		"noise_suppression": {b: &j.NoiseSuppression}, "auto_gain": {b: &j.AutoGain}}
}

func (in *VoiceInput) fields() map[string]field {
	return map[string]field{"input_mode": {s: &in.Mode}, "push_to_talk_key": {s: &in.Key}}
}

// Check refuses a mode that is not one of the words, and a key not written the one way CanonicalKey writes
// it: a client writes it so before sending, and a key compared as text is the same key only spelled once.
func (in *VoiceInput) Check() error {
	if in.Mode != VoiceInputVoice && in.Mode != VoiceInputPushToTalk {
		return fmt.Errorf("the input mode is %s or %s", VoiceInputVoice, VoiceInputPushToTalk)
	}
	key, err := CanonicalKey(in.Key)
	if err != nil {
		return err
	}
	if key != in.Key {
		return fmt.Errorf("a push-to-talk key is sent as %s writes it", key)
	}
	return nil
}

// DecodeVoiceJoin reads a PathVoiceJoin request's body. No body, or null, is every component on and input
// by voice. Anything else is an object naming the three switches and both halves of the input exactly, each
// once, and nothing after it: a switch left out would otherwise read as off, which is not what a client
// that forgot it meant.
func DecodeVoiceJoin(body json.RawMessage) (VoiceJoinRequest, error) {
	if absent(body) {
		return VoiceJoinRequest{VoiceJoin: VoiceAllOn(), VoiceInput: DefaultVoiceInput()}, nil
	}
	var out VoiceJoinRequest
	fields := out.VoiceJoin.fields()
	maps.Copy(fields, out.VoiceInput.fields())
	if !decodeExactly(bytes.TrimSpace(body), fields) {
		return VoiceJoinRequest{}, errors.New("the join's settings must be an object naming echo_cancellation, " +
			"noise_suppression and auto_gain, each true or false, and input_mode and push_to_talk_key, each " +
			"once, and nothing else")
	}
	if err := out.Check(); err != nil {
		return VoiceJoinRequest{}, err
	}
	return out, nil
}

// DecodeVoiceProcessing reads a PathVoiceProcessing request's body: the three switches, every one named.
// Unlike a join's, no body is not every component on, since a change that names nothing changes nothing.
func DecodeVoiceProcessing(body json.RawMessage) (VoiceJoin, error) {
	var out VoiceJoin
	if absent(body) || !decodeExactly(bytes.TrimSpace(body), out.fields()) {
		return VoiceJoin{}, errors.New("the processing must be an object naming echo_cancellation, " +
			"noise_suppression and auto_gain, each once and true or false, and nothing else")
	}
	return out, nil
}

// DecodeVoiceMode reads a PathVoiceMode request's body: the input mode and the key, both named.
func DecodeVoiceMode(body json.RawMessage) (VoiceInput, error) {
	var out VoiceInput
	if absent(body) || !decodeExactly(bytes.TrimSpace(body), out.fields()) {
		return VoiceInput{}, errors.New("the input must be an object naming input_mode and push_to_talk_key, " +
			"each once, and nothing else")
	}
	if err := out.Check(); err != nil {
		return VoiceInput{}, err
	}
	return out, nil
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
	// Input is how the call's microphone is let through (M35), while there is a call; nil when there is
	// none.
	Input *VoiceInputStatus `json:"input"`
	// Rejoin is the daemon joining a call again by itself (M36), after the call was lost without anybody
	// leaving it: from the moment it was lost, through each attempt, and once rejoined or given up, until the
	// person next joins or leaves. State says how far it got: VoiceJoining while it waits or tries, Reason
	// saying why the call was lost, VoiceInCall once rejoined, VoiceIdle once given up, Reason saying why.
	// Nil for a call the person joined.
	Rejoin *VoiceRejoin `json:"rejoin"`
}

// VoiceRejoin is why a call is being joined again, and how many attempts have been made, at most
// MaxVoiceRejoins: 0 while the first waits for the sign-in or the instance.
type VoiceRejoin struct {
	Cause   string `json:"cause"`
	Attempt int    `json:"attempt"`
}

// Why a call is joined again (M36): the daemon went away mid-call and started again, its voice worker
// crashed, or the instance no longer held the session the call was signaled on.
const (
	VoiceRejoinDaemonRestarted = "daemon_restarted"
	VoiceRejoinWorkerCrashed   = "worker_crashed"
	VoiceRejoinSessionLost     = "session_lost"
)

// MaxVoiceRejoins is how many times a lost call is joined again before the daemon gives up: a rejoin that
// takes the daemon or its worker down with it is not tried for ever. The count starts again once a call
// has stayed up for a while.
const MaxVoiceRejoins = 3

// VoiceInputStatus is how a call's microphone is let through: the mode and the key as asked, and, by
// push-to-talk, what became of the key and whether it is down.
type VoiceInputStatus struct {
	Mode string `json:"mode"`
	Key  string `json:"key"`
	// Hotkey is what became of the key: VoiceHotkeyRegistering while it is being registered,
	// VoiceHotkeyWaiting while the desktop asks the person about it, VoiceHotkeyRegistered, or
	// VoiceHotkeyUnavailable, Reason saying why. "" by voice.
	Hotkey string `json:"hotkey"`
	Reason string `json:"reason"`
	// Trigger is the key as the desktop says it is bound, which on Linux's portal the person chooses and
	// may differ from Key, cleaned; "" where the OS takes Key as it is.
	Trigger string `json:"trigger"`
	// Held is whether the key is down now.
	Held bool `json:"held"`
}

// The states of a push-to-talk key.
const (
	VoiceHotkeyRegistering = "registering"
	VoiceHotkeyWaiting     = "waiting"
	VoiceHotkeyRegistered  = "registered"
	VoiceHotkeyUnavailable = "unavailable"
)

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
