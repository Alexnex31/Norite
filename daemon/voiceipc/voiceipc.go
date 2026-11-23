// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package voiceipc is what the daemon and its voice-worker say to each other (M28): the frames on the two
// pipes the worker inherits, one each way, in the attach socket's framing (daemon/ipc).
//
// It lives outside internal/ so the worker, a module of its own, imports it; the daemon defines the pipe,
// so the daemon's module owns its shape. contracts/voice-worker.schema.json is the source of truth for every
// frame, and the tests at both ends validate what they send against it.
//
// Trust tier (rule 16), the third beside ADR 0017's two: the pipes are anonymous and inherited by one
// child, so no other process can reach them, and the daemon trusts the worker least of anything it talks
// to. From M29 the worker decodes a stranger's audio in C, and a worker that was exploited holds the pipe.
// So the daemon bounds every frame by MaxWorkerFrame and a rate, decodes it strictly, and kills the worker
// at the first frame that breaks any of it. The worker holds no credential but the TURN one, which reaches
// the SFU's port and nothing else (ADR 0023); no frame carries an access token.
//
// That keeps a broken worker from taking the daemon down through its pipe, and confines nothing: the worker
// runs as the account, and one that was exploited reaches what the account reaches, the attach socket
// included. Whether the OS confines it is M29's to decide (ADR 0017 as amended).
//
// The pipes are not stdin and stdout. From M29 the worker links C libraries, and a printf in one would land
// mid-frame; on Windows, rerouting descriptor 1 afterwards does not move the C runtime's. So the worker is
// started with two more (Pipes), its stdin is the null device, and whatever it prints goes to the daemon's
// log and never into the protocol.
package voiceipc

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/backend/voiceproto"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// The ops. A worker sends HELLO first and once, then OFFER once it has a JOIN, then STATE as its call
// changes, and from its first STATE, DEVICES as its microphone and speakers change (M32), DSP as its audio
// processing changes and, once ready, SPEAKING as who speaks changes (M34). The daemon sends JOIN once,
// ANSWER once, RIGHTS whenever what the account may do in the call changes after the answer (M33),
// SUBSCRIBE and PROCESSING whenever the person changes whom they hear or how their microphone is processed
// (M34), and LEAVE when the call should end.
const (
	OpHello    gatewayproto.Opcode = 1
	OpOffer    gatewayproto.Opcode = 2
	OpState    gatewayproto.Opcode = 3
	OpDevices  gatewayproto.Opcode = 4
	OpSpeaking gatewayproto.Opcode = 5
	OpDSP      gatewayproto.Opcode = 6

	OpJoin       gatewayproto.Opcode = 10
	OpAnswer     gatewayproto.Opcode = 11
	OpLeave      gatewayproto.Opcode = 12
	OpRights     gatewayproto.Opcode = 13
	OpSubscribe  gatewayproto.Opcode = 14
	OpProcessing gatewayproto.Opcode = 15
)

// Bounds. The largest frame either way is the offer or the answer, each under voiceproto.MaxOffer, which
// JSON escaping may grow by a twentieth; 64 KiB leaves room for that and for M29–M34's fields.
const (
	// MaxWorkerFrame bounds a frame from the worker, and MaxDaemonFrame one from the daemon.
	MaxWorkerFrame = 64 << 10
	MaxDaemonFrame = 64 << 10

	// MaxReason bounds the reason a STATE carries, in bytes.
	MaxReason = 256
	// MaxICEServers and MaxURLs bound what an ANSWER names: TURN over UDP, TCP and TLS in each family is
	// six. MaxURL and MaxCredential bound each string, which come from the instance. A URL is built from the
	// instance's host name, up to DNS's 253 bytes, and its scheme, port and transport: 512 holds any
	// (M33's /code-review: at 256, a host name past about 235 bytes failed every join on its instance).
	MaxICEServers = 4
	MaxURLs       = 8
	MaxURL        = 512
	MaxCredential = 128

	// MinBitrate and MaxBitrate bound the call's bitrate (M33): the congestion controller's floor, and the
	// most a 20 ms Opus packet with its headers carries under the SFU's 256 kbps track bound. The voice
	// server names it, so the worker clamps it to these whatever it says; zero is the worker's default.
	MinBitrate = 16_000
	MaxBitrate = 224_000

	// MaxSpeaking bounds who a SPEAKING names, and MaxDrop whom a SUBSCRIBE drops: the SFU's own bound on a
	// subscribe's list (voiceproto.MaxSubscribeList), a full room.
	MaxSpeaking = voiceproto.MaxSubscribeList
	MaxDrop     = voiceproto.MaxSubscribeList
	// MaxSlots is the most slots a SUBSCRIBE's cap may name: the slots the worker receives on.
	MaxSlots = 3
)

