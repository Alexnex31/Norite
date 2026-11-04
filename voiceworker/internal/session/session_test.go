// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/voiceipc"
)

// answered makes a session's offer and applies an answer to it that names extra as its candidates, as a
// voice server's answer would. It returns the session, to read what its ICE agent kept.
func answered(t *testing.T, join voiceipc.Join, extra []string) *Session {
	t.Helper()
	s, err := New(join, Options{Version: "0.4.0", Log: io.Discard})
	require.NoError(t, err)
	t.Cleanup(s.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	offer, err := s.Offer(ctx)
	require.NoError(t, err)

	peer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })
	require.NoError(t, peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}))
	answer, err := peer.CreateAnswer(nil)
	require.NoError(t, err)

	// The candidates go in the first section, after its ICE password, where the SFU writes its own.
	lines := strings.Split(answer.SDP, "\r\n")
	var out []string
	placed := false
	for _, l := range lines {
		out = append(out, l)
		if !placed && strings.HasPrefix(l, "a=ice-pwd:") {
			out = append(out, extra...)
			placed = true
		}
	}
	require.True(t, placed, "the answer has no ICE password to place candidates after")
	require.NoError(t, s.Accept(strings.Join(out, "\r\n")))
	return s
}

func remoteCandidate(i int, ip string) string {
	return fmt.Sprintf("a=candidate:%d 1 udp 2130706431 %s %d typ host", i, ip, 40000+i)
}

// remoteIPs is what the session's ICE agent holds as remote candidates, by address.
func remoteIPs(s *Session) map[string]int {
	ips := map[string]int{}
	for _, st := range s.pc.GetStats() {
		if c, ok := st.(webrtc.ICECandidateStats); ok && c.Type == webrtc.StatsTypeRemoteCandidate {
			ips[c.IP]++
		}
	}
	return ips
}

// An answer naming this host's loopback, its link, a broadcast or a metadata service is not taken up: the
// worker would send its checks there, on the voice server's say-so. A private address stays, which is
// where a LAN instance's SFU is.
func TestAnAnswerCannotSendTheWorkerToItsOwnMachineOrLink(t *testing.T) {
	refused := []string{"127.0.0.1", "::1", "169.254.169.254", "169.254.1.1", "fe80::1", "fd00:ec2::254",
		"100.100.100.200", "0.0.0.0", "255.255.255.255", "224.0.0.1"}
	kept := []string{"192.0.2.10", "10.1.2.3", "2001:db8::10"}
	var lines []string
	for i, ip := range append(append([]string{}, refused...), kept...) {
		lines = append(lines, remoteCandidate(i+1, ip))
	}
	s := answered(t, voiceipc.Join{ICEServers: []voiceipc.ICEServer{}}, lines)
	require.Eventually(t, func() bool { return len(remoteIPs(s)) == len(kept) }, 5*time.Second, 20*time.Millisecond,
		"%v", remoteIPs(s))
	got := remoteIPs(s)
	for _, ip := range kept {
		require.Contains(t, got, ip)
	}
	for _, ip := range refused {
		require.NotContains(t, got, ip)
	}
}

// A join for a voice server on this machine admits loopback, and nothing else changes.
func TestAJoinForALocalVoiceServerAdmitsLoopback(t *testing.T) {
	s := answered(t, voiceipc.Join{ICEServers: []voiceipc.ICEServer{}, Loopback: true},
		[]string{remoteCandidate(1, "127.0.0.1"), remoteCandidate(2, "169.254.169.254")})
	require.Eventually(t, func() bool { return remoteIPs(s)["127.0.0.1"] > 0 }, 5*time.Second, 20*time.Millisecond)
	require.NotContains(t, remoteIPs(s), "169.254.169.254")
}

// An answer naming more candidates than any SFU announces is taken up to the bound and no further.
func TestAnAnswerIsTakenUpToTheBound(t *testing.T) {
	var lines []string
	for i := range 200 {
		lines = append(lines, remoteCandidate(i+1, net.IPv4(192, 0, 2, byte(i+1)).String()))
	}
	s := answered(t, voiceipc.Join{ICEServers: []voiceipc.ICEServer{}}, lines)
	require.Eventually(t, func() bool { return len(remoteIPs(s)) > 0 }, 5*time.Second, 20*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	require.LessOrEqual(t, len(remoteIPs(s)), maxRemotes)
}

func TestReachable(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1": false, "::ffff:127.0.0.1": false, "::": false, "0.1.2.3": false,
		"192.0.2.1": true, "10.0.0.1": true, "100.64.0.1": true, "2001:db8::1": true,
		"::ffff:169.254.169.254": false,
	} {
		require.Equal(t, want, reachable(net.ParseIP(ip), false), ip)
	}
	require.True(t, reachable(net.ParseIP("127.0.0.1"), true))
}
