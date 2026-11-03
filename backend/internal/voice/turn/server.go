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
	// OtherRelay and OtherSFU are the same in the other address family, when the SFU listens on every
	// interface and so is reached on loopback in both: a client reaching TURN over IPv6 asks for an IPv6
	// relay, Pion's client included, and with one family alone it would be refused every allocation.
	// Both unset for one family.
	OtherRelay netip.Addr
	OtherSFU   netip.AddrPort
	// Announced are the addresses the SFU announces, at SFU's port, which are what a client names it by:
	// its public addresses, or the one address it binds. They count family by family, and in a family
	// with none, because the SFU binds every interface or its public addresses are all of the other,
	// each address of this host's is one. A permission names one of these or the family's SFU address;
	// the relay sends to that SFU address whichever it was.
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
	// v4 and v6 are what the relay serves in each family, nil for one it does not.
	v4, v6 *family

	// host is this host's addresses, for a family nothing is announced in, read again every hostRecheck
	// by a goroutine of its own, never on a packet's path.
	host     atomic.Pointer[[]netip.Addr]
	stop     chan struct{}
	stopOnce sync.Once
	// panicked is each place a panic has been logged from.
	panicked sync.Map
}

const hostRecheck = 10 * time.Second

// family is what the relay serves in one address family: where its sockets are bound, the SFU's media
// address there, and what the SFU announces in it.
type family struct {
	relay     netip.Addr
	sfu       netip.AddrPort
	sfuUDP    *net.UDPAddr
	announced []netip.Addr
}

// newFamily checks one family's relay and SFU address and builds it, nil when neither is set.
func newFamily(relay netip.Addr, sfu netip.AddrPort, announced []netip.Addr) (*family, error) {
	if !relay.IsValid() && !sfu.IsValid() {
		return nil, nil
	}
	if !relay.IsValid() || !sfu.IsValid() {
		return nil, errors.New("turn: the relay and the SFU need an address")
	}
	relay, sfu = relay.Unmap(), netip.AddrPortFrom(sfu.Addr().Unmap(), sfu.Port())
	if relay.Is4() != sfu.Addr().Is4() {
		return nil, errors.New("turn: the relay and the SFU are in different address families")
	}
	f := &family{relay: relay, sfu: sfu, sfuUDP: net.UDPAddrFromAddrPort(sfu)}
	for _, a := range announced {
		if a = a.Unmap(); a.Is4() == relay.Is4() {
			f.announced = append(f.announced, a)
		}
	}
	return f, nil
}

