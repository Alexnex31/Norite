// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package turn

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/pion/turn/v5"
)

// TestTheSFUIsReachedByTheAddressesItAnnounces: a client names the SFU by what it announced, a public
// address the host does not have, and the relay delivers to the SFU's media address all the same; what
// the SFU answers comes back as from the address the client wrote to, a STUN response as from the one
// its request went to. The relay worked only where the two addresses were one until /code-review.
func TestTheSFUIsReachedByTheAddressesItAnnounces(t *testing.T) {
	public := []netip.Addr{netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("192.0.2.11")}
	f := newFixture(t, func(o *Options) { o.Announced = public })
	port := f.sfuAddr().(*net.UDPAddr).Port
	at := func(a netip.Addr) *net.UDPAddr { return net.UDPAddrFromAddrPort(netip.AddrPortFrom(a, uint16(port))) }

	relay, err := f.client(t, cred(1)).Allocate()
	if err != nil {
		t.Fatal(err)
	}
	// Two checks, one to each address, as ICE sends them: the answers are matched by transaction.
	first := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	second := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	for i, m := range []*stun.Message{first, second} {
		if _, err := relay.WriteTo(m.Raw, at(public[i])); err != nil {
			t.Fatal(err)
		}
	}
	got := map[[stun.TransactionIDSize]byte]bool{}
	var from net.Addr
	buf := make([]byte, 1500)
	for range 2 {
		_ = f.sfu.SetReadDeadline(time.Now().Add(time.Second))
		n, src, err := f.sfu.ReadFrom(buf)
		if err != nil {
			t.Fatalf("the SFU heard %d of 2 checks: %v", len(got), err)
		}
		m := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
		if err := m.Decode(); err != nil {
			t.Fatal(err)
		}
		got[m.TransactionID] = true
		from = src
	}
	if !got[first.TransactionID] || !got[second.TransactionID] {
		t.Fatal("the SFU did not hear both checks")
	}
	// Answered in the other order: each still comes back from the address its check went to.
	for i, m := range []*stun.Message{second, first} {
		reply := stun.MustBuild(stun.NewTransactionIDSetter(m.TransactionID), stun.BindingSuccess)
		if _, err := f.sfu.WriteTo(reply.Raw, from); err != nil {
			t.Fatal(err)
		}
		_ = relay.SetReadDeadline(time.Now().Add(time.Second))
		_, src, err := relay.ReadFrom(buf)
		if err != nil {
			t.Fatalf("answer %d: %v", i, err)
		}
		want := at(public[1-i])
		if src.String() != want.String() {
			t.Fatalf("an answer to a check sent to %v came from %v", want, src)
		}
	}

	// Data comes back from the address the client last sent data to, whatever it checked since.
	if _, err := relay.WriteTo([]byte("media"), at(public[0])); err != nil {
		t.Fatal(err)
	}
	if _, err := relay.WriteTo(stun.MustBuild(stun.TransactionID, stun.BindingRequest).Raw, at(public[1])); err != nil {
		t.Fatal(err)
	}
	if got := received(t, f.sfu, 300*time.Millisecond); len(got) != 2 || string(got[0]) != "media" {
		t.Fatalf("the SFU heard %q", got)
	}
	if _, err := f.sfu.WriteTo([]byte("media back"), from); err != nil {
		t.Fatal(err)
	}
	_ = relay.SetReadDeadline(time.Now().Add(time.Second))
	n, src, err := relay.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "media back" || src.String() != at(public[0]).String() {
		t.Fatalf("read %q from %v: %v", buf[:n], src, err)
	}

	// An address the SFU does not announce is no peer, and neither is another port of one it does.
	if _, err := relay.WriteTo([]byte("x"), at(netip.MustParseAddr("192.0.2.12"))); err == nil {
		t.Error("a permission for an address the SFU does not announce was granted")
	}
	_, _ = relay.WriteTo([]byte("another port"), net.UDPAddrFromAddrPort(netip.AddrPortFrom(public[0], uint16(port+1))))
	if got := received(t, f.sfu, 300*time.Millisecond); len(got) != 0 {
		t.Fatalf("the SFU heard %q", got)
	}
}

// TestAnSFUOnEveryInterfaceIsKnownByEachOfThem: with nothing announced in the relay's family, the SFU is
// listening on every interface and announces each, so each of this host's addresses reaches it. That
// includes public addresses all of the other family, which Pion's rewrite leaves this family's alone for;
// before /code-review found it, those made every address of the relay's family unknown.
func TestAnSFUOnEveryInterfaceIsKnownByEachOfThem(t *testing.T) {
	var host netip.Addr
	ifaddrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, ia := range ifaddrs {
		if n, ok := ia.(*net.IPNet); ok {
			if a, ok := netip.AddrFromSlice(n.IP); ok && a.Unmap().Is4() && !a.IsLoopback() {
				host = a.Unmap()
				break
			}
		}
	}
	if !host.IsValid() {
		t.Skip("no IPv4 address here but loopback")
	}
	for name, announced := range map[string][]netip.Addr{
		"nothing announced":       nil,
		"only the other family's": {netip.MustParseAddr("2001:db8::7")},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, func(o *Options) { o.Announced = announced })
			relay, err := f.client(t, cred(1)).Allocate()
			if err != nil {
				t.Fatal(err)
			}
			to := net.UDPAddrFromAddrPort(netip.AddrPortFrom(host, uint16(f.sfuAddr().(*net.UDPAddr).Port)))
			if _, err := relay.WriteTo([]byte("to the SFU"), to); err != nil {
				t.Fatalf("to %v: %v", to, err)
			}
			if got := received(t, f.sfu, time.Second); len(got) != 1 || string(got[0]) != "to the SFU" {
				t.Fatalf("the SFU heard %q", got)
			}
		})
	}
}

