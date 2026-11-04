// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"strings"
)

// offered is the gathered SDP as the worker sends it: each candidate once, all in the first media section,
// at most max of them, relays first, then server-reflexive, then host, each kind in the order gathered.
//
// The SFU counts candidate lines across every section and refuses an offer past voiceproto.MaxCandidates,
// and a machine with a few interfaces in both families, docker's bridges or a VPN among them, gathers more:
// ten here, each written twice by Pion, was twenty. Pion reads a remote offer's candidates from its first
// bundled section alone, so that is the one section that needs them. Relays come first because a client
// that needs one has nothing else that works; a host candidate left out is still used by this side's own
// checks, which the SFU learns from as peer-reflexive.
//
// Only what is sent is trimmed: the local description stays as gathered.
func offered(sdp string, max int) string {
	lines := strings.Split(strings.TrimSuffix(sdp, "\r\n"), "\r\n")
	var (
		kept       []string
		candidates [4][]string // by rank: relay, srflx, prflx, host
		seen       = map[string]bool{}
		firstEnd   = -1 // index in kept where the first media section ends
		sections   = 0
	)
	for _, l := range lines {
		if strings.HasPrefix(l, "m=") {
			sections++
			if sections == 2 {
				firstEnd = len(kept)
			}
		}
		switch {
		case strings.HasPrefix(l, "a=candidate:"):
			if !seen[l] {
				seen[l] = true
				r := rank(l)
				candidates[r] = append(candidates[r], l)
			}
		case l == "a=end-of-candidates":
		default:
			kept = append(kept, l)
		}
	}
	if firstEnd < 0 {
		firstEnd = len(kept)
	}
	var chosen []string
	for _, group := range candidates {
		for _, c := range group {
			if len(chosen) < max {
				chosen = append(chosen, c)
			}
		}
	}
	if len(chosen) > 0 {
		chosen = append(chosen, "a=end-of-candidates")
	}
	out := make([]string, 0, len(kept)+len(chosen))
	out = append(out, kept[:firstEnd]...)
	out = append(out, chosen...)
	out = append(out, kept[firstEnd:]...)
	return strings.Join(out, "\r\n") + "\r\n"
}

// rank orders a candidate line by its type.
func rank(line string) int {
	switch {
	case strings.Contains(line, " typ relay"):
		return 0
	case strings.Contains(line, " typ srflx"):
		return 1
	case strings.Contains(line, " typ prflx"):
		return 2
	}
	return 3
}
