// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ipc is what the daemon's attach socket speaks, and the client half that speaks it: the CLI now and
// the GUI later attach through here (ADR 0010, M20). Outside internal/ so both can import it, the precedent
// daemon/credentials and daemon/termsafe set: the daemon owns what its socket speaks, as it owns what a
// stored credential is. The server half — listener, peer check, fan-out, relay — is daemon/internal/attach.
//
// # Trust tier (rule 16)
//
// OS-permission-protected, for first-party clients, and no secret. On Unix the socket is daemon.sock inside
// the daemon's 0700 state directory, so only processes running as the owning user can reach it, and the
// daemon checks each peer's uid as well. On Windows it is a named pipe created as the first instance with an
// owner-only DACL, and this package refuses to talk to a pipe owned by anybody else. Every process running
// as that user can attach, which is the tier: they can already read the state directory, and on the file
// backend the credential in it. It is not the bot-automation port, which is secret-protected and lower
// trust, and the two are never interchangeable.
//
// Attaching gives the daemon's DISPATCH stream and the request relay. The relay performs a call with the
// daemon's own access token, which never crosses the socket in either direction: nothing in this package
// has a field that could carry one.
//
// # The protocol
//
// Frames are gatewayproto.Frame — op, d, s, t — as JSON behind a 4-byte big-endian length (frame.go). What
// is reused from the gateway is the envelope, the op numbers for HELLO, IDENTIFY and DISPATCH, the version
// rule (gatewayproto.Check), the close codes whose meaning is the same here, and every dispatch the daemon
// forwards, unchanged but for its sequence number. What is local:
//
//   - HELLO carries the daemon's version and nothing else. There is no heartbeat: a Unix socket and a named
//     pipe report a dead peer as EOF, and a heartbeat exists to detect a silent network, which a local
//     socket does not have.
//   - IDENTIFY carries the client's properties and whether it wants events. It carries no token.
//   - READY is the daemon's own: the signed-in account, if any, and the guild summaries it holds.
//   - Dispatches the daemon originates itself, about this machine rather than the instance. Their types
//     begin LocalEventPrefix, which no gateway dispatch does, and they are numbered with the forwarded
//     ones. EventConfigUpdate is the first.
//   - Request and Response, ops 100 and 101, and Close, op 102. Numbered from 100 so they can never meet an
//     op the gateway assigns later. A raw socket has no close frame of its own, so the daemon sends Close
//     with a code and a reason, then closes.
//
// contracts/daemon-ipc.schema.json is the contract, and references gateway-events.schema.json for the
// frames the two share. The tests validate every frame in both directions against it.
package ipc

import (
	"encoding/json"
	"fmt"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
)

// LocalEventPrefix begins the type of every dispatch the daemon originates itself, as opposed to one it
// forwards from the gateway. It is what the local ops' numbering from 100 is for op codes: a name the
// gateway never assigns, so a dispatch added there later cannot be mistaken for one of these. A test holds
// the gateway's schema to it.
const LocalEventPrefix = "DAEMON_"

// EventConfigUpdate tells a watching client that the user's config.toml changed (M21). It carries an
// empty object and nothing else: the daemon interprets the file on nobody's behalf, so the client reads
// it again through daemon/config. Sent whether or not anybody is signed in, since the file is the
// user's and not the account's.
//
// A new dispatch type is not a change to any frame. A client that predates it ignores a type it does not
// know, as it would a gateway event added later, so this needs nothing of the version rule.
const EventConfigUpdate = LocalEventPrefix + "CONFIG_UPDATE"

// LocalPathPrefix begins the path of a request the daemon answers itself rather than relaying to the
// instance (M21). The request and response frames are the relay's own, so nothing about a frame changes;
// what changes is who answers. It is a first path segment no REST route has or can be given (a test holds
// openapi.yaml to that), and the relay refuses it too, so a request under it never leaves the machine.
//
// Trust tier (rule 16): the attach socket's. First-party clients running as the daemon's own OS account,
// kept to that account by the socket's permissions and the peer check. No secret is involved and none is
// needed; the local bot-automation port (M22) is a different surface with a different tier, and serves
// none of these.
//
// Unlike a relayed request, a local one is answered whether or not anybody is signed in: it is about the
// user's machine. Success is status 200 and a body. Anything else is an Error and never a status, since a
// status is the instance's to give: RelayConflict when the daemon understood and did not do it, RelayFailed
// when it tried and could not.
const LocalPathPrefix = "/@daemon/"

// PathConfig is GET, and answers a ConfigLocation: where this daemon keeps configs, and how the toggle
// stands. A command asks it before asking for a toggle, to find a daemon whose environment names another
// directory than the shell's before that daemon splits a directory no client reads.
const PathConfig = LocalPathPrefix + "config"

