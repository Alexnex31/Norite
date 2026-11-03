// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"errors"
	"strings"
	"testing"

	"github.com/Alexnex31/Norite/backend/voiceproto"
)

func TestAClientsOfferHasTheShapeTheSFUAdmits(t *testing.T) {
	shape, err := readOffer(offerOf(t, newClient(t, 3)), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if shape.sends[voiceproto.KindAudio] != 1 || shape.slots[voiceproto.KindAudio] != 3 {
		t.Fatalf("shape: %+v", shape)
	}
}

// The largest offer a client makes is the slot ceiling's, and op 4 has to carry it in the gateway's 16 KiB
// inbound frame from M33. A Pion client repeats its ICE credentials and fingerprint in every section, so
// the offer grows with the slots; this is what holds the ceiling and the frame together.
func TestTheLargestOfferFitsTheBound(t *testing.T) {
	ceiling := DefaultLimits().Tracks[voiceproto.KindAudio].Slots
	offer := offerOf(t, newClient(t, ceiling))
	if len(offer) > voiceproto.MaxOffer {
		t.Fatalf("an offer at %d slots is %d bytes, over %d", ceiling, len(offer), voiceproto.MaxOffer)
	}
	if _, err := readOffer(offer, DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	t.Logf("an offer at %d slots is %d bytes", ceiling, len(offer))
}

func TestAnOfferOutsideTheShapeIsRefused(t *testing.T) {
	base := offerOf(t, newClient(t, 2))
	lines := strings.Split(base, "\r\n")

	// section returns the lines of the n-th media section (0-based) and where it starts.
	section := func(n int) (start, end int) {
		seen := -1
		start, end = -1, len(lines)
		for i, l := range lines {
			if strings.HasPrefix(l, "m=") {
				seen++
				if seen == n {
					start = i
				} else if seen == n+1 {
					end = i
					break
				}
			}
		}
		return start, end
	}
	join := func(ls []string) string { return strings.Join(ls, "\r\n") }
	replace := func(old, new string) string { return strings.Replace(base, old, new, 1) }

	cases := map[string]string{
		"too large": base + strings.Repeat("a=x-padding:0123456789\r\n", voiceproto.MaxOffer/20),
		"not SDP":   "hello",
		"setup":     replace("a=setup:actpass", "a=setup:active"),
		"no setup": func() string {
			var out []string
			for _, l := range lines {
				if !strings.HasPrefix(l, "a=setup:") {
					out = append(out, l)
				}
			}
			return join(out)
		}(),
		"no channel": func() string {
			// The data channel is the last section; the offer still ends with its CRLF.
			s, _ := section(3)
			return join(append(append([]string{}, lines[:s]...), ""))
		}(),
		"two sends": func() string {
			// The first slot, turned into a second sending section.
			s, e := section(1)
			out := append([]string{}, lines[:s]...)
			for _, l := range lines[s:e] {
				out = append(out, strings.Replace(l, "a=recvonly", "a=sendonly", 1))
			}
			return join(append(out, lines[e:]...))
		}(),
		"sendrecv": replace("a=sendonly", "a=sendrecv"),
		"video": func() string {
			s, e := section(1)
			out := append([]string{}, lines[:s]...)
			for _, l := range lines[s:e] {
				out = append(out, strings.Replace(l, "m=audio", "m=video", 1))
			}
			return join(append(out, lines[e:]...))
		}(),
		"two fingerprints": func() string {
			// A second, different fingerprint on the last section.
			s, _ := section(3)
			out := append([]string{}, lines[:s+1]...)
			out = append(out, "a=fingerprint:sha-256 "+strings.Repeat("AB:", 31)+"AB")
			return join(append(out, lines[s+1:]...))
		}(),
		"weak fingerprint": strings.ReplaceAll(base, "a=fingerprint:sha-256 ", "a=fingerprint:sha-1 "),
		"not bundled": func() string {
			var out []string
			for _, l := range lines {
				if !strings.HasPrefix(l, "a=group:BUNDLE") {
					out = append(out, l)
				}
			}
			return join(out)
		}(),
		"too many candidates": func() string {
			s, _ := section(0)
			out := append([]string{}, lines[:s+1]...)
			for i := range DefaultLimits().MaxCandidates + 1 {
				out = append(out, "a=candidate:"+strings.Repeat("1", i+1)+" 1 udp 2130706431 10.0.0.1 9 typ host")
			}
			return join(append(out, lines[s+1:]...))
		}(),
	}
	want := map[string]string{
		"too large":           "bytes",
		"not SDP":             "not SDP",
		"setup":               "DTLS role",
		"no setup":            "DTLS role",
		"no channel":          "one data channel",
		"two sends":           "sends more audio",
		"sendrecv":            "sendonly or recvonly",
		"video":               "more video slots",
		"two fingerprints":    "exactly one DTLS fingerprint",
		"weak fingerprint":    "SHA-256",
		"not bundled":         "one bundle",
		"too many candidates": "candidates",
	}
	for name, offer := range cases {
		t.Run(name, func(t *testing.T) {
			if offer == base {
				t.Fatal("the case did not change the offer")
			}
			_, err := readOffer(offer, DefaultLimits())
			if !errors.Is(err, ErrBadOffer) {
				t.Fatalf("got %v, want ErrBadOffer", err)
			}
			if !strings.Contains(err.Error(), want[name]) {
				t.Fatalf("refused for another reason: %v", err)
			}
		})
	}
}

func TestMoreSlotsThanTheBoundAreRefused(t *testing.T) {
	lim := DefaultLimits()
	lim.Tracks = map[string]TrackBounds{voiceproto.KindAudio: {Send: 1, Slots: 2}}
	if _, err := readOffer(offerOf(t, newClient(t, 3)), lim); !errors.Is(err, ErrBadOffer) {
		t.Fatalf("got %v, want ErrBadOffer", err)
	}
}

// FuzzReadOffer feeds the validator arbitrary text; it must refuse or accept, never panic.
func FuzzReadOffer(f *testing.F) {
	f.Add("v=0\r\n")
	f.Add("m=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=mid:0\r\n")
	f.Fuzz(func(_ *testing.T, offer string) {
		_, _ = readOffer(offer, DefaultLimits())
	})
}
