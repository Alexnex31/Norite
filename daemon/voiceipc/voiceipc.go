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
// changes. The daemon sends JOIN once, ANSWER once, and LEAVE when the call should end.
const (
	OpHello gatewayproto.Opcode = 1
	OpOffer gatewayproto.Opcode = 2
	OpState gatewayproto.Opcode = 3

	OpJoin   gatewayproto.Opcode = 10
	OpAnswer gatewayproto.Opcode = 11
	OpLeave  gatewayproto.Opcode = 12
)

// Bounds. The largest frame either way is the offer or the answer, each under voiceproto.MaxOffer, which
// JSON escaping may grow by a twentieth; 64 KiB leaves room for that and for M29–M34's fields.
const (
	// MaxWorkerFrame bounds a frame from the worker, and MaxDaemonFrame one from the daemon.
	MaxWorkerFrame = 64 << 10
	MaxDaemonFrame = 64 << 10

	// MaxReason bounds the reason a STATE carries, in bytes.
	MaxReason = 256
	// MaxICEServers and MaxURLs bound what a JOIN names: TURN over UDP, TCP and TLS in each family is six.
	MaxICEServers = 4
	MaxURLs       = 8
)

// Hello is the worker's first frame: which version it is. The daemon checks it by the attach socket's rule
// (gatewayproto.Check, the daemon as the server): two programs of one install always agree, and the check
// is for a package manager that replaced one under the other.
type Hello struct {
	Version string `json:"version"`
}

// Join tells the worker to make its call's connection: gather with these servers and offer.
type Join struct {
	// ICEServers are the TURN servers to gather relayed candidates from, with the account's credential.
	ICEServers []ICEServer `json:"ice_servers"`
	// Loopback admits loopback candidates, for an SFU on this machine: M26's harness, and tests.
	Loopback bool `json:"loopback"`
}

// ICEServer is one TURN server, as WebRTC names it.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
}

// Offer is the worker's SDP offer, every candidate gathered. At most voiceproto.MaxOffer bytes.
type Offer struct {
	SDP string `json:"sdp"`
}

// Answer is the SFU's answer to the offer.
type Answer struct {
	SDP string `json:"sdp"`
}

// Leave ends the call. It carries nothing.
type Leave struct{}

// State is what the worker's call has become.
type State struct {
	// State is one of the State* words.
	State string `json:"state"`
	// Reason says why a call ended, in words for a person, or "".
	Reason string `json:"reason"`
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

// Check refuses a state that is not one of the words, or a reason past MaxReason.
func (s State) Check() error {
	switch s.State {
	case StateConnecting, StateReady, StateEnded:
	default:
		return fmt.Errorf("an unknown state %q", truncate(s.State, 32))
	}
	if len(s.Reason) > MaxReason {
		return fmt.Errorf("a reason of %d bytes, past %d", len(s.Reason), MaxReason)
	}
	return nil
}

// Check refuses a join naming more servers or URLs than any correct one.
func (j Join) Check() error {
	if len(j.ICEServers) > MaxICEServers {
		return fmt.Errorf("%d ICE servers, past %d", len(j.ICEServers), MaxICEServers)
	}
	for _, s := range j.ICEServers {
		if len(s.URLs) == 0 || len(s.URLs) > MaxURLs {
			return fmt.Errorf("an ICE server with %d URLs", len(s.URLs))
		}
	}
	return nil
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
