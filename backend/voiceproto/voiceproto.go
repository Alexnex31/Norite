// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package voiceproto is the protocol a voice connection speaks on its data channel (M26): the slot map,
// subscription and the active speaker, plus what the offer must look like for the SFU to admit it.
//
// It lives outside backend/internal for gatewayproto's reason: the voice-worker (M28–M29) speaks exactly
// this protocol, from another module, and two copies of a wire format drift. Everything that decides
// anything (who is admitted, who fills a slot) stays in backend/internal/voice.
//
// contracts/voice-datachannel.schema.json is the source of truth for every message. This package is its Go
// form, and the SFU's tests validate every message both ends send against that document.
//
// Decoding is strict: an unknown field is refused, so a field added to a message is a MINOR change and
// never a PATCH one, the attach socket's rule (M20). The hello's version check is what turns a mismatch
// into a reason a person can act on rather than a refusal for malformed input.
package voiceproto

// MaxOffer is the largest SDP offer the SFU accepts, in bytes. Op 4 carries the offer over the gateway from
// M33, whose inbound frame is 16 KiB. JSON escapes each line's CRLF as four bytes, about a twentieth more
// on an SDP's short lines, and 12 KiB leaves that and the frame's other fields room.
const MaxOffer = 12 << 10

// MaxCandidates is the most ICE candidate lines an offer may carry, counted across every media section (M26).
// The SFU refuses an offer past it, and the voice-worker trims what it gathered to it (M28): a machine with
// a few interfaces in both families, docker's bridges or a VPN among them, gathers more.
const MaxCandidates = 16

// Kinds of track. A connection's offer has its sending sections and its receiving slots counted per kind.
const (
	KindAudio = "audio"
	KindVideo = "video"
)

// Label is the data channel's label. A connection has exactly one data channel and it carries this label;
// the SFU closes any other the peer opens.
const Label = "norite-voice"

// AudioLevelURI is RFC 6464's header extension, the one extension the SFU reads and the one it forwards.
const AudioLevelURI = "urn:ietf:params:rtp-hdrext:ssrc-audio-level"

// ClockRateAudio is Opus's RTP clock, the only audio codec at M26.
const ClockRateAudio = 48000

// MaxMessage is the largest data-channel message either end sends or accepts, in bytes. The largest is a
// subscribe naming every participant of a full room, a few kilobytes.
const MaxMessage = 8 << 10

// MaxSubscribeList is the most users a subscribe's pin or drop names: a full room's.
const MaxSubscribeList = 64
