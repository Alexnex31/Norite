// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package turn

import (
	"errors"
	"time"
)

// Limits bound what a stranger can make TURN hold or send. Every one has a test that plays the peer.
type Limits struct {
	// Allocations is how many allocations the server holds in all, AllocationsPerUser how many one
	// account's credentials hold (one per transport a client tries, and a retry), and
	// AllocationsPerSource how many one address holds, an IPv6 /64 being one address: an office behind
	// one NAT is many accounts at one source.
	Allocations          int
	AllocationsPerUser   int
	AllocationsPerSource int

	// Streams is how many TCP and TLS connections the server holds in all, StreamsPerSource how many from
	// one address. A connection that has made no allocation after AuthenticateWithin is closed, and one
	// that has sent nothing for Idle is: a call sends ICE consent every few seconds and media far more
	// often. HandshakeWithin bounds a TLS handshake.
	Streams            int
	StreamsPerSource   int
	AuthenticateWithin time.Duration
	HandshakeWithin    time.Duration

	// Idle closes a relay the SFU has sent nothing through for that long: one no connection uses, or
	// whose connection the SFU closed. Its allocation goes with it.
	Idle time.Duration

	// RelayPacketsPerSecond and RelayBitsPerSecond bound what one allocation relays towards the SFU,
	// with M26's strikes: past the bound is dropped, past twice it for StrikesToClose seconds in a row
	// closes the relay. A relayed client sends everything through it, media, RTCP, the data channel and
	// ICE, so the bound is a connection's, not a track's.
	RelayPacketsPerSecond int
	RelayBitsPerSecond    int
	StrikesToClose        int

	// RepliesPerSecond bounds what TURN answers without authentication, to one address and in all:
	// Binding responses and the challenges that start an allocation. Over UDP the source can be forged,
	// and each reply is larger than the request that drew it (security ledger).
	RepliesPerSecond      int
	RepliesTotalPerSecond int
}

// DefaultLimits are the bounds a server starts with.
func DefaultLimits() Limits {
	return Limits{
		Allocations:           1024,
		AllocationsPerUser:    4,
		AllocationsPerSource:  32,
		Streams:               1024,
		StreamsPerSource:      32,
		AuthenticateWithin:    10 * time.Second,
		HandshakeWithin:       5 * time.Second,
		Idle:                  30 * time.Second,
		RelayPacketsPerSecond: 600,
		RelayBitsPerSecond:    2_000_000,
		StrikesToClose:        3,
		RepliesPerSecond:      20,
		RepliesTotalPerSecond: 2000,
	}
}

func (l Limits) validate() error {
	for _, n := range []int{
		l.Allocations, l.AllocationsPerUser, l.AllocationsPerSource, l.Streams, l.StreamsPerSource,
		l.RelayPacketsPerSecond, l.RelayBitsPerSecond, l.StrikesToClose, l.RepliesPerSecond,
		l.RepliesTotalPerSecond,
	} {
		if n <= 0 {
			return errors.New("turn: a limit is zero or negative, which admits nobody")
		}
	}
	for _, d := range []time.Duration{l.AuthenticateWithin, l.HandshakeWithin, l.Idle} {
		if d <= 0 {
			return errors.New("turn: a deadline is zero or negative")
		}
	}
	return nil
}
