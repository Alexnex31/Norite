// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package turn is the embedded TURN and STUN server: a relay for clients whose network will not let them
// reach the SFU directly, over UDP, TCP or TLS, that reaches the SFU and nothing else.
//
// A client of this SFU behind any NAT, symmetric included, reaches it without help: it sends first, and
// the SFU learns its address from the check. What a relay is for is a network that refuses UDP to the
// SFU's port, which is why TCP and TLS are the transports that matter here.
package turn

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/stun/v4"
	"github.com/pion/turn/v5"
	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/internal/platform/ratelimit"
	"github.com/Alexnex31/Norite/backend/internal/voice/pionlog"
)

// Options configures a server. Each listener is optional, and a server with none serves nothing.
type Options struct {
	// UDP, TCP and TLS are the listeners, owned by the server from New. TLS is a plain TCP listener that
	// the server wraps with TLSConfig, so that it bounds handshakes itself (streams.go).
	UDP       net.PacketConn
	TCP       net.Listener
	TLS       net.Listener
	TLSConfig *tls.Config

	// Secret signs credentials (DeriveSecret).
	Secret []byte

	// Relay is the address relay sockets are bound on, the SFU's own host, and RelayPorts the range they
	// take; zeroes for any port.
	Relay      netip.Addr
	RelayPorts [2]uint16

	// SFU is the SFU's media address as the relay reaches it: the only peer there is, and the only address
	// a relay socket sends to.
	SFU netip.AddrPort
	// Announced are the addresses the SFU announces, at SFU's port, which are what a client names it by:
	// its public addresses, or the one address it binds. Empty when it binds every interface, and then
	// each address of this host's is one. A permission names one of these or SFU's own; the relay
	// sends to SFU whichever it was.
	Announced []netip.Addr

	Limits Limits
	Logger zerolog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Server is a running TURN server.
type Server struct {
	opts Options
	lim  Limits
	log  zerolog.Logger
	now  func() time.Time

	relays    *relays
	sources   *sources
	replies   *replies
	streams   *streams
	listeners []*streamListener
	pion      *turn.Server
	// sfu is opts.SFU as a relay socket writes to it.
	sfu *net.UDPAddr

	// host is this host's addresses, read again at most every hostRecheck, when Announced is empty.
	host atomic.Pointer[hostAddrs]
	// panicked is each place a panic has been logged from.
	panicked sync.Map
}

type hostAddrs struct {
	addrs []netip.Addr
	at    time.Time
}

const hostRecheck = 10 * time.Second

// New starts a server.
func New(opts Options) (*Server, error) {
	if opts.UDP == nil && opts.TCP == nil && opts.TLS == nil {
		return nil, errors.New("turn: no listener")
	}
	if opts.TLS != nil && opts.TLSConfig == nil {
		return nil, errors.New("turn: a TLS listener without a certificate")
	}
	if len(opts.Secret) < 32 {
		return nil, errors.New("turn: the secret is too short")
	}
	if !opts.Relay.IsValid() || !opts.SFU.IsValid() {
		return nil, errors.New("turn: the relay and the SFU need an address")
	}
	if opts.Relay.Unmap().Is4() != opts.SFU.Addr().Unmap().Is4() {
		return nil, errors.New("turn: the relay and the SFU are in different address families")
	}
	if lo, hi := opts.RelayPorts[0], opts.RelayPorts[1]; (lo == 0) != (hi == 0) || lo > hi {
		return nil, fmt.Errorf("turn: the relay port range %d-%d is not a range", lo, hi)
	}
	opts.Relay = opts.Relay.Unmap()
	opts.SFU = netip.AddrPortFrom(opts.SFU.Addr().Unmap(), opts.SFU.Port())
	if reflect.ValueOf(opts.Limits).IsZero() {
		opts.Limits = DefaultLimits()
	}
	if err := opts.Limits.validate(); err != nil {
		return nil, err
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	opts.Announced = slices.Clone(opts.Announced)
	for i, a := range opts.Announced {
		opts.Announced[i] = a.Unmap()
	}

	s := &Server{
		opts: opts,
		lim:  opts.Limits,
		log:  opts.Logger.With().Str("subsystem", "turn").Logger(),
		now:  opts.Now,
		sfu:  net.UDPAddrFromAddrPort(opts.SFU),
	}
	s.relays = &relays{s: s, perUser: map[string]int{}}
	s.sources = &sources{perSource: map[string]int{}, perAddr: map[string]int{}}
	s.streams = &streams{lim: s.lim, perSource: map[string]int{}, live: map[*streamConn]struct{}{}}
	replies, err := newReplies(s.lim)
	if err != nil {
		return nil, err
	}
	s.replies = replies
	if err := s.relays.probe(); err != nil {
		return nil, err
	}

	config := turn.ServerConfig{
		Realm:        Realm,
		AuthHandler:  s.authenticate,
		QuotaHandler: s.quota,
		// Sampled: pion logs some of what a stranger sends at error level, an unknown attribute on a
		// Binding request for one, before anything here sees the request.
		LoggerFactory: pionlog.Logs{Log: s.log.Sample(&zerolog.BurstSampler{Burst: pionLogBurst, Period: time.Minute})},
		EventHandler: turn.EventHandler{
			OnAllocationCreated: func(src, dst net.Addr, _, _, _ string, _ net.Addr, _ int) {
				defer s.contain("the allocation events", nil)
				panicHook("the allocation events")
				s.sources.add(src, dst)
			},
			OnAllocationDeleted: func(src, dst net.Addr, _, _, _ string) {
				defer s.contain("the allocation events", nil)
				s.sources.remove(src, dst)
			},
		},
	}
	if opts.UDP != nil {
		config.PacketConnConfigs = append(config.PacketConnConfigs, turn.PacketConnConfig{
			PacketConn:            &replyConn{PacketConn: opts.UDP, s: s},
			RelayAddressGenerator: s.relays,
			PermissionHandler:     s.permit,
		})
	}
	for _, l := range []struct {
		ln  net.Listener
		tls *tls.Config
	}{{opts.TCP, nil}, {opts.TLS, opts.TLSConfig}} {
		if l.ln == nil {
			continue
		}
		sl := newStreamListener(s, l.ln, l.tls)
		s.listeners = append(s.listeners, sl)
		config.ListenerConfigs = append(config.ListenerConfigs, turn.ListenerConfig{
			Listener:              sl,
			RelayAddressGenerator: s.relays,
			PermissionHandler:     s.permit,
		})
	}

	srv, err := turn.NewServer(config)
	if err != nil {
		for _, sl := range s.listeners {
			_ = sl.Close()
		}
		return nil, fmt.Errorf("turn: %w", err)
	}
	s.pion = srv
	return s, nil
}

// pionLogBurst is how many of pion's lines TURN logs a minute.
const pionLogBurst = 10

// Close stops the server: its listeners, every connection they accepted, and every allocation. pion ends
// the allocations once each listener's loop has stopped, and leaves open the connections it accepted.
func (s *Server) Close() error {
	err := s.pion.Close()
	s.streams.close()
	return err
}

// AllocationCount is how many allocations the server holds.
func (s *Server) AllocationCount() int {
	return s.pion.AllocationCount()
}

// UDPAddr, TCPAddr and TLSAddr are where the server listens, nil where it does not: what the harness
// tells a client, and what a test dials.
func (s *Server) UDPAddr() net.Addr {
	if s.opts.UDP == nil {
		return nil
	}
	return s.opts.UDP.LocalAddr()
}

func (s *Server) TCPAddr() net.Addr {
	if s.opts.TCP == nil {
		return nil
	}
	return s.opts.TCP.Addr()
}

func (s *Server) TLSAddr() net.Addr {
	if s.opts.TLS == nil {
		return nil
	}
	return s.opts.TLS.Addr()
}

// authenticate is pion's auth handler. It writes nothing to the log: a username is a stranger's text,
// and pion's own handlers log every one they refuse at error level, quoting it.
//
// Expiry is asked of an Allocate alone. Every later request, a Refresh, a permission or a channel, is
// authenticated again, so a credential checked for expiry on each would have to last the whole call. Asked
// here, it lasts the gather: a request on an allocation that exists is that allocation's, and one with no
// allocation is refused by pion whatever its credential.
func (s *Server) authenticate(ra *turn.RequestAttributes) (userID string, key []byte, ok bool) {
	defer s.contain("authentication", nil)
	panicHook("authentication")
	if ra.Realm != Realm {
		return "", nil, false
	}
	expires, user, ok := parseUsername(ra.Username)
	if !ok {
		return "", nil, false
	}
	if ra.Method == stun.MethodAllocate && !s.now().Before(expires) {
		return "", nil, false
	}
	return user.String(), turn.GenerateAuthKey(ra.Username, Realm, password(s.opts.Secret, ra.Username)), true
}

// quota is pion's quota handler: the per-source bound, whose count the allocation events keep. The total
// and per-account bounds are the relay generator's, which counts exactly; this one can be passed by as
// many allocations as arrive at once, one per stream connection, which StreamsPerSource bounds.
func (s *Server) quota(_, _ string, src net.Addr) (ok bool) {
	defer s.contain("the quota", nil)
	return s.sources.count(src) < s.lim.AllocationsPerSource
}

// permit is pion's permission handler: a permission or a channel names an address of the SFU's, or
// nothing. pion asks with the IP alone, so the port is the relay socket's to hold (relay.go).
func (s *Server) permit(_ net.Addr, peer net.IP) (ok bool) {
	defer s.contain("the permission", nil)
	a, ok := netip.AddrFromSlice(peer)
	return ok && s.known(a.Unmap())
}

// sfuPeer is addr as an address the client knows the SFU by, and whether it is one.
func (s *Server) sfuPeer(addr net.Addr) (netip.AddrPort, bool) {
	u, ok := addr.(*net.UDPAddr)
	if !ok {
		return netip.AddrPort{}, false
	}
	ap := u.AddrPort()
	ap = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	return ap, ap.Port() == s.opts.SFU.Port() && s.known(ap.Addr())
}

// known is whether a is an address the SFU is known by, in the relay's family.
func (s *Server) known(a netip.Addr) bool {
	switch {
	case a.Is4() != s.opts.SFU.Addr().Is4():
		return false
	case a == s.opts.SFU.Addr():
		return true
	case len(s.opts.Announced) > 0:
		return slices.Contains(s.opts.Announced, a)
	}
	return slices.Contains(s.hostAddrs(), a)
}

// hostAddrs is this host's addresses, read again when the last reading is older than hostRecheck: an SFU
// on every interface announces each of them, as they are when a call starts.
func (s *Server) hostAddrs() []netip.Addr {
	if h := s.host.Load(); h != nil && s.now().Sub(h.at) < hostRecheck {
		return h.addrs
	}
	h := &hostAddrs{at: s.now()}
	if ifaddrs, err := net.InterfaceAddrs(); err == nil {
		for _, ia := range ifaddrs {
			if n, ok := ia.(*net.IPNet); ok {
				if a, ok := netip.AddrFromSlice(n.IP); ok {
					h.addrs = append(h.addrs, a.Unmap())
				}
			}
		}
	}
	s.host.Store(h)
	return h.addrs
}

// isSFU is whether addr is the SFU's media address, IP and port.
func (s *Server) isSFU(addr net.Addr) bool {
	u, ok := addr.(*net.UDPAddr)
	if !ok {
		return false
	}
	ap := u.AddrPort()
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()) == s.opts.SFU
}

// contain is deferred in every callback this package gives pion. The self-hosted TURN server shares
// norite-server's process, as the SFU does, and these parse a stranger's requests. A panic refuses the
// request, through err when the callback returns one, and is logged once for each place it happens: one
// on a request's shape recurs with every request, and a second bug elsewhere is a second line.
func (s *Server) contain(where string, err *error) {
	v := recover()
	if v == nil {
		return
	}
	if err != nil {
		*err = fmt.Errorf("turn: panicked in %s", where)
	}
	if _, logged := s.panicked.LoadOrStore(where, true); !logged {
		s.log.Error().Str("in", where).Str("panic", fmt.Sprint(v)).Bytes("stack", debug.Stack()).
			Msg("TURN panicked; refusing the request")
	}
}

// sources counts allocations by source address, an IPv6 /64 being one, and by connection: the client's
// address and the server's, with the transport, which is how a stream connection is known to hold one.
type sources struct {
	mu        sync.Mutex
	perSource map[string]int
	perAddr   map[string]int
}

func sourceKey(addr net.Addr) string {
	switch a := addr.(type) {
	case *net.UDPAddr:
		return ratelimit.AddrKey(a.IP)
	case *net.TCPAddr:
		return ratelimit.AddrKey(a.IP)
	}
	return "unknown"
}

func (c *sources) count(addr net.Addr) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.perSource[sourceKey(addr)]
}

// connKey names one connection: a UDP client and a TCP one may share an address and a port, and so may
// a TCP and a TLS one, to different ports of the server's.
func connKey(src, dst net.Addr) string {
	return src.Network() + " " + src.String() + " " + dst.String()
}

func (c *sources) allocated(src, dst net.Addr) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.perAddr[connKey(src, dst)] > 0
}

func (c *sources) add(src, dst net.Addr) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.perSource[sourceKey(src)]++
	c.perAddr[connKey(src, dst)]++
}

func (c *sources) remove(src, dst net.Addr) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := sourceKey(src)
	if c.perSource[k]--; c.perSource[k] <= 0 {
		delete(c.perSource, k)
	}
	a := connKey(src, dst)
	if c.perAddr[a]--; c.perAddr[a] <= 0 {
		delete(c.perAddr, a)
	}
}

// testPanic, set only by tests, is called where a stranger's request is handled, to prove a panic there is
// contained.
var testPanic atomic.Pointer[func(where string)]

func panicHook(where string) {
	if f := testPanic.Load(); f != nil {
		(*f)(where)
	}
}
