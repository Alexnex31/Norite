// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package congestion sets the encoder's bitrate and the loss its FEC is sized for from the SFU's
// transport-wide feedback (M31). The feedback is the sender's own hop, since the SFU terminates RTCP per hop
// (M26): a sender adapts to its uplink and to nothing past the SFU.
//
// It holds the queue on the path short, and reads it directly: each packet's arrival, as the feedback
// reports it, minus its departure, against the smallest such delay seen in the last ten seconds. What is
// left is how long the packet waited behind others. Past high it backs off; under low, while packets flow,
// it climbs. Loss is the second signal, and the one a policer that drops without queuing gives.
//
// It is not pion/interceptor's GCC, which M31's spike measured against a shaped link: its loss half clamps
// its own rate to a 100 kbps floor and so never moves an audio stream, and its delay half reads a trend,
// which a full drop-tail queue does not have. Under a 64 kbps bucket GCC left 190 ms of queue standing,
// and this design 46.
//
// The controller works in the encoder's bitrate, not in a rate the link is estimated to carry, so the
// headers and the tag a packet carries beside its Opus are nowhere in its arithmetic: the queue says whether
// what is sent fits.
//
// Every time it reads is handed to it, so a test drives it on a clock of its own.
package congestion

import (
	"math"
	"sync"
	"time"

	"github.com/pion/rtcp"
)

const (
	// Floor is the lowest bitrate the controller sets: below it Opus speech degrades quickly, and at 50
	// packets a second the 62 bytes of headers, tag and IP that each carries cost more than the codec.
	// 16 kbps of Opus is 41 kbps on the wire, and a link slower than that cannot carry 20 ms frames at
	// all (architecture.md §17).
	Floor = 16000

	// high and low are the queue the controller backs off past and climbs under. Sixty milliseconds is
	// about what a person notices added to a call's delay; twenty leaves the jitter of a loaded Wi-Fi link
	// room under it.
	high = 60 * time.Millisecond
	low  = 20 * time.Millisecond
	// backoff is what a decrease keeps, and settle how long after one the next may come: about three
	// feedback intervals, so the queue the last one drained has been seen to drain.
	backoff = 0.85
	settle  = 300 * time.Millisecond
	// climb is how much the bitrate may rise in a second of speech, so 16 kbps reaches 64 in about ten.
	climb = 1.15
	// flowing is how many of its packets a feedback must acknowledge for an increase. In DTX the encoder
	// sends a comfort-noise update every 400 ms or so, and what such a link carries says nothing about what
	// speech would: the spike measured GCC raising its target from 41 to 70 kbps over four seconds of
	// silence, into a link that carried 40, so each sentence started above it.
	flowing = 3

	// congested and policed are the losses past which the bitrate comes down: with a queue standing, the
	// loss is the queue overflowing; past policed with none, a policer dropping what exceeds its rate.
	// Below them loss is the link's own, which a lower bitrate does nothing for and the FEC is sized for.
	congested = 0.10
	policed   = 0.25
	// lossWindow is the time constant of the loss the controller judges by.
	lossWindow = time.Second
	// MaxLossPercent is the most loss the FEC is sized for. Past it, Opus spends so much of the bitrate on
	// redundancy that what is protected is worth little.
	MaxLossPercent = 25

	// baseWindow is how long the smallest delay is remembered. Kept in two halves, so forgetting the old
	// half never leaves the controller with no base.
	baseWindow = 10 * time.Second
	// ring is how many departures are remembered, 20 s of packets: a feedback naming anything older is
	// naming nothing this controller sent.
	ring = 1024
	// MaxStatuses is the most packets of one feedback the controller reads. The SFU sends one every 100 ms,
	// covering five packets; a feedback is a stranger's (from M33 an instance's), and its count is 16 bits.
	MaxStatuses = 256
	// remembered is how long a departure stays: the ring's span at 50 packets a second.
	remembered = ring * 20 * time.Millisecond
)

// Controller is one call's. Sent is called by the sender, Feedback by the RTCP reader, and the rest by
// either.
type Controller struct {
	mu      sync.Mutex
	ceiling float64
	rate    float64
	loss    float64 // smoothed, 0 to 1

	sent [ring]departure
	base time.Time // what departures are measured from

	// The smallest one-way delay in each half of baseWindow, the current half started at half.
	minNow, minPrev time.Duration
	half            time.Time
	haveBase        bool

	lastFeedback, lastDecrease, lastLoss time.Time

	queue time.Duration // what the last feedback measured, for the log

	visited int // packets read out of feedback, for a test of the bound
}

type departure struct {
	seq  uint16
	at   time.Duration // since base
	used bool
}

