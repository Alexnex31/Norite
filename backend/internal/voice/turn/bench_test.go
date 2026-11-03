// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package turn

import (
	"net"
	"net/netip"
	"testing"
)

// relayPath is one relay socket in front of the fixture's SFU, written to as an announced address, as
// every relayed packet is.
type relayPath struct {
	c *relayConn
	// sfu is the stand-in, read and written without allocating, so that what is counted is the relay's.
	sfu   *net.UDPConn
	peer  net.Addr
	relay netip.AddrPort
}

func newRelayPath(tb testing.TB) relayPath {
	tb.Helper()
	public := netip.MustParseAddr("192.0.2.10")
	f := newFixture(tb, func(o *Options) {
		o.Announced = []netip.Addr{public}
		o.Limits = DefaultLimits()
		o.Limits.RelayPacketsPerSecond, o.Limits.RelayBitsPerSecond = 1<<30, 1<<30
	})
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = pc.Close() })
	c := &relayConn{PacketConn: pc, s: f.s, fam: f.s.v4, heard: f.s.now()}
	c.udp, _ = pc.(*net.UDPConn)
	peer := net.UDPAddrFromAddrPort(netip.AddrPortFrom(public, uint16(f.sfuAddr().(*net.UDPAddr).Port)))
	return relayPath{c: c, sfu: f.sfu.(*net.UDPConn), peer: peer, relay: pc.LocalAddr().(*net.UDPAddr).AddrPort()}
}

// roundTrip relays one packet to the SFU, and one back.
func (r relayPath) roundTrip(tb testing.TB, p, buf []byte) {
	if _, err := r.c.WriteTo(p, r.peer); err != nil {
		tb.Fatal(err)
	}
	if _, _, err := r.sfu.ReadFromUDPAddrPort(buf); err != nil {
		tb.Fatal(err)
	}
	if _, err := r.sfu.WriteToUDPAddrPort(p, r.relay); err != nil {
		tb.Fatal(err)
	}
	if _, _, err := r.c.ReadFrom(buf); err != nil {
		tb.Fatal(err)
	}
}

// TestTheRelayAllocatesNothingPerPacket holds the relay's own work per packet, both ways, at no
// allocation: it read through net.UDPConn.ReadFrom and made a net.Addr for every packet the SFU sent
// back, four allocations each, measured by /optimization-review.
func TestTheRelayAllocatesNothingPerPacket(t *testing.T) {
	r := newRelayPath(t)
	p, buf := make([]byte, 120), make([]byte, 1500)
	p[0] = 0x80
	r.roundTrip(t, p, buf)
	if n := testing.AllocsPerRun(200, func() { r.roundTrip(t, p, buf) }); n != 0 {
		t.Fatalf("%v allocations a round trip through the relay", n)
	}
}

func BenchmarkRelayRoundTrip(b *testing.B) {
	r := newRelayPath(b)
	p, buf := make([]byte, 120), make([]byte, 1500)
	p[0] = 0x80
	b.ReportAllocs()
	for b.Loop() {
		r.roundTrip(b, p, buf)
	}
}