// ConfigLocation is the answer to PathConfig.
type ConfigLocation struct {
	// Dir is the directory the daemon reads and writes config.toml in.
	Dir string `json:"dir"`
	// Split is the same-machine toggle.
	Split bool `json:"split"`
}

// The requests that change the toggle. Both are POST with no body, and both answer a ConfigToggle.
const (
	// PathConfigSplit turns the same-machine config toggle on: each client gets its own config file,
	// starting as a copy of config.toml.
	PathConfigSplit = LocalPathPrefix + "config/split"
	// PathConfigUnsplit turns it off, folding the two files back onto config.toml.
	PathConfigUnsplit = LocalPathPrefix + "config/unsplit"
)

// ConfigToggle is what a split or an unsplit did.
type ConfigToggle struct {
	// Split is the toggle as it now stands.
	Split bool `json:"split"`
	// Files are the config files now in use: one when not split, the two clients' when split.
	Files []string `json:"files"`
	// Base is the file an unsplit took as its starting point, the more recently written of the two, or
	// empty for a split.
	Base string `json:"base"`
	// Merged lists the keys an unsplit added to the base from the other file.
	Merged []string `json:"merged"`
	// Kept lists the keys both files set differently, where the base's value stayed.
	Kept []string `json:"kept"`
	// Skipped lists what the other file held that this version could not carry over, each with its reason.
	Skipped []string `json:"skipped"`
	// Backups are the files an unsplit set aside rather than deleted.
	Backups []string `json:"backups"`
}

// The local ops, numbered from 100. HELLO, IDENTIFY and DISPATCH keep the gateway's numbers.
const (
	// OpRequest asks the daemon to perform a REST call. Client to daemon.
	OpRequest gatewayproto.Opcode = 100
	// OpResponse answers one, by the request's id. Daemon to client.
	OpResponse gatewayproto.Opcode = 101
	// OpClose is the daemon's last frame on a connection it is closing. Daemon to client.
	OpClose gatewayproto.Opcode = 102
)

// Close codes. The gateway's number wherever the meaning is the same, so one table reads both; the local
// cases are numbered from 4100, clear of anything the gateway assigns.
const (
	// CloseGoingAway: the daemon is stopping. Attach again once it is back.
	CloseGoingAway = 1001
	// CloseUnknownOpcode: an op a client may not send.
	CloseUnknownOpcode = gatewayproto.CloseUnknownOpcode
	// CloseDecodeError: a frame that is not valid, or too large.
	CloseDecodeError = gatewayproto.CloseDecodeError
	// CloseNotIdentified: anything but IDENTIFY first.
	CloseNotIdentified = gatewayproto.CloseNotAuthenticated
	// CloseAlreadyIdentified: a second IDENTIFY.
	CloseAlreadyIdentified = gatewayproto.CloseAlreadyAuthenticated
	// CloseVersionMismatch: the two versions are incompatible under ADR 0033; the reason names which to
	// upgrade.
	CloseVersionMismatch = gatewayproto.CloseVersionMismatch
	// CloseTooSlow: the client stopped reading and its buffer filled. It was dropped rather than allowed to
	// stall the daemon; attach again and resync.
	CloseTooSlow = gatewayproto.CloseTooSlow
	// CloseResync: the daemon's own session started afresh or its sign-in ended, and the state the client's
	// events were building was cleared with it. Attach again for a new READY. Sent only to clients that
	// asked for events, since nothing else holds a view to resync.
	CloseResync = 4100
	// CloseTooManyClients: the daemon is already serving MaxClients.
	CloseTooManyClients = 4101
)

// Size bounds, in bytes, on one frame's JSON.
const (
	// MaxClientFrame bounds a frame from an attach client. A request body is at most a message.
	MaxClientFrame = 1 << 20
	// MaxDaemonFrame bounds a frame from the daemon. A relayed response body is capped at MaxResponseBody,
	// and a forwarded dispatch at what the gateway accepted (4 MiB); this leaves room for the envelope.
	MaxDaemonFrame = 16 << 20
	// MaxResponseBody bounds a body the relay returns. A page of messages is a few hundred kilobytes.
	MaxResponseBody = 8 << 20
)

// MaxClients is how many attach clients the daemon serves at once: generous for one person, and far below
// the raised open-file limit.
const MaxClients = 64

// MaxInFlight is how many requests one connection may have outstanding.
const MaxInFlight = 16

// Hello is op 10's payload, sent by the daemon on connect.
type Hello struct {
	// Version is the daemon's release version, or "dev".
	Version string `json:"version"`
}

// Identify is op 2's payload, the client's first frame.
type Identify struct {
	Properties gatewayproto.IdentifyProperties `json:"properties"`
	// Events asks for the daemon's DISPATCH stream. A one-shot command wants its request answered and
	// nothing else; a TUI wants the stream.
	Events bool `json:"events"`
}

