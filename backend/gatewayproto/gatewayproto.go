// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package gatewayproto is the gateway's wire protocol: the frame envelope, the op-codes, the close codes,
// the control payloads, and the version rule both ends apply at the handshake.
//
// It lives outside backend/internal for operatortoken's reason (M10): the daemon speaks exactly this
// protocol from M19, and Go's internal/ rule would make a package under internal/ unreachable from another
// module. Two copies of a wire format drift, and the failure is a connection that closes with a code one
// side has no name for. So the format lives once, here, and everything that decides anything (who may
// connect, what they receive) stays in backend/internal/gateway.
//
// contracts/gateway-events.schema.json is the source of truth for every frame. This package is its Go
// form, and the gateway's tests validate every frame the server sends against that document.
package gatewayproto

import (
	"encoding/json"
	"time"
)

// Opcode names what a frame is for. The numbering is Discord's, which docs/architecture.md §2 adopts.
type Opcode int

// Server to client: Dispatch, Reconnect, InvalidSession, Hello, HeartbeatAck.
// Client to server: Heartbeat, Identify, Resume, and three reserved for later milestones.
const (
	OpDispatch     Opcode = 0
	OpHeartbeat    Opcode = 1
	OpIdentify     Opcode = 2
	OpResume       Opcode = 6
	OpReconnect    Opcode = 7
	OpInvalidSess  Opcode = 9
	OpHello        Opcode = 10
	OpHeartbeatAck Opcode = 11

	// Reserved and refused until the milestone that builds each: presence at M38, voice state in Phase E,
	// member requests when a client first needs a member list over the gateway. Refused with
	// CloseUnknownOpcode rather than ignored, because a client that thinks its presence update landed is
	// worse off than one told it did not.
	OpPresenceUpdate      Opcode = 3
	OpVoiceStateUpdate    Opcode = 4
	OpRequestGuildMembers Opcode = 8
)

// Frame is the envelope every message on the socket shares.
//
// S and T are present on every frame, null except on a dispatch, rather than omitted: a client decoding
// into a fixed struct then never has to tell "absent" from "null", and the schema can require both.
type Frame struct {
	Op Opcode          `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s"`
	T  *string         `json:"t"`
}

// Close codes, sent in the WebSocket close frame. Every refusal the gateway makes is one of these, so a
// client can tell "re-identify", "log in again", "upgrade" and "you are sending too much" apart without
// parsing the reason text, which is for people.
//
// The numbers are Discord's where the meaning matches. 4006 is skipped because Discord retired it, and a
// client written against both should not find it meaning something here.
const (
	CloseUnknownError         = 4000
	CloseUnknownOpcode        = 4001
	CloseDecodeError          = 4002
	CloseNotAuthenticated     = 4003
	CloseAuthenticationFailed = 4004
	CloseAlreadyAuthenticated = 4005
	CloseInvalidSeq           = 4007
	CloseRateLimited          = 4008
	CloseSessionTimedOut      = 4009
	CloseVersionMismatch      = 4010
	CloseSessionRevoked       = 4011
	CloseTooSlow              = 4012
	CloseInvalidIntents       = 4013
)

// Hello is the first frame the server sends.
type Hello struct {
	// HeartbeatInterval is how often, in milliseconds, the client must send a heartbeat.
	HeartbeatInterval int64 `json:"heartbeat_interval"`
	// ServerTime lets the client compute its clock offset for token-expiry checks (ADR 0010).
	ServerTime time.Time `json:"server_time"`
	// Version is the server's release version, or "dev" for an unstamped build (ADR 0033).
	Version string `json:"version"`
}

// Identify authenticates a new session.
type Identify struct {
	// Token is an access token. Never an API token: the gateway carries everything an account can see, and
	// bots reach it through the daemon (M22).
	Token      string             `json:"token"`
	Properties IdentifyProperties `json:"properties"`
	// Intents is reserved. It must be absent or 0 until a milestone defines a bit; refusing a value now is
	// the reversible direction, where accepting one would mean a later bit is already "granted".
	Intents *int64 `json:"intents,omitempty"`
}

// IdentifyProperties describes the connecting client.
type IdentifyProperties struct {
	OS      string `json:"os"`
	Client  string `json:"client"`
	Version string `json:"version"`
}

// Resume continues a session after a disconnect.
//
// It carries the token as well as the session id. With the id alone, anybody who read one from a log line
// or a crash report could resume somebody else's stream (docs/architecture.md §2).
type Resume struct {
	Token     string `json:"token"`
	SessionID string `json:"session_id"`
	Seq       int64  `json:"seq"`
}
