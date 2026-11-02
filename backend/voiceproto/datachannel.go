// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
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

// MaxVersion is the longest version a hello may name, as the contract says.
const MaxVersion = 64

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

// Decode decodes b into v strictly, as the contract reads: one JSON value, no trailing data, and in every
// object each key spelled exactly as a field's, every field present that is not omitempty, and no null.
//
// encoding/json alone is lenient three ways the contract is not: it matches keys whatever their case, it
// leaves a missing field at its zero value, and it takes null for an array as an empty one. A client
// written against this decoder would come to rely on all three, and another implementation of the
// contract would refuse it. What a value means (an id's digits, an op's name) is the reader's to check.
func Decode(b []byte, v any) error {
	if len(b) > MaxMessage {
		return fmt.Errorf("%w: %d bytes", ErrMalformed, len(b))
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	// Not dec.More, which reports false before a ']' or a '}' and so let `{...}]x` through.
	if len(bytes.TrimSpace(b[dec.InputOffset():])) != 0 {
		return fmt.Errorf("%w: trailing data", ErrMalformed)
	}
	if err := shape(json.RawMessage(b), reflect.TypeOf(v)); err != nil {
		return fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	return nil
}

// shape checks raw against t as Decode describes. raw has already decoded into t, so only what
// encoding/json lets through is left to find.
func shape(raw json.RawMessage, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("null where a value belongs")
	}
	switch t.Kind() {
	case reflect.Struct:
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return err
		}
		known := 0
		for i := range t.NumField() {
			f := t.Field(i)
			name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
			if !f.IsExported() || name == "-" || name == "" {
				continue
			}
			val, ok := obj[name]
			if !ok {
				if opts != "omitempty" {
					return fmt.Errorf("%q is missing", name)
				}
				continue
			}
			known++
			if err := shape(val, f.Type); err != nil {
				return fmt.Errorf("%q: %w", name, err)
			}
		}
		// A key matched only without regard to case decoded into a field, and is not one of the above.
		if known != len(obj) {
			return errors.New("a key is not spelled as the contract spells it")
		}
	case reflect.Slice:
		var elems []json.RawMessage
		if err := json.Unmarshal(raw, &elems); err != nil {
			return err
		}
		for _, e := range elems {
			if err := shape(e, t.Elem()); err != nil {
				return err
			}
		}
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
