// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pion/sdp/v3"

	"github.com/Alexnex31/Norite/backend/voiceproto"
)

// ErrBadOffer is an offer the SFU will not answer. Its message says which rule the offer broke, in words
// for whoever wrote the client; it carries nothing from the offer itself.
var ErrBadOffer = errors.New("voice: the offer is not one the SFU admits")

// offerShape is what a valid offer asks for, read off its structure.
type offerShape struct {
	// sends counts the media sections the client sends on, per kind; slots counts those it receives on.
	sends map[string]int
	slots map[string]int
	// slotMids are the receiving sections' mids, per kind, in the order they appear.
	slotMids map[string][]string
}

// readOffer checks an offer's structure against the limits and returns what it asks for.
//
// It is strict about what the SFU relies on and indifferent to the rest. What the SFU relies on: the size,
// which sections there are and in which direction, one bundle, one fingerprint, and that the DTLS role is
// the SFU's to choose. What it ignores: attributes it never acts on, such as a browser's extra codecs,
// msid or extmap-allow-mixed. Ignoring them is safe because the answer is Pion's, built from the SFU's own
// configuration, and no attribute of the offer is copied into it unread.
func readOffer(offer string, lim Limits) (offerShape, error) {
	if len(offer) > voiceproto.MaxOffer {
		return offerShape{}, fmt.Errorf("%w: it is over %d bytes", ErrBadOffer, voiceproto.MaxOffer)
	}
	var desc sdp.SessionDescription
	if err := desc.UnmarshalString(offer); err != nil {
		return offerShape{}, fmt.Errorf("%w: it is not SDP", ErrBadOffer)
	}

	shape := offerShape{sends: map[string]int{}, slots: map[string]int{}, slotMids: map[string][]string{}}
	fingerprints := map[string]bool{}
	ufrags := map[string]bool{}
	candidates := 0
	setups := 0
	apps := 0
	mids := map[string]bool{}

	note := func(attrs []sdp.Attribute) error {
		for _, a := range attrs {
			switch a.Key {
			case "fingerprint":
				fingerprints[strings.ToLower(a.Value)] = true
			case "setup":
				setups++
				if a.Value != "actpass" {
					return fmt.Errorf("%w: the DTLS role must be the SFU's to choose (a=setup:actpass)", ErrBadOffer)
				}
			case "ice-ufrag":
				ufrags[a.Value] = true
			case "candidate":
				candidates++
			}
		}
		return nil
	}
	if err := note(desc.Attributes); err != nil {
		return offerShape{}, err
	}

	if len(desc.MediaDescriptions) > lim.maxSections() {
		return offerShape{}, fmt.Errorf("%w: it has more media sections than any connection may", ErrBadOffer)
	}
	for _, m := range desc.MediaDescriptions {
		if err := note(m.Attributes); err != nil {
			return offerShape{}, err
		}
		mid, ok := m.Attribute("mid")
		if !ok || mid == "" || mids[mid] {
			return offerShape{}, fmt.Errorf("%w: every section needs its own mid", ErrBadOffer)
		}
		mids[mid] = true
		if _, ok := m.Attribute("rtcp-mux"); !ok && m.MediaName.Media != "application" {
			return offerShape{}, fmt.Errorf("%w: RTCP must be multiplexed (a=rtcp-mux)", ErrBadOffer)
		}

		switch kind := m.MediaName.Media; kind {
		case "application":
			if len(m.MediaName.Formats) != 1 || m.MediaName.Formats[0] != "webrtc-datachannel" {
				return offerShape{}, fmt.Errorf("%w: the application section must be a data channel", ErrBadOffer)
			}
			apps++
		case voiceproto.KindAudio, voiceproto.KindVideo:
			switch direction(m) {
			case "sendonly":
				shape.sends[kind]++
			case "recvonly":
				shape.slots[kind]++
				shape.slotMids[kind] = append(shape.slotMids[kind], mid)
			default:
				return offerShape{}, fmt.Errorf("%w: a %s section must be sendonly or recvonly", ErrBadOffer, kind)
			}
		default:
			return offerShape{}, fmt.Errorf("%w: it has a section of a kind the SFU does not carry", ErrBadOffer)
		}
	}

	if apps != 1 {
		return offerShape{}, fmt.Errorf("%w: it needs exactly one data channel", ErrBadOffer)
	}
	for _, kind := range []string{voiceproto.KindAudio, voiceproto.KindVideo} {
		b := lim.Tracks[kind]
		if shape.sends[kind] > b.Send {
			return offerShape{}, fmt.Errorf("%w: it sends more %s tracks than a connection may (%d)",
				ErrBadOffer, kind, b.Send)
		}
		if shape.slots[kind] > b.Slots {
			return offerShape{}, fmt.Errorf("%w: it asks for more %s slots than a connection may (%d)",
				ErrBadOffer, kind, b.Slots)
		}
	}
	if shape.sends[voiceproto.KindAudio] != 1 || shape.slots[voiceproto.KindAudio] < 1 {
		return offerShape{}, fmt.Errorf("%w: it must send one audio track and receive on at least one slot",
			ErrBadOffer)
	}
	if len(fingerprints) != 1 {
		return offerShape{}, fmt.Errorf("%w: it must carry exactly one DTLS fingerprint", ErrBadOffer)
	}
	for f := range fingerprints {
		if !strings.HasPrefix(f, "sha-256 ") {
			return offerShape{}, fmt.Errorf("%w: the fingerprint must be SHA-256", ErrBadOffer)
		}
	}
	// Present, not only never wrong: an offer naming no role leaves Pion to assume one.
	if setups == 0 {
		return offerShape{}, fmt.Errorf("%w: the DTLS role must be the SFU's to choose (a=setup:actpass)", ErrBadOffer)
	}
	if len(ufrags) != 1 {
		return offerShape{}, fmt.Errorf("%w: it must carry exactly one ICE username", ErrBadOffer)
	}
	if candidates > lim.MaxCandidates {
		return offerShape{}, fmt.Errorf("%w: it has more than %d candidates", ErrBadOffer, lim.MaxCandidates)
	}
	if !bundlesAll(&desc, mids) {
		return offerShape{}, fmt.Errorf("%w: every section must be in one bundle", ErrBadOffer)
	}
	return shape, nil
}

// direction is a media section's direction attribute, sendrecv when it states none (RFC 4566).
func direction(m *sdp.MediaDescription) string {
	for _, a := range m.Attributes {
		switch a.Key {
		case "sendonly", "recvonly", "sendrecv", "inactive":
			return a.Key
		}
	}
	return "sendrecv"
}

// bundlesAll says whether one BUNDLE group names exactly the offer's sections. The SFU has one port and one
// transport per connection, so a section outside the bundle is one it cannot carry.
func bundlesAll(desc *sdp.SessionDescription, mids map[string]bool) bool {
	groups := 0
	ok := false
	for _, a := range desc.Attributes {
		if a.Key != "group" {
			continue
		}
		groups++
		fields := strings.Fields(a.Value)
		if len(fields) == 0 || fields[0] != "BUNDLE" || len(fields)-1 != len(mids) {
			continue
		}
		seen := map[string]bool{}
		for _, mid := range fields[1:] {
			if !mids[mid] || seen[mid] {
				return false
			}
			seen[mid] = true
		}
		ok = true
	}
	return ok && groups == 1
}
