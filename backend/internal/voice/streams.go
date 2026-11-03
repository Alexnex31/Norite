// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"io"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/pion/transport/v5/packetio"
	"github.com/pion/webrtc/v4"
)

// An admitted peer holds the SRTP keys, so whatever it encrypts is decrypted. For every SSRC it sends
// that its offer did not declare, SRTP opens a read stream with a buffer of its own (a megabyte, by
// default), and Pion keeps the stream until the connection closes, unread. Pion bounds the goroutines
// that probe such streams and nothing else, and those packets never reach the per-track meter. So a peer
// sending to a new SSRC with every packet grows the server's memory without limit, and the self-hosted
// SFU shares norite-server's process. Found by /security-sweep, by reading Pion's
// undeclaredRTPMediaProcessor and srtp's getOrCreateReadStream.
//
// Every stream's buffer comes from the connection's own factory, which counts them. Past what the
// connection's offer accounts for, the stream gets a closed buffer, which SRTP drops packets into, and the
// connection is closed.

// Buffer limits per stream. A declared stream is read as fast as it arrives; these bound one nobody reads.
const (
	rtpBuffer  = 128 << 10
	rtcpBuffer = 32 << 10
)

// streamsFor is how many streams a connection with these tracks and slots may open: an RTP and an RTCP
// stream for each track it sends, an RTCP stream for each slot, and a few for what Pion opens of its own
// (bandwidth probes on SSRC 0, feedback naming no media).
func streamsFor(tracks, slots int) int {
	return 2*tracks + slots + 8
}

// apiFor builds the API a connection's peer connection comes from, with the connection's buffer factory
// and its count of remote candidates.
func (s *SFU) apiFor(c *conn) *webrtc.API {
	se := s.se
	se.BufferFactory = c.buffer
	se.SetRemoteIPFilter(c.remoteCandidate)
	return webrtc.NewAPI(webrtc.WithMediaEngine(s.media), webrtc.WithInterceptorRegistry(s.reg),
		webrtc.WithSettingEngine(se))
}

// buffer is the connection's buffer factory. Called from inside SRTP, so it never closes the connection
// itself: that would wait on the goroutine calling it.
func (c *conn) buffer(kind packetio.BufferPacketType, _ uint32) io.ReadWriteCloser {
	c.mu.Lock()
	c.streams++
	over := c.streams > c.maxStreams
	c.mu.Unlock()

	b := streamBuffer{packetio.NewBuffer()}
	if over {
		_ = b.Close()
		go c.close("sending on more SSRCs than its offer declared")
		return b
	}
	if kind == packetio.RTCPBufferPacket {
		b.SetLimitSize(rtcpBuffer)
	} else {
		b.SetLimitSize(rtpBuffer)
	}
	return b
}

// streamBuffer is a packetio.Buffer as SRTP takes one, as SRTP's own default adapts it: plain Read and
// Write for io.ReadWriteCloser, and the attribute-carrying pair and the deadline SRTP looks for.
type streamBuffer struct{ *packetio.Buffer }

var _ io.ReadWriteCloser = streamBuffer{}

func (b streamBuffer) Read(p []byte) (int, error) {
	n, _, err := b.Buffer.Read(p, nil)
	return n, err
}

func (b streamBuffer) Write(p []byte) (int, error) { return b.Buffer.Write(p, nil) }

func (b streamBuffer) ReadWithAttributes(p []byte, a packetio.Attributes) (int, packetio.Attributes, error) {
	return b.Buffer.Read(p, a)
}

func (b streamBuffer) WriteWithAttributes(p []byte, a packetio.Attributes) (int, error) {
	return b.Buffer.Write(p, a)
}

func (b streamBuffer) SetReadDeadline(t time.Time) error { return b.Buffer.SetReadDeadline(t) }

// The offer's candidates are bounded (readOffer), and those are not all the remote candidates ICE keeps.
// For every source address that sends an authenticated binding request and is not yet a candidate, Pion's
// ICE adds a peer-reflexive one, pairs it with each local candidate and checks each pair up to seven
// times; it has no bound of its own. A peer knows its credentials and the SFU's, from the answer, so it
// can send checks from thousands of ports, or from every address of its /64, and the SFU holds a
// candidate, its pairs and a mux mapping for each, and sends checks to addresses the peer never declared.
// Found by /code-review, by reading pion/ice's handleInboundRequest: the reopening condition of the
// ledger's "the SFU does work for a packet before any meter counts it".
//
// Pion asks the remote IP filter about each candidate before it adds one, signaled or peer-reflexive, so
// the filter counts the asking, and refuses past the bound. It is asked twice about each of the offer's
// candidates, once as the offer is applied and once as ICE starts, and once about each new source
// address; the filter sees only the IP, so it cannot tell the two apart and counts every ask.

// remoteCandidates is how many times a connection's filter says yes: twice for each of the offer's
// candidates, and as many peer-reflexive ones again as the offer may carry, which is more than a client
// behind NATs on two address families needs.
func remoteCandidates(lim Limits) int {
	return 3 * lim.MaxCandidates
}

// remoteCandidate is the connection's remote IP filter. Called on ICE's own goroutine, so it only counts.
func (c *conn) remoteCandidate(ip net.IP) bool {
	if !c.sfu.reachable(ip) {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.candidates >= remoteCandidates(c.sfu.lim) {
		return false
	}
	c.candidates++
	return true
}

// metadata are cloud instance-metadata services. Two sit in ranges the filter otherwise admits, because a
// LAN instance's clients are private: AWS's IPv6 address in fc00::/7, and Alibaba's in 100.64/10, where
// carrier-grade NAT puts real clients too.
var metadata = []netip.Addr{
	netip.MustParseAddr("169.254.169.254"),
	netip.MustParseAddr("fd00:ec2::254"),
	netip.MustParseAddr("100.100.100.200"),
}

// reachable is whether a remote candidate's address is one a client can have. The SFU is ICE-lite and
// sends checks to no candidate, so this is a second layer: it keeps the connection from ever pairing with
// an address on this host or its link, where a reply would reach a service rather than a client. Private
// ranges stay, because a LAN instance's clients are in them (security ledger).
func (s *SFU) reachable(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	switch {
	case slices.Contains(s.opts.Relays, a):
		return true
	case a.IsLoopback():
		return s.opts.Loopback
	case a.IsUnspecified(), a.IsMulticast(), a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast(),
		a.IsInterfaceLocalMulticast():
		return false
	case a.Is4() && (a.As4()[0] == 0 || a == netip.AddrFrom4([4]byte{255, 255, 255, 255})):
		return false
	}
	return !slices.Contains(metadata, a)
}
