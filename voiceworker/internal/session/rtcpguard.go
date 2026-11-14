// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"time"

	"github.com/pion/interceptor"
)

// rtcpGuard bounds what the SFU's RTCP can make the worker spend, before Pion parses it. It is the first
// interceptor in the chain, so the innermost: Pion's report interceptors, above it, parse every packet in
// full inside the read, and a transport-wide feedback claiming 8,191 received packets in two bytes makes
// that parse allocate a delta for each before it finds the bytes are not there (M31's /code-review). The
// SFU is a stranger's server, from M33 an instance's.
//
// A packet is dropped, and the next one read, when the second's budget is spent, when a feedback in it
// claims more deltas than it carries bytes for, which Pion would refuse anyway, after the allocation, or
// when it does not parse. The parse is kept in the attributes, where Pion's interceptors and readRTCP
// take it rather than parse again, so a packet the SFU sent can never fail a read above the guard: a read
// that fails is the stream's (M31's third /code-review).
type rtcpGuard struct{ interceptor.NoOp }

func (rtcpGuard) NewInterceptor(string) (interceptor.Interceptor, error) { return &rtcpGuard{}, nil }

func (*rtcpGuard) BindRTCPReader(r interceptor.RTCPReader) interceptor.RTCPReader {
	var budget rtcpBudget
	return interceptor.RTCPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes,
		error) {
		for {
			n, attr, err := r.Read(b, a)
			if err != nil {
				return n, attr, err
			}
			if !budget.allow(time.Now()) || !plausible(b[:n]) {
				continue
			}
			if attr == nil {
				attr = interceptor.Attributes{}
			}
			if _, err := attr.GetRTCPPackets(b[:n]); err == nil {
				return n, attr, nil
			}
		}
	})
}

// maxRTCP is how many RTCP packets a stream lets through each second; the rest are read and dropped. The
// SFU sends a transport-wide feedback every 100 ms and a sender report every second or so. pion/srtp
// splits a compound packet and writes each of its packets to its stream alone, so a read is one packet,
// and the worst one lets through is a single feedback, of which the controller reads 256 statuses.
const maxRTCP = 50

// rtcpBudget counts the RTCP packets read in the current second, on the monotonic clock.
type rtcpBudget struct {
	window time.Time
	read   int
}

// allow is whether a packet arriving at now may go on.
func (b *rtcpBudget) allow(now time.Time) bool {
	if now.Sub(b.window) >= time.Second {
		b.window, b.read = now, 0
	}
	b.read++
	return b.read <= maxRTCP
}

// plausible is whether every packet in a compound one is whole, and every transport-wide feedback in it
// carries the bytes its statuses' deltas need. It reads the chunks as RFC draft-holmer-rmcat-transport-
// wide-cc-extensions-01 §3.1 lays them out, without allocating.
func plausible(b []byte) bool {
	for off := 0; off < len(b); {
		if len(b)-off < 4 || b[off]>>6 != 2 {
			return false
		}
		length := (int(b[off+2])<<8 | int(b[off+3]) + 1) * 4
		if length > len(b)-off {
			return false
		}
		const transportFeedback, tcc = 205, 15
		if b[off+1] == transportFeedback && b[off]&0x1f == tcc && !wholeTWCC(b[off:off+length]) {
			return false
		}
		off += length
	}
	return true
}

// wholeTWCC is whether a transport-wide feedback's deltas fit in what follows its chunks.
func wholeTWCC(p []byte) bool {
	const chunks = 20 // the header, two SSRCs, the base, the count, the reference time and the count of feedbacks
	if len(p) < chunks {
		return false
	}
	count := int(p[14])<<8 | int(p[15])
	pos, statuses, deltas := chunks, 0, 0
	// size is the bytes a symbol's delta takes: one for a small delta, two for a large, none otherwise.
	size := func(symbol int) int {
		switch symbol {
		case 1:
			return 1
		case 2:
			return 2
		}
		return 0
	}
	for statuses < count {
		if len(p)-pos < 2 || deltas > len(p) {
			return false
		}
		c := int(p[pos])<<8 | int(p[pos+1])
		pos += 2
		switch {
		case c>>15 == 0: // a run of one symbol
			run := min(c&0x1fff, count-statuses)
			deltas += run * size(c>>13&3)
			statuses += run
		case c>>14&1 == 0: // fourteen one-bit symbols: received, with a small delta, or not
			for j := 13; j >= 0 && statuses < count; j-- {
				deltas += c >> j & 1
				statuses++
			}
		default: // seven two-bit symbols
			for j := 6; j >= 0 && statuses < count; j-- {
				deltas += size(c >> (2 * j) & 3)
				statuses++
			}
		}
	}
	return deltas <= len(p)-pos
}
