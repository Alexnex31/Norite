// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package turn

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/pion/turn/v5"

	"github.com/Alexnex31/Norite/backend/internal/voice/rate"
)

var (
	// errTCPRelay refuses RFC 6062: pion/turn opens a TCP connection to any peer its permission handler
	// admits, on any port, which is an open proxy whatever this package's UDP relay does.
	errTCPRelay = errors.New("turn: relaying over TCP is not offered")
	// errNoUser refuses a relay socket asked for without an authenticated account: pion asks for one to
	// probe an even port, which is not offered either.
	errNoUser = errors.New("turn: a relay socket needs an account")
	errFull   = errors.New("turn: no room for another allocation")
	errFamily = errors.New("turn: the relay does not serve that address family")
)

// relays makes every allocation's relay socket, and is the reason the relay is not an open proxy: each
// socket it hands pion can send to the SFU's media address and nothing else, and hears nothing else.
//
// The sockets are bound on the SFU's own host, so the relay and the SFU talk host-locally and the public
// firewall opens only the SFU's port and TURN's. The address each reports is the one it is bound to: the
// SFU is its only reader, and a public address would send the SFU out through its own NAT and back, which
// many routers do not do.
type relays struct {
	s *Server

	mu      sync.Mutex
	total   int
	perUser map[string]int
}

var _ turn.RelayAddressGenerator = (*relays)(nil)

func (r *relays) Validate() error { return nil }

func (r *relays) AllocateListener(turn.AllocateListenerConfig) (net.Listener, net.Addr, error) {
	return nil, nil, errTCPRelay
}

func (r *relays) AllocateConn(turn.AllocateConnConfig) (net.Conn, error) {
	return nil, errTCPRelay
}

func (r *relays) AllocatePacketConn(conf turn.AllocateListenerConfig) (_ net.PacketConn, _ net.Addr, err error) {
	defer r.s.contain("the relay generator", &err)
	panicHook("the relay generator")
	if conf.UserID == "" {
		return nil, nil, errNoUser
	}
	relay := r.s.opts.Relay
	if (conf.Network == "udp4") != relay.Is4() {
		return nil, nil, errFamily
	}
	if !r.reserve(conf.UserID) {
		return nil, nil, errFull
	}
	pc, err := r.bind(conf.Network, relay)
	if err != nil {
		r.release(conf.UserID)
		return nil, nil, err
	}
	c := &relayConn{PacketConn: pc, s: r.s, user: conf.UserID, heard: r.s.now()}
	c.mu.Lock()
	c.idle = time.AfterFunc(r.s.lim.Idle, c.checkIdle)
	c.mu.Unlock()
	return c, pc.LocalAddr(), nil
}

// bind takes a socket on the relay address, from the configured range when there is one.
func (r *relays) bind(network string, relay netip.Addr) (net.PacketConn, error) {
	lo, hi := r.s.opts.RelayPorts[0], r.s.opts.RelayPorts[1]
	if lo == 0 {
		return net.ListenPacket(network, netip.AddrPortFrom(relay, 0).String())
	}
	span := int(hi) - int(lo) + 1
	start := rand.IntN(span)
	// A busy port is passed over, and the whole range at most once.
	for i := range span {
		port := uint16(int(lo) + (start+i)%span)
		pc, err := net.ListenPacket(network, netip.AddrPortFrom(relay, port).String())
		if err == nil {
			return pc, nil
		}
	}
	return nil, fmt.Errorf("turn: every relay port from %d to %d is taken", lo, hi)
}

func (r *relays) reserve(user string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.total >= r.s.lim.Allocations || r.perUser[user] >= r.s.lim.AllocationsPerUser {
		return false
	}
	r.total++
	r.perUser[user]++
	return true
}

func (r *relays) release(user string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.total--
	if r.perUser[user]--; r.perUser[user] <= 0 {
		delete(r.perUser, user)
	}
}

// relayConn is one allocation's relay socket. pion writes what the client sends through it and reads
// what comes back; closing it ends the allocation, since pion deletes an allocation whose relay socket
// fails a read.
type relayConn struct {
	net.PacketConn
	s    *Server
	user string

	// mu guards the meter, heard and the idle timer. pion writes from the client's read loop and reads
	// from its own, and the timer fires on a third.
	mu    sync.Mutex
	meter rate.Meter
	heard time.Time
	idle  *time.Timer

	once sync.Once
}

// WriteTo sends towards the SFU, and towards nothing else: a write to any other address arrives nowhere
// and reports success, so pion has nothing to log for it. pion's permission handler has refused it
// already; this is the check that holds whatever pion does, port included.
func (c *relayConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if !c.s.isSFU(addr) {
		return len(p), nil
	}
	lim := c.s.lim
	c.mu.Lock()
	ok, out := c.meter.Admit(c.s.now(), len(p), lim.RelayPacketsPerSecond, lim.RelayBitsPerSecond, lim.StrikesToClose)
	c.mu.Unlock()
	if out {
		_ = c.Close()
		return 0, net.ErrClosed
	}
	if !ok {
		return len(p), nil
	}
	return c.PacketConn.WriteTo(p, addr)
}

// ReadFrom passes on what the SFU sends and drops the rest: a socket on the SFU's host hears whatever
// else on that host, or beyond it, writes to its port.
func (c *relayConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(p)
		if err != nil {
			return n, addr, err
		}
		if !c.s.isSFU(addr) {
			continue
		}
		c.mu.Lock()
		c.heard = c.s.now()
		c.mu.Unlock()
		return n, addr, nil
	}
}

// checkIdle closes the relay when the SFU has sent nothing through it for Idle. A connection in a call
// answers the client's consent checks every few seconds; one the SFU closed, or never admitted, does not.
func (c *relayConn) checkIdle() {
	c.mu.Lock()
	quiet := c.s.now().Sub(c.heard)
	if quiet < c.s.lim.Idle {
		c.idle.Reset(c.s.lim.Idle - quiet)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	_ = c.Close()
}

func (c *relayConn) Close() error {
	var err error
	c.once.Do(func() {
		c.mu.Lock()
		c.idle.Stop()
		c.mu.Unlock()
		err = c.PacketConn.Close()
		c.s.relays.release(c.user)
	})
	return err
}