// familyOf is what the relay serves in a's family, nil when it serves none.
func (s *Server) familyOf(a netip.Addr) *family {
	if a.Unmap().Is4() {
		return s.v4
	}
	return s.v6
}

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
	primary, err := newFamily(opts.Relay, opts.SFU, opts.Announced)
	if err != nil {
		return nil, err
	}
	other, err := newFamily(opts.OtherRelay, opts.OtherSFU, opts.Announced)
	if err != nil {
		return nil, err
	}
	if other != nil && other.relay.Is4() == primary.relay.Is4() {
		return nil, errors.New("turn: the other relay is in the relay's own family")
	}
	if lo, hi := opts.RelayPorts[0], opts.RelayPorts[1]; (lo == 0) != (hi == 0) || lo > hi {
		return nil, fmt.Errorf("turn: the relay port range %d-%d is not a range", lo, hi)
	}
	if reflect.ValueOf(opts.Limits).IsZero() {
		opts.Limits = DefaultLimits()
	}
	if err := opts.Limits.validate(); err != nil {
		return nil, err
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	s := &Server{
		opts: opts,
		lim:  opts.Limits,
		log:  opts.Logger.With().Str("subsystem", "turn").Logger(),
		now:  opts.Now,
		stop: make(chan struct{}),
	}
	for _, f := range []*family{primary, other} {
		switch {
		case f == nil:
		case f.relay.Is4():
			s.v4 = f
		default:
			s.v6 = f
		}
	}
	s.relays = &relays{s: s, perUser: map[string]int{}, byAddr: map[string]*relayConn{}}
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
	if (s.v4 != nil && len(s.v4.announced) == 0) || (s.v6 != nil && len(s.v6.announced) == 0) {
		s.readHost()
		go s.watchHost()
	}

	config := turn.ServerConfig{
		Realm: Realm,
		// An allocation the client names no family for is in the family of the address it reached TURN
		// on, by pion's default: a client reaching a dual-stack listener over IPv6 would be refused every
		// allocation by an IPv4 relay alone. Strict is RFC 6156's default, IPv4, which is right whenever
		// IPv4 is all the relay serves. Serving both, or IPv6 alone, pion's default is.
		StrictAddressFamily: s.v6 == nil,
		AuthHandler:         s.authenticate,
		QuotaHandler:        s.quota,
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
			OnChannelCreated: s.channelCreated,
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
		s.stopOnce.Do(func() { close(s.stop) })
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
	s.stopOnce.Do(func() { close(s.stop) })
	err := s.pion.Close()
	s.streams.close()
	return err
}

// Drain waits until no relay has heard from the SFU for quiet, counting from the call, and returns after
// limit whatever happens. pion relays each packet the SFU sent on a goroutine of its own, after the read
// that records it, so a quiet stretch with the SFU closed is one in which everything it sent on its way
// out has been read and passed on, unless that goroutine was held off for longer than quiet.
func (s *Server) Drain(quiet, limit time.Duration) {
	start := s.now()
	for {
		last := start
		s.relays.mu.Lock()
		for _, c := range s.relays.byAddr {
			c.mu.Lock()
			if c.heard.After(last) {
				last = c.heard
			}
			c.mu.Unlock()
		}
		s.relays.mu.Unlock()
		now := s.now()
		if now.Sub(last) >= quiet || now.Sub(start) >= limit {
			return
		}
		time.Sleep(quiet / 10)
	}
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

// channelCreated ends an allocation that bound a channel to anything but an address of the SFU's at its
// port. pion admits the binding on the peer's IP alone, and keeps up to 16,384 bindings an allocation in a
// list it walks for every packet relayed either way, so bindings to every port of the SFU's address
// would make each packet a call carries cost thousands of comparisons. A client in a call binds only the
// SFU's addresses, which are few; one binding anything else is not a client in a call.
func (s *Server) channelCreated(_, _ net.Addr, _, _, _ string, relay, peer net.Addr, _ uint16) {
	defer s.contain("the channel events", nil)
	if _, ok := s.sfuPeer(peer); ok {
		return
	}
	s.relays.close(relay)
}

// sfuPeer is addr as an address the client knows the SFU by, and whether it is one.
func (s *Server) sfuPeer(addr net.Addr) (netip.AddrPort, bool) {
	u, ok := addr.(*net.UDPAddr)
	if !ok {
		return netip.AddrPort{}, false
	}
	ap := u.AddrPort()
	ap = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	f := s.familyOf(ap.Addr())
	return ap, f != nil && ap.Port() == f.sfu.Port() && s.known(ap.Addr())
}

// known is whether a is an address the SFU is known by, in a family the relay serves.
func (s *Server) known(a netip.Addr) bool {
	a = a.Unmap()
	f := s.familyOf(a)
	switch {
	case f == nil:
		return false
	case a == f.sfu.Addr():
		return true
	case len(f.announced) > 0:
		return slices.Contains(f.announced, a)
	}
	return slices.Contains(*s.host.Load(), a)
}

// readHost reads this host's addresses: an SFU on every interface announces each of them, as they are when
// a call starts.
func (s *Server) readHost() {
	var addrs []netip.Addr
	if ifaddrs, err := net.InterfaceAddrs(); err == nil {
		for _, ia := range ifaddrs {
			if n, ok := ia.(*net.IPNet); ok {
				if a, ok := netip.AddrFromSlice(n.IP); ok {
					addrs = append(addrs, a.Unmap())
				}
			}
		}
	}
	s.host.Store(&addrs)
}

func (s *Server) watchHost() {
	defer s.contain("the host's addresses", nil)
	t := time.NewTicker(hostRecheck)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.readHost()
		}
	}
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
