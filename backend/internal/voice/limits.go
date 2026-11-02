// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"time"

	"github.com/Alexnex31/Norite/backend/voiceproto"
)

// Limits bounds everything a stranger's connection makes the SFU hold or do. Every participant sends RTP,
// RTCP and SCTP the SFU parses, and in public matchmaking that is anybody, so each bound is set against a
// hostile peer rather than a correct client, the rule M19 set for the daemon (finding 5 of M26's plan).
type Limits struct {
	// Rooms is the most rooms one SFU holds, ParticipantsPerRoom the most connections in one, and
	// ConnsPerUser the most connections one user holds in one room (a second machine, M105's video).
	Rooms               int
	ParticipantsPerRoom int
	ConnsPerUser        int

	// Tracks bounds, per kind, how many tracks a connection sends and how many slots it receives on.
	// Video is zero until M105–M107 raise it.
	Tracks map[string]TrackBounds

	// MaxCandidates bounds the ICE candidates an offer carries.
	MaxCandidates int

	// ProvisionTimeout is how long a provision waits for its connection. It also bounds how long an
	// eviction's cutoff is remembered: a provision older than this is refused for its age alone.
	ProvisionTimeout time.Duration

	// PacketsPerSecond and BitsPerSecond bound what one track may send. Above either, packets are dropped
	// unforwarded and do not count towards the active speaker; StrikesToEvict seconds in a row at more
	// than twice the bound close the connection.
	PacketsPerSecond int
	BitsPerSecond    int
	StrikesToEvict   int

	// RTCPPerSecond bounds the RTCP packets one connection sends, with the same strikes.
	RTCPPerSecond int

	// MessagesPerSecond bounds data-channel messages; voiceproto.MaxMessage bounds their size.
	MessagesPerSecond int

	// ExtraChannels is how many data channels beyond the first a peer may open, each closed at once,
	// before the connection is closed.
	ExtraChannels int
}

// TrackBounds is one kind's bounds per connection.
type TrackBounds struct {
	Send  int
	Slots int
}

// DefaultLimits are the bounds a server starts with.
//
// The slot bound is a ceiling on what a client may ask for, not what it gets: the client chooses its
// slot count, so changing a default later changes no wire constant. The forwarding benchmark is what sets
// it (bench_test.go).
func DefaultLimits() Limits {
	return Limits{
		Rooms:               1024,
		ParticipantsPerRoom: 64,
		ConnsPerUser:        2,
		Tracks: map[string]TrackBounds{
			voiceproto.KindAudio: {Send: 1, Slots: 16},
			voiceproto.KindVideo: {Send: 0, Slots: 0},
		},
		MaxCandidates:    16,
		ProvisionTimeout: 30 * time.Second,
		// The answer's minptime=10 allows 10 ms frames, 100 a second; half again above that leaves room
		// for a burst after a stall. The bit bound and the strikes, at twice this, are what catch a flood.
		PacketsPerSecond: 150,
		BitsPerSecond:    256_000,
		StrikesToEvict:   3,
		// A client reports on every slot once a second, and its own sender too; M31's congestion
		// feedback adds tens a second more.
		RTCPPerSecond:     100,
		MessagesPerSecond: 20,
		ExtraChannels:     4,
	}
}

// maxSections is the most media sections any valid offer has: every bound filled, plus the data channel.
func (l Limits) maxSections() int {
	n := 1
	for _, b := range l.Tracks {
		n += b.Send + b.Slots
	}
	return n
}
