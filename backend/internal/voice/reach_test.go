// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/pion/webrtc/v4"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// TestTheSFUSendsNoCheckToAnOfferedAddress offers a candidate at a socket the test holds. A full ICE agent
// sends it checks within a second; the SFU is lite, and sends none.
func TestTheSFUSendsNoCheckToAnOfferedAddress(t *testing.T) {
	s := newSFU(t, nil)
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.LocalAddr().(*net.UDPAddr).Port

	offer := offerOf(t, newClient(t, 1))
	planted := fmt.Sprintf("a=candidate:9 1 udp 2130706431 127.0.0.1 %d typ host\r\n", port)
	i := strings.Index(offer, "a=candidate:")
	if i < 0 {
		t.Fatal("the offer has no candidate")
	}
	offer = offer[:i] + planted + offer[i:]
	if _, err := s.Provision(context.Background(), ids.MustNext(), ids.MustNext(), ids.MustNext(), ids.MustNext(),
		offer); err != nil {
		t.Fatal(err)
	}

	_ = listener.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	if n, from, err := listener.ReadFrom(buf); err == nil {
		t.Fatalf("the SFU sent %d bytes from %v to an address the offer named", n, from)
	}
}

// TestAnOfferClaimingLiteIsSentNothing: a lite SFU and a client claiming to be lite too would select a
// pair without a single check, Pion's lite selector taking the offer's best candidate as it stands, and a
// DTLS client would then send its handshake there. Two guards, each enough alone: the offer is refused,
// and the SFU is the DTLS server, which sends nothing first.
func TestAnOfferClaimingLiteIsSentNothing(t *testing.T) {
	s := newSFU(t, nil)
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.LocalAddr().(*net.UDPAddr).Port

	offer := offerOf(t, newClient(t, 1))
	planted := fmt.Sprintf("a=candidate:9 1 udp 2130706431 127.0.0.1 %d typ host\r\n", port)
	i := strings.Index(offer, "a=candidate:")
	if i < 0 || !strings.Contains(offer, "\r\nt=0 0\r\n") {
		t.Fatal("the offer is not shaped as this test expects")
	}
	// The planted candidate alone, so that it is the best pair: what an offer aiming the SFU would send.
	var kept []string
	for _, l := range strings.Split(offer[:i]+planted+offer[i:], "\r\n") {
		if !strings.HasPrefix(l, "a=candidate:") || strings.HasPrefix(l, "a=candidate:9 ") {
			kept = append(kept, l)
		}
	}
	offer = strings.Replace(strings.Join(kept, "\r\n"), "\r\nt=0 0\r\n", "\r\nt=0 0\r\na=ice-lite\r\n", 1)
	_, _ = s.Provision(context.Background(), ids.MustNext(), ids.MustNext(), ids.MustNext(), ids.MustNext(), offer)

	_ = listener.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	if n, from, err := listener.ReadFrom(buf); err == nil {
		t.Fatalf("the SFU sent %d bytes from %v to an address an offer claiming lite named", n, from)
	}
}

func TestAClientsAddressIsOneAClientCanHave(t *testing.T) {
	s := &SFU{opts: Options{Relay: netip.MustParseAddr("127.0.0.2")}}
	for addr, want := range map[string]bool{
		"203.0.113.7":            true,
		"2001:db8::7":            true,
		"192.168.1.20":           true, // a LAN instance's clients are private
		"10.0.0.4":               true,
		"fd12::4":                true,
		"100.64.0.9":             true, // carrier-grade NAT
		"127.0.0.1":              false,
		"::1":                    false,
		"127.0.0.2":              true, // the relay
		"0.0.0.0":                false,
		"0.1.2.3":                false,
		"::":                     false,
		"255.255.255.255":        false,
		"224.0.0.251":            false,
		"ff02::fb":               false,
		"169.254.10.1":           false,
		"fe80::1":                false,
		"169.254.169.254":        false,
		"fd00:ec2::254":          false,
		"100.100.100.200":        false,
		"::ffff:169.254.169.254": false,
	} {
		if got := s.reachable(net.ParseIP(addr)); got != want {
			t.Errorf("%s: reachable %v, want %v", addr, got, want)
		}
	}
	s.opts.Loopback = true
	if !s.reachable(net.ParseIP("127.0.0.1")) {
		t.Error("loopback refused with Loopback set")
	}
}

