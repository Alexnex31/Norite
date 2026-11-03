// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/internal/voice/turn"
	"github.com/Alexnex31/Norite/backend/internal/voice/voicetest"
)

var testSigningKey = []byte("a signing key for tests, long enough to be one")

func stackOptions(t *testing.T) StackOptions {
	t.Helper()
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "turn.pem"), filepath.Join(dir, "turn.key")
	der, err := x509.MarshalPKCS8PrivateKey(testCertificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: testCertificate.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	loopback := netip.MustParseAddr("127.0.0.1")
	return StackOptions{
		MediaListen:   "127.0.0.1:0",
		TURNListen:    freePort(t),
		TURNTLSListen: "127.0.0.1:0",
		TLSCert:       cert,
		TLSKey:        key,
		Relay:         loopback,
		SFU:           netip.AddrPortFrom(loopback, 0),
		SigningKey:    testSigningKey,
		Loopback:      true,
		Version:       "dev",
		Logger:        zerolog.Nop(),
	}
}

// freePort is a loopback address whose port is free on UDP and TCP alike, as TURN's must be.
func freePort(t *testing.T) string {
	t.Helper()
	for range 20 {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		if pc, err := net.ListenPacket("udp4", addr); err == nil {
			_ = pc.Close()
			return addr
		}
	}
	t.Fatal("no port free on both")
	return ""
}