// Hello is the worker's first frame: which version it is. The daemon checks it by the attach socket's rule
// (gatewayproto.Check, the daemon as the server): two programs of one install always agree, and the check
// is for a package manager that replaced one under the other.
type Hello struct {
	Version string `json:"version"`
}

// Join tells the worker to make its call's connection and offer. The offer carries no candidate: the SFU
// is ICE-lite and learns the client's addresses from its checks, so the worker gathers only once the
// answer has named the servers to gather with (M33), and a TURN server that cannot be reached no longer
// holds the offer back.
type Join struct {
	// Loopback admits loopback candidates, for an SFU on this machine: M26's harness, and tests.
	Loopback bool `json:"loopback"`
	// Processing is which of the DSP's components process the microphone (M30), as the join request
	// carried it from the client's config.toml. All off still runs RNNoise, for the voice probability the
	// audio level's V flag follows. The worker converts it to its own dsp.Settings, which has the same
	// fields, so a field added to one and not the other does not compile.
	Processing ipc.VoiceJoin `json:"processing"`
}

// ICEServer is one TURN server, as WebRTC names it.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
}

// Offer is the worker's SDP offer, with no candidate. At most voiceproto.MaxOffer bytes.
type Offer struct {
	SDP string `json:"sdp"`
}

// Answer is the SFU's answer to the offer, and what the worker gathers and sends with.
type Answer struct {
	SDP string `json:"sdp"`
	// ICEServers are the TURN servers to gather relayed candidates from, with the account's credential.
	ICEServers []ICEServer `json:"ice_servers"`
	// Bitrate is the channel's, which the encoder starts at and the congestion controller never exceeds,
	// clamped by the worker to MinBitrate and MaxBitrate; zero is the worker's default.
	Bitrate int `json:"bitrate"`
	// Rights is what the account may do in the call as it starts.
	Rights Rights `json:"rights"`
}

// Rights is what the account may do in its call (M33): send its microphone, and hear the call. The
// instance's server mute and a channel without SPEAK take the first, its server deafen the second. The
// client applies them and the SFU enforces neither, which the security ledger records. Not sending keeps
// the microphone's processing running, so the echo canceller stays converged for when sending resumes.
type Rights struct {
	Send bool `json:"send"`
	Hear bool `json:"hear"`
}

// Leave ends the call. It carries nothing.
type Leave struct{}

// Speaking is who the SFU says is speaking in the call now (M34), as user ids, the newest list only: the
// worker sends at most a few a second whatever the SFU sends, since the daemon's bound on its frames would
// otherwise be a talkative room's to break.
type Speaking struct {
	Users []string `json:"users"`
}

// Subscribe is whom the person does not want to hear, and how many people at once (M34): the daemon's
// local mutes and the voice.max_streams setting, whole each time. The worker asks the SFU to forward none
// of Drop and at most Cap of the rest, and plays nothing of Drop that is already on its way.
type Subscribe struct {
	Drop []string `json:"drop"`
	// Cap is how many slots the SFU may fill, 1 to MaxSlots; nil is every slot the worker has.
	Cap *int `json:"cap"`
}

// Processing is how the person wants the microphone processed, changed during the call (M34). The join
// carries the first.
type Processing struct {
	Processing ipc.VoiceJoin `json:"processing"`
}

// DSP is what the audio processing is doing (M34), where the join's Processing is only what was asked:
// starting while it compiles, running with the stages that process the microphone, or failed for the rest
// of the call, Reason saying why. RNNoise runs whenever it runs, for the audio level's V flag.
type DSP struct {
	State            string `json:"state"`
	Reason           string `json:"reason"`
	EchoCancellation bool   `json:"echo_cancellation"`
	NoiseSuppression bool   `json:"noise_suppression"`
	AutoGain         bool   `json:"auto_gain"`
}

// The states the DSP is reported in.
const (
	DSPStarting = "starting"
	DSPRunning  = "running"
	DSPFailed   = "failed"
)

// State is what the worker's call has become.
type State struct {
	// State is one of the State* words.
	State string `json:"state"`
	// Reason says why a call ended, in words for a person, or "".
	Reason string `json:"reason"`
	// Devices is, on an ended call, the devices that ended it, both failing as it started (M32): the daemon
	// keeps them in its status after that end alone, from this frame rather than the last DEVICES it read.
	// Nil after any other end, and on every other state.
	Devices *Devices `json:"devices"`
}

// The states a worker reports.
const (
	// StateConnecting: the answer is applied, and ICE and DTLS are under way.
	StateConnecting = "connecting"
	// StateReady: connected, and the SFU's ready has arrived on the data channel.
	StateReady = "ready"
	// StateEnded: the call is over, Reason says why, and the worker is about to exit.
	StateEnded = "ended"
)

