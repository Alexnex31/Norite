// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/transport/v5/stdnet"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/voiceipc"
)

// Each way in refuses what a voice server cannot have, after resolving: the agent's TURN and STUN servers
// are the instance's words since M33.
func TestTheAgentDialsNothingAVoiceServerCannotHave(t *testing.T) {
	host, err := stdnet.NewNet()
	require.NoError(t, err)
	refuses := guardedNet{Net: host}
	admits := guardedNet{Net: host, loopback: true}

	for _, addr := range []string{"169.254.169.254:3478", "127.0.0.1:3478", "[::1]:3478", "[fe80::1]:3478",
		"0.0.0.0:3478", "100.100.100.200:3478"} {
		_, err := refuses.ResolveUDPAddr("udp", addr)
		require.ErrorIs(t, err, errRefusedAddress, "udp %s", addr)
		_, err = refuses.ResolveTCPAddr("tcp", addr)
		require.ErrorIs(t, err, errRefusedAddress, "tcp %s", addr)
		_, err = refuses.Dial("tcp", addr)
		require.ErrorIs(t, err, errRefusedAddress, "dial %s", addr)
		_, err = refuses.CreateDialer(&net.Dialer{}).Dial("tcp", addr)
		require.ErrorIs(t, err, errRefusedAddress, "a dialer's %s", addr)
	}
	_, err = refuses.Dial("tcp", "localhost:3478")
	require.ErrorIs(t, err, errRefusedAddress, "a name is judged on what it resolves to")
	_, err = refuses.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(169, 254, 169, 254), Port: 3478})
	require.ErrorIs(t, err, errRefusedAddress)
	_, err = refuses.DialTCP("tcp4", nil, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 3478})
	require.ErrorIs(t, err, errRefusedAddress)

	// A LAN instance's server, and loopback when the join admits it, are dialed.
	_, err = refuses.ResolveUDPAddr("udp", "192.168.1.10:3478")
	require.NoError(t, err)
	_, err = admits.ResolveUDPAddr("udp", "127.0.0.1:3478")
	require.NoError(t, err)
}

// The agent's TURN gathering goes through the guard: an answer naming a TURN server on loopback, to a join
// that does not admit loopback, sends it nothing, and the same join admitting loopback does. The second half
// is what shows the first is the guard and not a gather that never ran.
func TestAnInstancesTURNServerIsDialedThroughTheGuard(t *testing.T) {
	for _, loopback := range []bool{false, true} {
		turn, err := net.ListenPacket("udp4", "127.0.0.1:0")
		require.NoError(t, err)
		var got atomic.Int32
		go func() {
			buf := make([]byte, 1500)
			for {
				if _, _, err := turn.ReadFrom(buf); err != nil {
					return
				}
				got.Add(1)
			}
		}()

		s, err := New(voiceipc.Join{Loopback: loopback}, Options{Version: "0.4.0", Log: io.Discard})
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		offer, err := s.Offer(ctx)
		cancel()
		require.NoError(t, err)
		peer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
		require.NoError(t, err)
		require.NoError(t, peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}))
		answer, err := peer.CreateAnswer(nil)
		require.NoError(t, err)
		require.NoError(t, s.Accept(voiceipc.Answer{SDP: answer.SDP, ICEServers: []voiceipc.ICEServer{{
			URLs: []string{"turn:" + turn.LocalAddr().String() + "?transport=udp"}, Username: "1:1", Credential: "x",
		}}}))

		time.Sleep(time.Second)
		if loopback {
			require.Positive(t, got.Load(), "the control: an admitted TURN server was sent nothing")
		} else {
			require.Zero(t, got.Load(), "a TURN server on loopback was dialed by a join that does not admit it")
		}
		s.Close()
		_ = peer.Close()
		_ = turn.Close()
	}
}
