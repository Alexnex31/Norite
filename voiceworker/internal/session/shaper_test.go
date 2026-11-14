// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// shaper is a constrained uplink between one caller and the SFU (M31): a token bucket with a drop-tail
// queue, as a home router's is. It is a UDP hop of its own, so what it drops and delays is what the SFU's
// feedback sees, where Options.Lossy drops a packet before it is ever numbered or sent.
//
// The caller reaches it by the SFU's answer, rewritten to name its port (through); the SFU, which is
// ICE-lite, answers whatever address the checks come from, which is the shaper's own socket. M27's
// public_addresses would announce it instead, but refuses a loopback address, rightly.
type shaper struct {
	ln  *net.UDPConn
	sfu *net.UDPAddr
	out chan shaped

	mu       sync.Mutex
	rate     float64 // bits a second, counted on the UDP payload and IPv4's 28 bytes; zero is unlimited
	maxQueue time.Duration
	free     time.Time
	client   *net.UDPAddr
	up       *net.UDPConn
	// What was delivered of the caller's audio since the last take.
	bytes, packets, dropped int
	queued                  time.Duration
}

type shaped struct {
	b     []byte
	at    time.Time
	audio bool
	queue time.Duration
}

func newShaper(t *testing.T, sfu *net.UDPAddr) *shaper {
	t.Helper()
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	s := &shaper{ln: ln, sfu: sfu, out: make(chan shaped, 4096), maxQueue: 300 * time.Millisecond}
	t.Cleanup(func() {
		_ = ln.Close()
		s.mu.Lock()
		if s.up != nil {
			_ = s.up.Close()
		}
		s.mu.Unlock()
	})
	go s.uplink()
	go s.deliver()
	return s
}

var hostCandidate = regexp.MustCompile(`(a=candidate:\S+ \d+ udp \d+ 127\.0\.0\.1 )(\d+)( typ host)`)

// through makes the SFU's answer name the shaper, which then forwards to the port it named. Every
// candidate must be the SFU's one media port on loopback, once for each component, or the call would go
// around the shaper.
func through(t *testing.T, sh **shaper) func(string) string {
	return func(sdp string) string {
		m := hostCandidate.FindAllStringSubmatch(sdp, -1)
		if len(m) == 0 || strings.Count(sdp, "a=candidate:") != len(m) {
			t.Fatalf("the answer has %d loopback candidates of %d:\n%s", len(m), strings.Count(sdp, "a=candidate:"), sdp)
		}
		for _, c := range m {
			if c[2] != m[0][2] {
				t.Fatalf("the answer names two ports:\n%s", sdp)
			}
		}
		port, _ := strconv.Atoi(m[0][2])
		*sh = newShaper(t, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		at := strconv.Itoa((*sh).ln.LocalAddr().(*net.UDPAddr).Port)
		return hostCandidate.ReplaceAllString(sdp, "${1}"+at+"${3}")
	}
}

// isAudio is an RTP packet of the call's Opus payload type, whose header SRTP leaves in the clear.
func isAudio(b []byte) bool { return len(b) > 12 && b[0]>>6 == 2 && b[1]&0x7f == 111 }

// uplink is the only sender on out, so it closes it when the listener closes, and deliver returns.
func (s *shaper) uplink() {
	defer close(s.out)
	buf := make([]byte, 2048)
	for {
		n, from, err := s.ln.ReadFromUDP(buf)
		if err != nil {
			return
		}
		p := shaped{b: append([]byte(nil), buf[:n]...), audio: isAudio(buf[:n])}
		now := time.Now()
		s.mu.Lock()
		if s.up == nil {
			up, err := net.DialUDP("udp", nil, s.sfu)
			if err != nil {
				s.mu.Unlock()
				return
			}
			s.client, s.up = from, up
			go s.downlink(up)
		}
		p.at = now
		if s.rate > 0 {
			start := s.free
			if start.Before(now) {
				start = now
			}
			p.at = start.Add(time.Duration(float64((n+28)*8) / s.rate * float64(time.Second)))
			if p.at.Sub(now) > s.maxQueue {
				if p.audio {
					s.dropped++
				}
				s.mu.Unlock()
				continue
			}
			s.free = p.at
		}
		p.queue = p.at.Sub(now)
		s.mu.Unlock()
		s.out <- p
	}
}

func (s *shaper) deliver() {
	for p := range s.out {
		time.Sleep(time.Until(p.at))
		s.mu.Lock()
		up := s.up
		if p.audio {
			s.bytes += len(p.b)
			s.packets++
			s.queued += p.queue
		}
		s.mu.Unlock()
		if _, err := up.Write(p.b); errors.Is(err, net.ErrClosed) {
			return
		}
	}
}

// downlink carries what the SFU sends back, feedback included, unshaped.
func (s *shaper) downlink(up *net.UDPConn) {
	buf := make([]byte, 2048)
	for {
		n, err := up.Read(buf)
		if err != nil {
			return
		}
		s.mu.Lock()
		to := s.client
		s.mu.Unlock()
		_, _ = s.ln.WriteToUDP(buf[:n], to)
	}
}

func (s *shaper) limit(rate float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rate = rate
}

// delivered is what reached the SFU of the caller's audio since the last call.
type delivered struct {
	kbps             float64
	packets, dropped int
	queue            time.Duration // the average a delivered packet waited
}

func (s *shaper) take(over time.Duration) delivered {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := delivered{kbps: float64(s.bytes*8) / over.Seconds() / 1000, packets: s.packets, dropped: s.dropped}
	if s.packets > 0 {
		d.queue = s.queued / time.Duration(s.packets)
	}
	s.bytes, s.packets, s.dropped, s.queued = 0, 0, 0, 0
	return d
}
