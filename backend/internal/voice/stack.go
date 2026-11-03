// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
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
	// OtherRelay and OtherSFU are the same in IPv6 when the SFU is reached on loopback in both families,
	// used only if the media socket listens on both and this host can bind ::1.
	Relay      netip.Addr
	SFU        netip.AddrPort
	OtherRelay netip.Addr
	OtherSFU   netip.AddrPort
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
	mediaPort := media.LocalAddr().(*net.UDPAddr).AddrPort().Port()
	if opts.SFU.Port() == 0 {
		opts.SFU = netip.AddrPortFrom(opts.SFU.Addr(), mediaPort)
	}
	if opts.OtherRelay.IsValid() && !dualStack(media) {
		opts.OtherRelay, opts.OtherSFU = netip.Addr{}, netip.AddrPort{}
	}
	if opts.OtherSFU.IsValid() && opts.OtherSFU.Port() == 0 {
		opts.OtherSFU = netip.AddrPortFrom(opts.OtherSFU.Addr(), mediaPort)
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

	bound := media.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap()
	limits := stackLimits(opts.RelayPerAddress)
	topts := turn.Options{
		UDP:        udp,
		TCP:        tcp,
		Secret:     secret,
		Relay:      opts.Relay,
		RelayPorts: opts.RelayPorts,
		SFU:        opts.SFU,
		OtherRelay: opts.OtherRelay,
		OtherSFU:   opts.OtherSFU,
		Announced:  announced(opts.Public, bound),
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
		Relays:   relays(opts.Relay, opts.OtherRelay),
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

// announced is what a client names the SFU by: its public addresses, and the address it binds in a family
// they do not cover, since Pion rewrites only the host candidates a public address is given for. Nothing
// in a family, with the SFU on every interface, is each of the host's addresses, which TURN reads itself.
func announced(public []netip.Addr, bound netip.Addr) []netip.Addr {
	out := slices.Clone(public)
	covered := slices.ContainsFunc(public, func(a netip.Addr) bool { return a.Is4() == bound.Is4() })
	if !bound.IsUnspecified() && !covered {
		out = append(out, bound)
	}
	return out
}

// dualStack is whether the media socket hears both families on loopback: bound to every IPv6 address,
// which Go does with an empty host when the host has IPv6, and with ::1 there to be reached on.
func dualStack(media net.PacketConn) bool {
	if a := media.LocalAddr().(*net.UDPAddr).AddrPort().Addr(); a != netip.IPv6Unspecified() {
		return false
	}
	probe, err := net.ListenPacket("udp6", "[::1]:0")
	if err != nil {
		return false
	}
	_ = probe.Close()
	return true
}

func relays(addrs ...netip.Addr) []netip.Addr {
	var out []netip.Addr
	for _, a := range addrs {
		if a.IsValid() {
			out = append(out, a)
		}
	}
	return out
}

// stackLimits are TURN's, with perAddress for what one address holds. Replies to unauthenticated requests
// follow it up: every client's first Allocate on each transport is answered with a challenge, so an
// office behind one NAT, which is who raises it, would otherwise have them dropped at twenty a second.
func stackLimits(perAddress int) turn.Limits {
	limits := turn.DefaultLimits()
	if perAddress > 0 {
		limits.AllocationsPerSource, limits.StreamsPerSource = perAddress, perAddress
		limits.RepliesPerSecond = max(limits.RepliesPerSecond, perAddress)
	}
	return limits
}

// closeQuiet and closeLimit bound how long TURN stays up after the SFU has closed, while it holds an
// allocation: until the relays have heard nothing for closeQuiet, so that what the SFU sent on its way out
// has been passed on, and closeLimit at most.
const (
	closeQuiet = 100 * time.Millisecond
	closeLimit = time.Second
)

// Close stops the SFU, then TURN: what the SFU sends a relayed client as it closes the connection goes
// through the relay, which must still be there to carry it.
func (s *Stack) Close() error {
	err := s.SFU.Close()
	if s.TURN.AllocationCount() > 0 {
		s.TURN.Drain(closeQuiet, closeLimit)
	}
	return errors.Join(err, s.TURN.Close())
}
