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

	// SFU is the SFU's media address as the relay reaches it: the only peer there is.
	SFU netip.AddrPort

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

	relays  *relays
	sources *sources
	replies *replies
	streams []*streamListener
	pion    *turn.Server

	panicked sync.Once
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

	s := &Server{
		opts: opts,
		lim:  opts.Limits,
		log:  opts.Logger.With().Str("subsystem", "turn").Logger(),
		now:  opts.Now,
	}
	s.relays = &relays{s: s, perUser: map[string]int{}}
	s.sources = &sources{perSource: map[string]int{}, perAddr: map[string]int{}}
	replies, err := newReplies(s.lim)
	if err != nil {
		return nil, err
	}
	s.replies = replies

	config := turn.ServerConfig{
		Realm:         Realm,
		AuthHandler:   s.authenticate,
		QuotaHandler:  s.quota,
		LoggerFactory: pionlog.Logs{Log: s.log},
		EventHandler: turn.EventHandler{
			OnAllocationCreated: func(src, _ net.Addr, _, _, _ string, _ net.Addr, _ int) {
				s.sources.add(src)
			},
			OnAllocationDeleted: func(src, _ net.Addr, _, _, _ string) {
				s.sources.remove(src)
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
		s.streams = append(s.streams, sl)
		config.ListenerConfigs = append(config.ListenerConfigs, turn.ListenerConfig{
			Listener:              sl,
			RelayAddressGenerator: s.relays,
			PermissionHandler:     s.permit,
		})
	}

	srv, err := turn.NewServer(config)
	if err != nil {
		for _, sl := range s.streams {
			_ = sl.Close()
		}
		return nil, fmt.Errorf("turn: %w", err)
	}
	s.pion = srv
	return s, nil
}

// Close stops the server: its listeners, and with them every allocation.
func (s *Server) Close() error {
	return s.pion.Close()
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

// permit is pion's permission handler: a permission or a channel names the SFU's address, or nothing.
// pion asks with the IP alone, so the port is the relay socket's to hold (relay.go).
func (s *Server) permit(_ net.Addr, peer net.IP) (ok bool) {
	defer s.contain("the permission", nil)
	a, ok := netip.AddrFromSlice(peer)
	return ok && a.Unmap() == s.opts.SFU.Addr()
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
// request, through err when the callback returns one, and is logged once: one on a request's shape recurs
// with every request.
func (s *Server) contain(where string, err *error) {
	v := recover()
	if v == nil {
		return
	}
	if err != nil {
		*err = fmt.Errorf("turn: panicked in %s", where)
	}
	s.panicked.Do(func() {
		s.log.Error().Str("in", where).Str("panic", fmt.Sprint(v)).Bytes("stack", debug.Stack()).
			Msg("TURN panicked; refusing the request")
	})
}

// sources counts allocations by source address, an IPv6 /64 being one, and by exact address, which is
// how a stream connection is known to have made one.
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

func (c *sources) allocated(addr net.Addr) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.perAddr[addr.String()] > 0
}

func (c *sources) add(addr net.Addr) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.perSource[sourceKey(addr)]++
	c.perAddr[addr.String()]++
}

func (c *sources) remove(addr net.Addr) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := sourceKey(addr)
	if c.perSource[k]--; c.perSource[k] <= 0 {
		delete(c.perSource, k)
	}
	a := addr.String()
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
