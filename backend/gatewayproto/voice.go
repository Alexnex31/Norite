// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package gatewayproto

import (
	"encoding/json"
	"errors"
	"regexp"
)

// MaxVoiceOffer bounds op 4's offer: M26's 12 KiB, which keeps the frame under the gateway's 16 KiB.
const MaxVoiceOffer = 12 << 10

// VoiceStateUpdate is op 4 (M33): a join, carrying the voice-worker's offer, or a leave, whose ChannelID is
// nil. Decode it with DecodeVoiceStateUpdate, which tells the two apart by what the frame names rather
// than by what a struct left empty.
type VoiceStateUpdate struct {
	Nonce         string  `json:"nonce,omitempty"`
	ChannelID     *string `json:"channel_id"`
	Offer         string  `json:"offer,omitempty"`
	SupportsVideo *bool   `json:"supports_video,omitempty"`
}

// Leave reports whether the frame leaves the session's call.
func (v VoiceStateUpdate) Leave() bool { return v.ChannelID == nil }

var nonceShape = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// ValidNonce reports whether s may name a voice request: 1 to 32 of [A-Za-z0-9_-]. It is echoed back in a
// reply, so it is held to a shape nothing can be smuggled in.
func ValidNonce(s string) bool { return nonceShape.MatchString(s) }

var digits = regexp.MustCompile(`^[0-9]{1,20}$`)

// ErrBadVoiceStateUpdate is a frame op 4's contract refuses.
var ErrBadVoiceStateUpdate = errors.New("gatewayproto: not a voice state update")

// DecodeVoiceStateUpdate decodes op 4's d strictly, as gateway-events.schema.json shapes it: a join names
// all four fields and a leave names channel_id alone, as null. A frame naming nothing is neither, rather
// than a leave by omission.
func DecodeVoiceStateUpdate(raw json.RawMessage, strict func([]byte, any) error) (VoiceStateUpdate, error) {
	var fields map[string]json.RawMessage
	if err := strict(raw, &fields); err != nil {
		return VoiceStateUpdate{}, ErrBadVoiceStateUpdate
	}
	if _, ok := fields["channel_id"]; !ok {
		return VoiceStateUpdate{}, ErrBadVoiceStateUpdate
	}
	var v VoiceStateUpdate
	if err := strict(raw, &v); err != nil {
		return VoiceStateUpdate{}, ErrBadVoiceStateUpdate
	}
	if v.Leave() {
		if len(fields) != 1 {
			return VoiceStateUpdate{}, ErrBadVoiceStateUpdate
		}
		return v, nil
	}
	if !ValidNonce(v.Nonce) || !digits.MatchString(*v.ChannelID) || v.Offer == "" ||
		len(v.Offer) > MaxVoiceOffer || v.SupportsVideo == nil {
		return VoiceStateUpdate{}, ErrBadVoiceStateUpdate
	}
	return v, nil
}

// VoiceServer is VOICE_SERVER_UPDATE's d: the SFU's answer, and what the worker gathers with.
type VoiceServer struct {
	Nonce        string      `json:"nonce"`
	GuildID      string      `json:"guild_id"`
	ChannelID    string      `json:"channel_id"`
	ConnectionID string      `json:"connection_id"`
	Answer       string      `json:"answer"`
	ICEServers   []ICEServer `json:"ice_servers"`
	Bitrate      int         `json:"bitrate"`
	Mute         bool        `json:"mute"`
	Deaf         bool        `json:"deaf"`
	Suppress     bool        `json:"suppress"`
}

// ICEServer is TURN as a client is told about it, with the account's credential.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
}

// VoiceJoinRefused is VOICE_JOIN_REFUSED's d.
type VoiceJoinRefused struct {
	Nonce     string `json:"nonce"`
	ChannelID string `json:"channel_id"`
	Code      string `json:"code"`
}

// What VOICE_JOIN_REFUSED's code may be. A fixed vocabulary, never prose (M8): the client words each.
const (
	VoiceNotFound    = "not_found"
	VoiceNotVoice    = "not_voice"
	VoiceForbidden   = "forbidden"
	VoiceFull        = "full"
	VoiceBadOffer    = "bad_offer"
	VoiceBusy        = "busy"
	VoiceRateLimited = "rate_limited"
	VoiceDisabled    = "voice_disabled"
	VoiceUnavailable = "unavailable"
)

// The voice dispatches' t. The first two answer one session's op 4 and nothing else receives them; the
// last two reach the channel's viewers.
const (
	EventVoiceServerUpdate = "VOICE_SERVER_UPDATE"
	EventVoiceJoinRefused  = "VOICE_JOIN_REFUSED"
	EventVoiceStateUpdate  = "VOICE_STATE_UPDATE"
	EventVoiceStateDelete  = "VOICE_STATE_DELETE"
)

// VoiceRefusalCodes is every code, for the tests holding them to the contract.
func VoiceRefusalCodes() []string {
	return []string{VoiceNotFound, VoiceNotVoice, VoiceForbidden, VoiceFull, VoiceBadOffer, VoiceBusy,
		VoiceRateLimited, VoiceDisabled, VoiceUnavailable}
}