// TestAClientReachingTURNOverIPv6IsServed: pion gives an allocation that names no family the family of
// the address the client reached TURN on, and pion's own client names IPv6 whenever its socket is IPv6.
// An IPv4 relay alone refused both. Now a client naming no family gets the IPv4 relay, and a server
// relaying in both families gives one naming IPv6 an IPv6 relay.
func TestAClientReachingTURNOverIPv6IsServed(t *testing.T) {
	if pc, err := net.ListenPacket("udp6", "[::1]:0"); err != nil {
		t.Skip("no IPv6 loopback here:", err)
	} else {
		_ = pc.Close()
	}
	for name, c := range map[string]struct {
		both   bool
		family turn.RequestedAddressFamily
		want4  bool
	}{
		"naming no family, IPv4 relay alone": {false, turn.RequestedAddressFamilyIPv4, true},
		"naming IPv6, both families":         {true, turn.RequestedAddressFamilyIPv6, false},
	} {
		t.Run(name, func(t *testing.T) {
			udp, err := net.ListenPacket("udp6", "[::1]:0")
			if err != nil {
				t.Fatal(err)
			}
			sfu, err := net.ListenPacket("udp", ":0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sfu.Close() }()
			port := uint16(sfu.LocalAddr().(*net.UDPAddr).Port)
			opts := Options{
				UDP:    udp,
				Secret: testSecret,
				Relay:  netip.MustParseAddr("127.0.0.1"),
				SFU:    netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port),
			}
			if c.both {
				opts.OtherRelay = netip.IPv6Loopback()
				opts.OtherSFU = netip.AddrPortFrom(netip.IPv6Loopback(), port)
			}
			s, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			conn, err := net.ListenPacket("udp6", "[::1]:0")
			if err != nil {
				t.Fatal(err)
			}
			cr := cred(1)
			client, err := turn.NewClient(&turn.ClientConfig{
				STUNServerAddr: udp.LocalAddr().String(), TURNServerAddr: udp.LocalAddr().String(),
				Conn: conn, Username: cr.Username, Password: cr.Password, Realm: Realm,
				RequestedAddressFamily: c.family,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Listen(); err != nil {
				t.Fatal(err)
			}
			defer func() { client.Close(); _ = conn.Close() }()
			relay, err := client.Allocate()
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if got4 := relay.LocalAddr().(*net.UDPAddr).IP.To4() != nil; got4 != c.want4 {
				t.Fatalf("the relay is %v", relay.LocalAddr())
			}
			// And it reaches the SFU, in that family.
			to := net.UDPAddrFromAddrPort(netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port))
			if !c.want4 {
				to = net.UDPAddrFromAddrPort(netip.AddrPortFrom(netip.IPv6Loopback(), port))
			}
			if _, err := relay.WriteTo([]byte("to the SFU"), to); err != nil {
				t.Fatal(err)
			}
			if got := received(t, sfu, time.Second); len(got) != 1 || string(got[0]) != "to the SFU" {
				t.Fatalf("the SFU heard %q", got)
			}
		})
	}
}

func TestARelayAddressThisHostDoesNotHaveFailsTheStart(t *testing.T) {
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = udp.Close() }()
	for name, ports := range map[string][2]uint16{"any port": {}, "a range": {50000, 50010}} {
		_, err := New(Options{
			UDP:        udp,
			Secret:     testSecret,
			Relay:      netip.MustParseAddr("192.0.2.1"),
			RelayPorts: ports,
			SFU:        netip.MustParseAddrPort("192.0.2.1:3479"),
		})
		if err == nil {
			t.Errorf("%s: a server started with a relay address this host does not have", name)
		}
	}
}

// TestAChannelToAnythingButTheSFUEndsTheAllocation: pion admits a channel on the peer's IP alone and
// keeps every binding in a list it walks for each relayed packet, so one allocation binding every port
// of the SFU's address made each packet cost thousands of comparisons. A client in a call binds the
// SFU's addresses only.
func TestAChannelToAnythingButTheSFUEndsTheAllocation(t *testing.T) {
	f := newFixture(t, nil)
	relay, err := f.client(t, cred(1)).Allocate()
	if err != nil {
		t.Fatal(err)
	}
	// The SFU's own address and port: pion's client binds a channel for it, and the allocation stays.
	for range 3 {
		if _, err := relay.WriteTo([]byte("to the SFU"), f.sfuAddr()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := f.s.AllocationCount(); n != 1 {
		t.Fatalf("binding the SFU's address ended the allocation: %d left", n)
	}
	// Another port of the same address, which the permission admits.
	other := net.UDPAddrFromAddrPort(netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"),
		uint16(f.sfuAddr().(*net.UDPAddr).Port+1)))
	for range 3 {
		_, _ = relay.WriteTo([]byte("elsewhere"), other)
		time.Sleep(100 * time.Millisecond)
	}
	eventually(t, 2*time.Second, "the allocation that bound another port ends", func() bool {
		return f.s.AllocationCount() == 0
	})
}
