// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/internal/voice/turn"
)

// StackOptions configure the SFU and TURN together, as norite-server and the harness both start them.
type StackOptions struct {
	// MediaListen is the SFU's UDP socket; Public what it announces instead of the bound address.
	MediaListen string
	Public      []netip.Addr

	// TURNListen is TURN's UDP and TCP address. TURNTLSListen, with a certificate's two files, is TURN
	// over TLS; empty for none.
	TURNListen    string
	TURNTLSListen string
	TLSCert       string
	TLSKey        string

	// Relay is where relay sockets are bound, SFU where they reach the SFU, and RelayPorts their range.
	Relay      netip.Addr
	SFU        netip.AddrPort
	RelayPorts [2]uint16

	// RelayPerAddress is how many allocations, and how many TCP and TLS connections, one address may
	// hold on TURN, an IPv6 /64 being one; zero for TURN's default.
	RelayPerAddress int

	// SigningKey is the instance's, which TURN's secret is derived from.
	SigningKey []byte

	// Loopback admits loopback candidates, for the harness on one machine.
	Loopback bool
	Version  string
	Logger   zerolog.Logger
}

// Stack is a running SFU and the TURN server that relays to it.
type Stack struct {
	SFU    *SFU
	TURN   *turn.Server
	Secret []byte
}

// StartStack binds every port and starts both, or starts nothing: a port already in use is reported here,
// at startup, and nothing is left listening.
func StartStack(opts StackOptions) (_ *Stack, err error) {
	var closers []func() error
	defer func() {
		if err != nil {
			for _, c := range closers {
				_ = c()
			}
		}
	}()

	secret, err := turn.DeriveSecret(opts.SigningKey)
	if err != nil {
		return nil, err
	}
	media, err := net.ListenPacket("udp", opts.MediaListen)
	if err != nil {
		return nil, fmt.Errorf("voice: the media port: %w", err)
	}
	closers = append(closers, media.Close)
	// A port of zero is one the system chose, which only the socket knows.
	if opts.SFU.Port() == 0 {
		opts.SFU = netip.AddrPortFrom(opts.SFU.Addr(), media.LocalAddr().(*net.UDPAddr).AddrPort().Port())
	}
	udp, err := net.ListenPacket("udp", opts.TURNListen)
	if err != nil {
		return nil, fmt.Errorf("voice: TURN's UDP port: %w", err)
	}
	closers = append(closers, udp.Close)
	tcp, err := net.Listen("tcp", opts.TURNListen)
	if err != nil {
		return nil, fmt.Errorf("voice: TURN's TCP port: %w", err)
	}
	closers = append(closers, tcp.Close)

	// A client names the SFU by what it announces: its public addresses, or the address it binds, or with
	// neither every address of the host's, which TURN reads for itself.
	announced := opts.Public
	if bound := media.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap(); len(announced) == 0 && !bound.IsUnspecified() {
		announced = []netip.Addr{bound}
	}
	limits := turn.DefaultLimits()
	if opts.RelayPerAddress > 0 {
		limits.AllocationsPerSource, limits.StreamsPerSource = opts.RelayPerAddress, opts.RelayPerAddress
	}
	topts := turn.Options{
		UDP:        udp,
		TCP:        tcp,
		Secret:     secret,
		Relay:      opts.Relay,
		RelayPorts: opts.RelayPorts,
		SFU:        opts.SFU,
		Announced:  announced,
		Limits:     limits,
		Logger:     opts.Logger,
	}
	if opts.TLSCert != "" {
		if opts.TURNTLSListen == "" {
			return nil, errors.New("voice: a certificate for TURN, and no address for TLS")
		}
		config, err := turn.CertificateFiles(opts.TLSCert, opts.TLSKey, opts.Logger)
		if err != nil {
			return nil, err
		}
		ln, err := net.Listen("tcp", opts.TURNTLSListen)
		if err != nil {
			return nil, fmt.Errorf("voice: TURN's TLS port: %w", err)
		}
		closers = append(closers, ln.Close)
		topts.TLS, topts.TLSConfig = ln, config
	}

	sfu, err := New(Options{
		Conn:     media,
		Loopback: opts.Loopback,
		Public:   opts.Public,
		Relay:    opts.Relay,
		Version:  opts.Version,
		Logger:   opts.Logger,
	})
	if err != nil {
		return nil, err
	}
	closers = append(closers, sfu.Close)
	srv, err := turn.New(topts)
	if err != nil {
		return nil, err
	}
	return &Stack{SFU: sfu, TURN: srv, Secret: secret}, nil
}

// closeGrace is how long TURN stays up after the SFU has closed, while it holds an allocation: pion
// relays what the SFU sent on a goroutine of its own, which a loaded machine may not have run yet.
const closeGrace = 250 * time.Millisecond

// Close stops the SFU, then TURN: what the SFU sends a relayed client as it closes the connection goes
// through the relay, which must still be there to carry it.
func (s *Stack) Close() error {
	err := s.SFU.Close()
	if s.TURN.AllocationCount() > 0 {
		time.Sleep(closeGrace)
	}
	return errors.Join(err, s.TURN.Close())
}