// Devices is how the call's microphone and speakers are doing (M32, ADR 0034 §1): the whole of it, each
// time either changes.
type Devices struct {
	Microphone Device `json:"microphone"`
	Speakers   Device `json:"speakers"`
	// Decided is whether the call's start has been judged and the call goes on (D4): a join waiting on a
	// failing microphone answers on it. A start judged to end the call says so with the ended state, which
	// follows, and its report is not decided: Decided is the worker's goesOn, never its startJudged.
	Decided bool `json:"decided"`
}

// Device is one direction's outcome.
type Device struct {
	// State is one of the Device* words.
	State string `json:"state"`
	// Reason says what went wrong, in words for a person, or "". A client words the state itself for its
	// OS, a refusal's setting included, and shows the reason beside it.
	Reason string `json:"reason"`
}

// The states a device is reported in. The worker words none of them for a person: it does not know which
// client will show them, nor, for refused, which setting that client's OS names.
const (
	// DeviceOpening: being opened, or opened and not yet delivering.
	DeviceOpening = "opening"
	// DeviceOpen: delivering audio, from its first frame.
	DeviceOpen = "open"
	// DeviceAsking: the OS is asking the person whether the program may use it (macOS).
	DeviceAsking = "asking"
	// DeviceRefused: the OS or the person refused it.
	DeviceRefused = "refused"
	// DeviceNoDevice: there is none to open.
	DeviceNoDevice = "no_device"
	// DeviceNoSession: the account is not logged in at the machine, and nothing it runs can use a device
	// until it is (Linux), whoever else is.
	DeviceNoSession = "no_session"
	// DeviceFailed: anything else, Reason saying what.
	DeviceFailed = "failed"
)

// Failing is whether a device in state s carries no audio and is not waiting to: refused, absent, without
// a session, or failed.
func Failing(s string) bool {
	switch s {
	case DeviceRefused, DeviceNoDevice, DeviceNoSession, DeviceFailed:
		return true
	}
	return false
}

// Check refuses a state that is not one of the words, or a reason past MaxReason, in either device.
func (d Devices) Check() error {
	for _, dev := range []struct {
		name string
		d    Device
	}{{"microphone", d.Microphone}, {"speakers", d.Speakers}} {
		switch dev.d.State {
		case DeviceOpening, DeviceOpen, DeviceAsking, DeviceRefused, DeviceNoDevice, DeviceNoSession, DeviceFailed:
		default:
			return fmt.Errorf("the %s in an unknown state %q", dev.name, truncate(dev.d.State, 32))
		}
		if len(dev.d.Reason) > MaxReason {
			return fmt.Errorf("a reason of %d bytes for the %s, past %d", len(dev.d.Reason), dev.name, MaxReason)
		}
	}
	return nil
}

// Check refuses what no correct worker sends, on top of the strict decoding: a payload the schema allows
// but the protocol does not. The daemon calls it on every frame from the worker.
func (o Offer) Check() error {
	switch {
	case o.SDP == "":
		return errors.New("an empty offer")
	case len(o.SDP) > voiceproto.MaxOffer:
		return fmt.Errorf("an offer of %d bytes, past %d", len(o.SDP), voiceproto.MaxOffer)
	}
	return nil
}

// Check refuses a state that is not one of the words, a reason past MaxReason, or devices that ended a
// call that has not.
func (s State) Check() error {
	switch s.State {
	case StateConnecting, StateReady, StateEnded:
	default:
		return fmt.Errorf("an unknown state %q", truncate(s.State, 32))
	}
	if len(s.Reason) > MaxReason {
		return fmt.Errorf("a reason of %d bytes, past %d", len(s.Reason), MaxReason)
	}
	if s.Devices != nil {
		if s.State != StateEnded {
			return fmt.Errorf("devices that ended a call that is %s", s.State)
		}
		if err := s.Devices.Check(); err != nil {
			return err
		}
		if !Failing(s.Devices.Microphone.State) || !Failing(s.Devices.Speakers.State) {
			return errors.New("devices that ended a call, one of them working")
		}
		if s.Devices.Decided {
			return errors.New("devices that ended a call, saying it goes on")
		}
	}
	return nil
}

// snowflakes reports whether every id is one: 1 to 20 digits.
func snowflakes(ids []string) bool {
	for _, id := range ids {
		if id == "" || len(id) > 20 || strings.Trim(id, "0123456789") != "" {
			return false
		}
	}
	return true
}

// Check refuses more users than MaxSpeaking, or one that is not an id.
func (s Speaking) Check() error {
	if len(s.Users) > MaxSpeaking {
		return fmt.Errorf("%d speaking, past %d", len(s.Users), MaxSpeaking)
	}
	if !snowflakes(s.Users) {
		return errors.New("a speaker that is not a user id")
	}
	return nil
}