// New starts at ceiling, which is what a call is configured for: a call starts as it would without the
// controller, and comes down only on the evidence of its own link.
func New(ceiling int, now time.Time) *Controller {
	return &Controller{ceiling: float64(max(ceiling, Floor)), rate: float64(max(ceiling, Floor)), base: now}
}

// Sent records that the packet carrying transport-wide sequence number seq left at now.
func (c *Controller) Sent(seq uint16, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent[seq%ring] = departure{seq: seq, at: now.Sub(c.base), used: true}
}

// Feedback reads one transport-wide feedback, arriving at now.
func (c *Controller) Feedback(fb *rtcp.TransportLayerCC, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	arrival := time.Duration(fb.ReferenceTime) * 64 * time.Millisecond
	var lost, total, received int
	queue := time.Duration(math.MaxInt64)
	deltas := fb.RecvDeltas
	n := min(int(fb.PacketStatusCount), MaxStatuses)
	i := 0
	visit := func(symbol uint16) bool {
		if i >= n {
			return false
		}
		seq := fb.BaseSequenceNumber + uint16(i) //nolint:gosec // i is under MaxStatuses
		i++
		c.visited++
		d := c.sent[seq%ring]
		mine := d.used && d.seq == seq && now.Sub(c.base)-d.at < remembered
		if symbol == rtcp.TypeTCCPacketNotReceived {
			if mine {
				lost++
				total++
			}
			return true
		}
		// Each received packet has a delta, in order, whether or not it is one this controller sent.
		if len(deltas) == 0 {
			return false
		}
		arrival += time.Duration(deltas[0].Delta) * time.Microsecond
		deltas = deltas[1:]
		if !mine {
			return true
		}
		total++
		received++
		queue = min(queue, c.delay(arrival-d.at, now))
		return true
	}
	for _, chunk := range fb.PacketChunks {
		switch ch := chunk.(type) {
		case *rtcp.RunLengthChunk:
			for range ch.RunLength {
				if !visit(ch.PacketStatusSymbol) {
					break
				}
			}
		case *rtcp.StatusVectorChunk:
			// A one-bit symbol is 0 or 1, not received or received with a small delta, which are the
			// two-bit symbols' own values.
			for _, s := range ch.SymbolList {
				if !visit(s) {
					break
				}
			}
		}
		if i >= n {
			break
		}
	}
	if total == 0 {
		return
	}
	c.adjust(lost, total, received, queue, now)
}

// delay is a one-way delay's excess over the smallest seen lately: the queue it waited in. The two clocks
// differ by a constant the subtraction removes, and a delay below every one seen is the new base.
func (c *Controller) delay(owd time.Duration, now time.Time) time.Duration {
	if !c.haveBase {
		c.minNow, c.minPrev, c.half, c.haveBase = owd, owd, now, true
	}
	if now.Sub(c.half) >= baseWindow/2 {
		c.minPrev, c.minNow, c.half = c.minNow, owd, now
	}
	c.minNow = min(c.minNow, owd)
	return owd - min(c.minNow, c.minPrev)
}

func (c *Controller) adjust(lost, total, received int, queue time.Duration, now time.Time) {
	dt := min(now.Sub(c.lastFeedback), time.Second)
	if c.lastFeedback.IsZero() {
		dt = 0
	}
	c.lastFeedback = now
	sample := float64(lost) / float64(total)
	if c.lastLoss.IsZero() {
		c.loss = sample
	} else {
		c.loss = sample + math.Exp(-float64(now.Sub(c.lastLoss))/float64(lossWindow))*(c.loss-sample)
	}
	c.lastLoss = now
	if received > 0 {
		c.queue = queue
	}
	canDecrease := now.Sub(c.lastDecrease) >= settle
	standing := received > 0 && queue > low
	switch {
	case received > 0 && queue > high && canDecrease:
		c.decrease(backoff, now)
	case canDecrease && (c.loss > policed || (c.loss > congested && standing)):
		c.decrease(1-c.loss/2, now)
	case received >= flowing && queue < low && c.loss <= congested:
		c.rate = min(c.ceiling, c.rate*math.Pow(climb, dt.Seconds()))
	}
}

func (c *Controller) decrease(by float64, now time.Time) {
	c.rate = max(Floor, c.rate*by)
	c.lastDecrease = now
}

// Bitrate is what the encoder should be set to, in bits per second.
func (c *Controller) Bitrate() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return int(c.rate)
}

// LossPercent is the loss the encoder's FEC should be sized for: what the feedback measured, up to
// MaxLossPercent.
func (c *Controller) LossPercent() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return min(MaxLossPercent, int(math.Round(c.loss*100)))
}

// Queue is the queue the last feedback measured.
func (c *Controller) Queue() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.queue
}