// TestAnAddressNoClientCanHaveIsNeverACandidate sends authenticated checks from loopback to an SFU that does
// not admit it: none becomes a remote candidate, where the same checks to one that does admit it do.
func TestAnAddressNoClientCanHaveIsNeverACandidate(t *testing.T) {
	for _, loopback := range []bool{true, false} {
		s := newSFUWith(t, func(o *Options) { o.Loopback = loopback })
		connID := ids.MustNext()
		offer := offerOf(t, newClient(t, 1))
		answer, err := s.Provision(context.Background(), ids.MustNext(), ids.MustNext(), connID, ids.MustNext(), offer)
		if err != nil {
			t.Fatal(err)
		}
		sendChecks(t, s, offer, answer, 4)
		pc := peerOf(s, connID)
		// The offer's own candidates count too: on loopback, and on whatever private interfaces the
		// machine has, which the filter rightly admits.
		onLoopback := func() int {
			return remoteCandidatesWhere(pc, func(ip string) bool { return net.ParseIP(ip).IsLoopback() })
		}
		if loopback {
			eventually(t, 5*time.Second, "checks from loopback make candidates", func() bool {
				return onLoopback() > 0
			})
			continue
		}
		time.Sleep(time.Second)
		if n := onLoopback(); n != 0 {
			t.Fatalf("checks from loopback left %d remote candidates on an SFU that does not admit it", n)
		}
	}
}

// sendChecks sends authenticated binding requests to the SFU's port, one from each of n loopback sockets,
// with the ICE credentials of the offer and the answer: what a peer can do, knowing both.
func sendChecks(t *testing.T, s *SFU, offer, answer string, n int) {
	t.Helper()
	attr := func(sdp, key string) string {
		for line := range strings.SplitSeq(sdp, "\r\n") {
			if v, ok := strings.CutPrefix(line, "a="+key+":"); ok {
				return v
			}
		}
		t.Fatalf("no a=%s", key)
		return ""
	}
	username := attr(answer, "ice-ufrag") + ":" + attr(offer, "ice-ufrag")
	password := attr(answer, "ice-pwd")
	target := s.opts.Conn.LocalAddr()
	for range n {
		pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pc.Close() })
		msg, err := stun.Build(stun.TransactionID, stun.BindingRequest, stun.NewUsername(username),
			stun.NewShortTermIntegrity(password), stun.Fingerprint)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pc.WriteTo(msg.Raw, target); err != nil {
			t.Fatal(err)
		}
	}
}

// peerOf is the connection's peer connection on the SFU's side.
func peerOf(s *SFU, connID snowflake.ID) *webrtc.PeerConnection {
	s.mu.Lock()
	c := s.conns[connID]
	s.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pc
}

func remoteCandidateCount(pc *webrtc.PeerConnection) int {
	return remoteCandidatesWhere(pc, func(string) bool { return true })
}

// remoteCandidatesWhere counts the peer's remote candidates whose address passes keep.
func remoteCandidatesWhere(pc *webrtc.PeerConnection, keep func(ip string) bool) int {
	n := 0
	for _, st := range pc.GetStats() {
		if cs, ok := st.(webrtc.ICECandidateStats); ok && cs.Type == webrtc.StatsTypeRemoteCandidate && keep(cs.IP) {
			n++
		}
	}
	return n
}

// TestTheSFUAnnouncesItsPublicAddress: behind a 1:1 NAT the bound address is one no client reaches, so
// the answer names the configured public one instead.
func TestTheSFUAnnouncesItsPublicAddress(t *testing.T) {
	public := netip.MustParseAddr("203.0.113.7")
	s := newSFUWith(t, func(o *Options) { o.Public = []netip.Addr{public} })
	answer, err := s.Provision(context.Background(), ids.MustNext(), ids.MustNext(), ids.MustNext(), ids.MustNext(),
		offerOf(t, newClient(t, 1)))
	if err != nil {
		t.Fatal(err)
	}
	port := s.opts.Conn.LocalAddr().(*net.UDPAddr).Port
	if !strings.Contains(answer, fmt.Sprintf(" %s %d typ host", public, port)) {
		t.Fatalf("the answer does not announce %s:%d:\n%s", public, port, answer)
	}
	if strings.Contains(answer, " 127.0.0.1 ") {
		t.Fatal("the answer still announces the bound address")
	}
}