func TestTheStackRelaysToItsOwnSFU(t *testing.T) {
	st, err := StartStack(stackOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	user := ids.MustNext()
	cred := turn.Mint(st.Secret, user, time.Now())
	a := joinWith(t, st.SFU, 1, user, voicetest.Options{Slots: 1, RelayOnly: true, ICEServers: []webrtc.ICEServer{{
		URLs:     []string{"turn:" + st.TURN.TCPAddr().String() + "?transport=tcp"},
		Username: cred.Username, Credential: cred.Password,
	}}})
	b := join(t, st.SFU, 1, ids.MustNext(), 1)
	tone := voicetest.TonePackets()[:50]
	send(t, map[*voicetest.Client][][]byte{a.client: tone, b.client: tone}, voicetest.SendOptions{Level: voicetest.Loud})
	assertTone(t, "the relayed client", a.client, tone)
	assertTone(t, "the direct client", b.client, tone)
}

// TestAStackThatCannotStartLeavesNothingBound starts a stack whose TURN port is taken: it fails, and the
// media port it had bound is free again.
func TestAStackThatCannotStartLeavesNothingBound(t *testing.T) {
	opts := stackOptions(t)
	taken, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	opts.MediaListen = freePort(t)
	opts.TURNListen = taken.Addr().String()
	if _, err := StartStack(opts); err == nil {
		t.Fatal("a stack started on a port in use")
	}
	pc, err := net.ListenPacket("udp4", opts.MediaListen)
	if err != nil {
		t.Fatalf("the media port is still held: %v", err)
	}
	_ = pc.Close()
}

// TestARelayedClientReachesAnSFUByWhatItAnnounces is the relay as an instance runs it: the SFU announces
// a public address the host does not have, or listens on every interface and announces each, and the
// relay reaches it on the host all the same. Both clients are relayed, since the public address leads
// nowhere. Before /code-review found it, the relay worked only where the SFU announced the one address it
// was bound to, which is what every earlier test and the manual pass had configured.
func TestARelayedClientReachesAnSFUByWhatItAnnounces(t *testing.T) {
	for name, tune := range map[string]func(*StackOptions){
		"a public address": func(o *StackOptions) {
			o.Public = []netip.Addr{netip.MustParseAddr("192.0.2.10")}
		},
		"every interface": func(o *StackOptions) {
			if !hasInterfaceAddress() {
				t.Skip("no IPv4 address here but loopback")
			}
			o.MediaListen = "0.0.0.0:0"
			o.Loopback = false
		},
	} {
		t.Run(name, func(t *testing.T) {
			opts := stackOptions(t)
			tune(&opts)
			st, err := StartStack(opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			relayed := func() joined {
				user := ids.MustNext()
				cred := turn.Mint(st.Secret, user, time.Now())
				return joinWith(t, st.SFU, 1, user, voicetest.Options{Slots: 1, RelayOnly: true, ICEServers: []webrtc.ICEServer{{
					URLs:     []string{"turn:" + st.TURN.TCPAddr().String() + "?transport=tcp"},
					Username: cred.Username, Credential: cred.Password,
				}}})
			}
			a, b := relayed(), relayed()
			tone := voicetest.TonePackets()[:50]
			send(t, map[*voicetest.Client][][]byte{a.client: tone, b.client: tone}, voicetest.SendOptions{Level: voicetest.Loud})
			assertTone(t, "the first", a.client, tone)
			assertTone(t, "the second", b.client, tone)
		})
	}
}

func hasInterfaceAddress() bool {
	ifaddrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, ia := range ifaddrs {
		if n, ok := ia.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() {
			return true
		}
	}
	return false
}

// TestAStackThatClosesSaysGoodbyeThroughTheRelay: the SFU's goodbye to a relayed client crosses the
// relay, so the SFU closes first. TURN first, the client heard nothing and waited for ICE to give up.
func TestAStackThatClosesSaysGoodbyeThroughTheRelay(t *testing.T) {
	st, err := StartStack(stackOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	user := ids.MustNext()
	cred := turn.Mint(st.Secret, user, time.Now())
	a := joinWith(t, st.SFU, 1, user, voicetest.Options{Slots: 1, RelayOnly: true, ICEServers: []webrtc.ICEServer{{
		URLs:     []string{"turn:" + st.TURN.UDPAddr().String() + "?transport=udp"},
		Username: cred.Username, Credential: cred.Password,
	}}})
	start := time.Now()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.client.ChannelClosed():
	case <-a.client.Failed():
	case <-time.After(2 * time.Second):
		t.Fatal("the relayed client heard nothing when the stack closed")
	}
	t.Logf("the relayed client knew within %v", time.Since(start))
}

// TestTheStackTellsTURNWhatTheSFUAnnounces: Pion rewrites only the host candidates a public address is
// given for, so a family the public addresses leave out is announced as bound.
func TestTheStackTellsTURNWhatTheSFUAnnounces(t *testing.T) {
	v4, v6 := netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("2001:db8::7")
	bound, every := netip.MustParseAddr("10.0.0.5"), netip.IPv4Unspecified()
	for name, c := range map[string]struct {
		public []netip.Addr
		bound  netip.Addr
		want   []netip.Addr
	}{
		"public, both families":          {[]netip.Addr{v4, v6}, bound, []netip.Addr{v4, v6}},
		"public in the other family":     {[]netip.Addr{v6}, bound, []netip.Addr{v6, bound}},
		"public in the other, every one": {[]netip.Addr{v6}, every, []netip.Addr{v6}},
		"none, bound":                    {nil, bound, []netip.Addr{bound}},
		"none, every interface":          {nil, every, nil},
	} {
		if got := announced(c.public, c.bound); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

// TestRepliesFollowWhatOneAddressMayHold: every client's first Allocate on a transport draws a
// challenge, and an office raising relay_per_address had them dropped at twenty a second.
func TestRepliesFollowWhatOneAddressMayHold(t *testing.T) {
	if got := stackLimits(0).RepliesPerSecond; got != turn.DefaultLimits().RepliesPerSecond {
		t.Fatalf("the default replies bound is %d", got)
	}
	if got := stackLimits(500); got.RepliesPerSecond != 500 || got.AllocationsPerSource != 500 || got.StreamsPerSource != 500 {
		t.Fatalf("relay_per_address 500 gave %+v", got)
	}
}

// TestAClientReachingTURNOverIPv6JoinsTheCall: a client whose network gives it IPv6 asks for an IPv6
// relay, Pion's client included, and an SFU on every interface is reached on ::1 as well as 127.0.0.1.
func TestAClientReachingTURNOverIPv6JoinsTheCall(t *testing.T) {
	probe, err := net.ListenPacket("udp6", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback here:", err)
	}
	turnAddr := probe.LocalAddr().String()
	_ = probe.Close()
	opts := stackOptions(t)
	opts.MediaListen = ":0"
	opts.TURNListen = turnAddr
	opts.TURNTLSListen, opts.TLSCert, opts.TLSKey = "", "", ""
	opts.OtherRelay = netip.IPv6Loopback()
	opts.OtherSFU = netip.AddrPortFrom(netip.IPv6Loopback(), 0)
	st, err := StartStack(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// The SFU admits the IPv6 relay as a remote candidate, as it does the IPv4 one, outside tests too:
	// this test admits loopback anyway, so it would pass without.
	if !slices.Contains(st.SFU.opts.Relays, netip.IPv6Loopback()) {
		t.Fatalf("the SFU admits relays %v", st.SFU.opts.Relays)
	}

	user := ids.MustNext()
	cred := turn.Mint(st.Secret, user, time.Now())
	a := joinWith(t, st.SFU, 1, user, voicetest.Options{Slots: 1, RelayOnly: true, IPv6: true, ICEServers: []webrtc.ICEServer{{
		URLs:     []string{"turn:" + turnAddr + "?transport=udp"},
		Username: cred.Username, Credential: cred.Password,
	}}})
	b := join(t, st.SFU, 1, ids.MustNext(), 1)
	tone := voicetest.TonePackets()[:50]
	send(t, map[*voicetest.Client][][]byte{a.client: tone, b.client: tone}, voicetest.SendOptions{Level: voicetest.Loud})
	assertTone(t, "the relayed client", a.client, tone)
	assertTone(t, "the direct client", b.client, tone)
}
