// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Ops name each message. Hello and Subscribe go from the client; the rest from the SFU.
const (
	OpHello     = "hello"
	OpReady     = "ready"
	OpSlots     = "slots"
	OpSpeaking  = "speaking"
	OpSubscribe = "subscribe"
	OpRefused   = "refused"
)

// Hello is the client's first message, naming its version. Anything else first is a protocol error.
type Hello struct {
	Op      string `json:"op"`
	Version string `json:"version"`
}

// Ready answers a hello the SFU accepts: its version, and which of the connection's receiving sections is
// which slot.
type Ready struct {
	Op      string      `json:"op"`
	Version string      `json:"version"`
	Slots   []SlotIdent `json:"slots"`
}

// SlotIdent names one receiving slot: its index among the connection's slots of its kind, and the mid of
// the offer's media section it is.
type SlotIdent struct {
	Slot int    `json:"slot"`
	Kind string `json:"kind"`
	Mid  string `json:"mid"`
}

// Slots is the whole slot map, sent whenever it changes. A slot not listed is empty.
//
// The map says who a source is. Which packets are whose is the CSRC each forwarded packet carries, because
// the data channel and the media are separate transports and neither is ordered against the other.
type Slots struct {
	Op  string      `json:"op"`
	Map []SlotEntry `json:"map"`
}

// SlotEntry is one occupied slot.
type SlotEntry struct {
	Slot int    `json:"slot"`
	Kind string `json:"kind"`
	// Source is the CSRC the SFU puts in every packet it forwards from this sender's track.
	Source uint32 `json:"source"`
	User   string `json:"user"`
	Conn   string `json:"conn"`
}

// Speaking lists who is speaking now, sent whenever it changes.
type Speaking struct {
	Op    string   `json:"op"`
	Users []string `json:"users"`
}

// Subscribe replaces the connection's subscription whole. Pin is served first, in order; Drop is never
// served; Cap, when set, is fewer slots than the connection has. The server's own exclusion is applied
// before any of it, and a pin cannot override it.
type Subscribe struct {
	Op   string   `json:"op"`
	Pin  []string `json:"pin"`
	Drop []string `json:"drop"`
	Cap  *int     `json:"cap,omitempty"`
}

// Refused is the SFU's last message before it closes the connection, saying why.
type Refused struct {
	Op     string `json:"op"`
	Reason string `json:"reason"`
}

// ErrMalformed is any message that is not one of the above, exactly.
var ErrMalformed = errors.New("voiceproto: malformed message")

// OpOf reads a message's op without decoding the rest.
func OpOf(b []byte) (string, error) {
	var head struct {
		Op string `json:"op"`
	}
	if err := json.Unmarshal(b, &head); err != nil || head.Op == "" {
		return "", ErrMalformed
	}
	return head.Op, nil
}

// Decode decodes b into v strictly: one JSON value, no unknown field, no trailing data.
func Decode(b []byte, v any) error {
	if len(b) > MaxMessage {
		return fmt.Errorf("%w: %d bytes", ErrMalformed, len(b))
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	if dec.More() {
		return fmt.Errorf("%w: trailing data", ErrMalformed)
	}
	return nil
}

// Encode encodes a message. It does not escape HTML, which would grow a message by bytes nobody needs.
func Encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
