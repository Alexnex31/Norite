// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pion/sdp/v3"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/voiceproto"
)

func candidate(i int, typ string) string {
	return fmt.Sprintf("a=candidate:%d 1 udp %d 192.0.2.%d %d typ %s", i, 2000-i, i, 40000+i, typ)
}

// gathered is an offer shaped as Pion writes one: three sections, the candidates in two of them.
func gathered(hosts, relays int) string {
	var cands []string
	for i := range hosts {
		cands = append(cands, candidate(i+1, "host"))
	}
	for i := range relays {
		cands = append(cands, candidate(100+i, "relay")+" raddr 0.0.0.0 rport 0")
	}
	section := func(m string, withCandidates bool) string {
		s := m + "\r\nc=IN IP4 0.0.0.0\r\na=mid:x\r\na=ice-ufrag:u\r\na=ice-pwd:p\r\n"
		if withCandidates && len(cands) > 0 {
			s += strings.Join(cands, "\r\n") + "\r\na=end-of-candidates\r\n"
		}
		return s
	}
	return "v=0\r\no=- 1 2 IN IP4 0.0.0.0\r\ns=-\r\nt=0 0\r\na=group:BUNDLE 0 1 2\r\n" +
		section("m=audio 9 UDP/TLS/RTP/SAVPF 111", true) +
		section("m=audio 9 UDP/TLS/RTP/SAVPF 111", false) +
		section("m=application 9 UDP/DTLS/SCTP webrtc-datachannel", true)
}

func candidatesBySection(t *testing.T, raw string) [][]string {
	t.Helper()
	var d sdp.SessionDescription
	require.NoError(t, d.UnmarshalString(raw))
	var out [][]string
	for _, m := range d.MediaDescriptions {
		var cs []string
		for _, a := range m.Attributes {
			if a.IsICECandidate() {
				cs = append(cs, a.Value)
			}
		}
		out = append(out, cs)
	}
	return out
}

// Ten candidates written twice were twenty lines, and the SFU refused the offer: each is sent once, in the
// first section, which is where Pion reads them.
func TestEachCandidateIsOfferedOnceInTheFirstSection(t *testing.T) {
	out := offered(gathered(8, 2), voiceproto.MaxCandidates)
	got := candidatesBySection(t, out)
	require.Len(t, got, 3, "every section survives")
	require.Len(t, got[0], 10)
	require.Empty(t, got[1])
	require.Empty(t, got[2])
	require.Equal(t, 1, strings.Count(out, "a=end-of-candidates"))
}

// Past the bound, relays are kept first: a client that needs one has nothing else that works.
func TestPastTheBoundRelaysAreKept(t *testing.T) {
	out := offered(gathered(30, 3), voiceproto.MaxCandidates)
	got := candidatesBySection(t, out)[0]
	require.Len(t, got, voiceproto.MaxCandidates)
	for _, c := range got[:3] {
		require.Contains(t, c, "typ relay")
	}
	require.Contains(t, got[3], " 192.0.2.1 ", "hosts follow in the order gathered")
}

func TestAnOfferWithNoCandidatesIsLeftWhole(t *testing.T) {
	in := gathered(0, 0)
	require.Equal(t, in, offered(in, voiceproto.MaxCandidates))
}