// The daemon's standings, as READY reports them.
const (
	// StandingSignedIn: the daemon holds a usable session.
	StandingSignedIn = "signed_in"
	// StandingStarting: a sign-in is being established — the store not yet read, a keyring not yet unlocked,
	// a token being obtained. A request now waits a bounded while for it rather than failing.
	StandingStarting = "starting"
	// StandingSignedOut: nobody is signed in, and nothing will change until somebody runs `norite login`.
	StandingSignedOut = "signed_out"
)

// Ready is the payload of the first dispatch, READY, which answers IDENTIFY.
type Ready struct {
	// Standing is whether the daemon is signed in. Not inferred from Account: a daemon still starting has no
	// account to name yet and is not signed out, and telling the two apart is what stops a client sending
	// somebody to `norite login` a moment after a restart (M20 /code-review).
	Standing string `json:"standing"`
	// Account is who the daemon is signed in as, or nil when nobody is, or nobody is yet.
	Account *Account `json:"account"`
	// Guilds are the summaries the daemon holds from its gateway connection, for the sign-in Account names:
	// empty before the gateway's own READY, and for a daemon nobody is signed in to. Never null.
	Guilds []GuildSummary `json:"guilds"`
}

// Account is the signed-in account, as the daemon's credential names it.
type Account struct {
	InstanceURL string `json:"instance_url"`
	UserID      string `json:"user_id"`
	// Username is sanitized (termsafe.Text): it was read from a file a person can edit.
	Username string `json:"username"`
}

// GuildSummary is a guild as the daemon's state keeps it. The name is sanitized and cut at the server's
// own limit, which is what the state stores (daemon/internal/state); a client wanting the guild as the
// instance holds it asks over the relay.
type GuildSummary struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	OwnerID string `json:"owner_id"`
}

// Request is op 100's payload.
type Request struct {
	// ID is chosen by the client and echoed in the response, so a connection can have several in flight.
	ID string `json:"id"`
	// Method is one of GET, POST, PUT, PATCH and DELETE.
	Method string `json:"method"`
	// Path is under /api/v1, which the daemon prefixes: "/guilds/123", with any query string.
	Path string `json:"path"`
	// Body is the JSON to send, or null for none.
	Body json.RawMessage `json:"body"`
}

// Response is op 101's payload. Exactly one of Status and Error is set: Status when the instance answered,
// whatever it answered, and Error when the daemon could not ask it.
type Response struct {
	ID string `json:"id"`
	// Status is the instance's HTTP status, or nil when Error is set.
	Status *int `json:"status"`
	// Body is the instance's JSON body, or null when it sent none — or sent something that is not JSON,
	// which a proxy in front of an instance does when it fails.
	Body json.RawMessage `json:"body"`
	// Error says why the daemon could not perform the request, or is nil.
	Error *RelayError `json:"error"`
}

// RelayError codes.
const (
	// RelayNotSignedIn: the daemon holds no live sign-in.
	RelayNotSignedIn = "not_signed_in"
	// RelayUnreachable: the instance could not be reached, or did not answer in time.
	RelayUnreachable = "unreachable"
	// RelayRefused: the daemon will not make this request — a path outside what the relay may reach, or one
	// that could leave the instance.
	RelayRefused = "refused"
	// RelayBadRequest: the request frame itself is malformed — an unknown method, a duplicate id.
	RelayBadRequest = "bad_request"
	// RelayTooManyRequests: the connection already has MaxInFlight requests outstanding.
	RelayTooManyRequests = "too_many_requests"
	// RelayTooLarge: the instance's answer exceeded MaxResponseBody.
	RelayTooLarge = "too_large"
	// RelayConflict: a request to the daemon itself (LocalPathPrefix) that it understood and did not carry
	// out, because of what it found: the toggle already where it was asked to go, a file that does not
	// parse. Nothing was changed, and the message says what to do.
	RelayConflict = "conflict"
	// RelayFailed: a request to the daemon itself that it tried and could not complete.
	RelayFailed = "failed"
)

// ValidMethod reports whether a request may use method: the five the REST API uses, spelled in capitals.
// One list for the attach socket, the automation port and the command that speaks to it.
func ValidMethod(method string) bool {
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
		return true
	}
	return false
}

// Failure is a Response saying why the daemon did not perform a request. Its ID is the caller's to set.
func Failure(code, msg string) Response {
	return Response{Error: &RelayError{Code: code, Message: msg}}
}

// RelayError is why the daemon could not perform a request. Its message is the daemon's own wording, never
// the instance's.
type RelayError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *RelayError) Error() string { return e.Message }

// Close is op 102's payload.
type Close struct {
	Code   int    `json:"code"`
	Reason string `json:"reason"`
}

// CloseError is a connection the daemon closed, with its code and reason.
type CloseError struct {
	Code   int
	Reason string
}

func (e *CloseError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("the daemon closed the connection (%d)", e.Code)
	}
	return fmt.Sprintf("the daemon closed the connection (%d): %s", e.Code, e.Reason)
}
