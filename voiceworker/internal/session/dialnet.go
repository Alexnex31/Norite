// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"errors"
	"fmt"
	"net"

	"github.com/pion/transport/v5"
)

// guardedNet is the network Pion's ICE agent dials through, refusing what reachable refuses for a remote
// candidate: this host's loopback unless the join admits it, its link, and the cloud metadata services.
//
// The TURN servers a call gathers with are the instance's words since M33 (M28's /security-sweep): a
// hostile instance could name any host and port, and the worker would dial it over UDP, TCP or TLS,
// resolving a name when it dials. So the address is judged where it is used, after resolving, which is
// rule 18's shape for a server's fetch: a name that resolves to a refused address is refused, and one
// resolved twice is judged on what it is dialed at. Private ranges stay, for an instance on a LAN.
type guardedNet struct {
	transport.Net
	loopback bool
}

// errRefusedAddress is a dial or a resolution to an address a voice server cannot have.
var errRefusedAddress = errors.New("an address no voice server has")

func (n guardedNet) check(ip net.IP) error {
	if !reachable(ip, n.loopback) {
		return fmt.Errorf("%w: %s", errRefusedAddress, ip)
	}
	return nil
}

// ResolveUDPAddr is how TURN over UDP and DTLS, and STUN, find their server: what it returns is what is
// written to.
func (n guardedNet) ResolveUDPAddr(network, address string) (*net.UDPAddr, error) {
	a, err := n.Net.ResolveUDPAddr(network, address)
	if err != nil {
		return nil, err
	}
	if err := n.check(a.IP); err != nil {
		return nil, err
	}
	return a, nil
}

// ResolveTCPAddr is how TURN over TCP and TLS find theirs.
func (n guardedNet) ResolveTCPAddr(network, address string) (*net.TCPAddr, error) {
	a, err := n.Net.ResolveTCPAddr(network, address)
	if err != nil {
		return nil, err
	}
	if err := n.check(a.IP); err != nil {
		return nil, err
	}
	return a, nil
}

// DialUDP and DialTCP judge the address dialed, whatever resolved it.
func (n guardedNet) DialUDP(network string, laddr, raddr *net.UDPAddr) (transport.UDPConn, error) {
	if raddr == nil {
		return nil, fmt.Errorf("%w: none", errRefusedAddress)
	}
	if err := n.check(raddr.IP); err != nil {
		return nil, err
	}
	return n.Net.DialUDP(network, laddr, raddr)
}

func (n guardedNet) DialTCP(network string, laddr, raddr *net.TCPAddr) (transport.TCPConn, error) {
	if raddr == nil {
		return nil, fmt.Errorf("%w: none", errRefusedAddress)
	}
	if err := n.check(raddr.IP); err != nil {
		return nil, err
	}
	return n.Net.DialTCP(network, laddr, raddr)
}

// Dial resolves first and dials the address it judged, so a name cannot answer one address to the check
// and another to the dial.
func (n guardedNet) Dial(network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ip, err := n.ResolveIPAddr("ip", host)
	if err != nil {
		return nil, err
	}
	if err := n.check(ip.IP); err != nil {
		return nil, err
	}
	return n.Net.Dial(network, net.JoinHostPort(ip.IP.String(), port))
}

// CreateDialer's dialer goes through Dial's check too: nothing Pion dials escapes it by asking for a dialer.
func (n guardedNet) CreateDialer(d *net.Dialer) transport.Dialer {
	return guardedDialer{n: n, d: n.Net.CreateDialer(d)}
}

type guardedDialer struct {
	n guardedNet
	d transport.Dialer
}

func (g guardedDialer) Dial(network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ip, err := g.n.ResolveIPAddr("ip", host)
	if err != nil {
		return nil, err
	}
	if err := g.n.check(ip.IP); err != nil {
		return nil, err
	}
	return g.d.Dial(network, net.JoinHostPort(ip.IP.String(), port))
}