// Check refuses more users than MaxDrop, one that is not an id, or a cap outside 1 to MaxSlots.
func (s Subscribe) Check() error {
	if len(s.Drop) > MaxDrop {
		return fmt.Errorf("%d dropped, past %d", len(s.Drop), MaxDrop)
	}
	if !snowflakes(s.Drop) {
		return errors.New("a dropped user that is not a user id")
	}
	if s.Cap != nil && (*s.Cap < 1 || *s.Cap > MaxSlots) {
		return fmt.Errorf("a cap of %d slots", *s.Cap)
	}
	return nil
}

// Check refuses a state that is not one of the words, a reason past MaxReason, a stage running in a DSP
// that is not, or a failure saying nothing.
func (d DSP) Check() error {
	switch d.State {
	case DSPStarting, DSPRunning, DSPFailed:
	default:
		return fmt.Errorf("the audio processing in an unknown state %q", truncate(d.State, 32))
	}
	if len(d.Reason) > MaxReason {
		return fmt.Errorf("a reason of %d bytes, past %d", len(d.Reason), MaxReason)
	}
	if d.State != DSPRunning && (d.EchoCancellation || d.NoiseSuppression || d.AutoGain) {
		return fmt.Errorf("a stage processing the microphone while the processing is %s", d.State)
	}
	if (d.State == DSPFailed) != (d.Reason != "") {
		return errors.New("a reason for the processing that did not fail, or none for one that did")
	}
	return nil
}

// Check refuses an answer naming more servers, URLs or bytes than any correct one, or none.
func (a Answer) Check() error {
	if a.SDP == "" || len(a.SDP) > voiceproto.MaxOffer {
		return fmt.Errorf("an answer of %d bytes", len(a.SDP))
	}
	if len(a.ICEServers) > MaxICEServers {
		return fmt.Errorf("%d ICE servers, past %d", len(a.ICEServers), MaxICEServers)
	}
	for _, s := range a.ICEServers {
		if len(s.URLs) == 0 || len(s.URLs) > MaxURLs {
			return fmt.Errorf("an ICE server with %d URLs", len(s.URLs))
		}
		for _, u := range s.URLs {
			if u == "" || len(u) > MaxURL {
				return fmt.Errorf("an ICE server URL of %d bytes", len(u))
			}
		}
		if len(s.Username) > MaxCredential || len(s.Credential) > MaxCredential {
			return errors.New("an ICE server credential past its bound")
		}
	}
	if a.Bitrate < 0 {
		return fmt.Errorf("a bitrate of %d", a.Bitrate)
	}
	return nil
}

// ClampBitrate is the bitrate a call runs at for what the voice server named: within MinBitrate and
// MaxBitrate, or def for none.
func ClampBitrate(named, def int) int {
	if named == 0 {
		return def
	}
	return min(max(named, MinBitrate), MaxBitrate)
}

// CutReason cuts a reason to MaxReason bytes on a character's boundary. A cut through a character would be
// encoded as U+FFFD, three bytes, and a reason cut to the bound would then exceed it: the worker would be
// killed for the length of its own error message.
func CutReason(s string) string {
	if len(s) <= MaxReason {
		return s
	}
	cut := MaxReason
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Write encodes v and writes it as one frame of op.
func Write(w io.Writer, op gatewayproto.Opcode, v any) error {
	f, err := ipc.Encode(op, v)
	if err != nil {
		return err
	}
	return ipc.WriteFrame(w, f)
}

// Read reads one frame of at most limit bytes. Decode its payload with ipc.Decode, which is strict.
func Read(r io.Reader, limit int) (gatewayproto.Frame, error) {
	return ipc.ReadFrame(r, limit)
}

// PipesFlag is the worker's flag naming its two pipes: the one it reads, then the one it writes, as
// numbers. On Unix they are descriptors, on Windows handle values; os.NewFile takes either.
const PipesFlag = "pipes"

// FormatPipes is the flag's value for a read pipe and a write pipe.
func FormatPipes(read, write uintptr) string {
	return strconv.FormatUint(uint64(read), 10) + "," + strconv.FormatUint(uint64(write), 10)
}

// ParsePipes reads the flag's value back.
func ParsePipes(s string) (read, write uintptr, err error) {
	a, b, ok := strings.Cut(s, ",")
	if !ok {
		return 0, 0, fmt.Errorf("-%s is two numbers, comma-separated", PipesFlag)
	}
	r, err1 := strconv.ParseUint(a, 10, 64)
	w, err2 := strconv.ParseUint(b, 10, 64)
	if err := errors.Join(err1, err2); err != nil || r == w {
		return 0, 0, fmt.Errorf("-%s is two different numbers, comma-separated", PipesFlag)
	}
	return uintptr(r), uintptr(w), nil
}
