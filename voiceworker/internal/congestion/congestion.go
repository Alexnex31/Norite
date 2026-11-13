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
	// climb is how much the bitrate may rise in a second of speech, so 16 kbps reaches 64 in about ten. A
	// second of speech is fifty packets received, each Frame's worth credited once: a climb measured in time
	// since the last feedback credited the silence before a sentence to it (M31's third /code-review: twenty
	// 100 ms bursts of speech climbed as four seconds would). One feedback is credited with settle at most:
	// one reporting 256 packets doubled the bitrate (the fourth).
	climb = 1.15
	// Frame is the encoder's frame, which the climb credits per packet. TestTheFrameIsOneSizeEverywhere
	// holds it to the codec's.
	Frame = 20 * time.Millisecond
	// flowing is how many of its packets a feedback must acknowledge for an increase. In DTX the encoder
	// sends a comfort-noise update every 400 ms or so, and what such a link carries says nothing about what
	// speech would: the spike measured GCC raising its target from 41 to 70 kbps over four seconds of
	// silence, into a link that carried 40, so each sentence started above it.
	flowing = 3

	// congested and policed are the losses past which the bitrate comes down: with a queue standing, the
	// loss is the queue overflowing; past policed with none, a policer dropping what exceeds its rate.
	// Below them loss is the link's own, which a lower bitrate does nothing for and the FEC is sized for.
	// congested is also the most loss the current talk-spurt may show for the bitrate to climb.
	congested = 0.10
	policed   = 0.25
	// overflowed is how far past the link's own loss the loss may rise after a climb before it is the
	// bitrate's doing (baselined).
	overflowed = 0.02
	// baselined is how many packets after a decrease measure the link's own loss, at a bitrate known to
	// fit: loss confidently past it by overflowed, among the packets sent after them, is the bitrate's
	// doing, and is answered by backoff. A link that drops what queues past 35 ms holds less than two
	// packets at 64 kbit/s, so no queue shows, and its 17% read as random: the controller climbed back
	// into it after each cut and stayed (M31's fourth /code-review).
	baselined = 50
	// lossWindow is how many packets the FEC's loss is counted over: the newest of known outcome, three
	// seconds of speech, however long the silences between them. They are counted off the departures the
	// controller keeps, so a packet reported lost and then received is taken back exactly, and nothing
	// fades. Counted over a time, a window after a silence held a few comfort-noise updates, and one of
	// them lost sized the FEC for 12%.
	lossWindow = 150
	// judged is how many packets of known outcome a count must hold before it says anything: the FEC's
	// window, and the packets since the last decrease that loss brings the bitrate down on. One
	// comfort-noise update lost is not half the link lost: judged on fewer, 3% random loss through a
	// minute of silence took the bitrate to the floor (M31's /code-review).
	judged = 10
	// confidence is how sure the controller must be that the loss since the last decrease is past its line
	// before it acts on it: the lower bound of a Wilson interval, at about one chance
	// in a thousand of acting on random loss under the line. A fixed sample cannot do both jobs: ten
	// packets read 15% random loss as a policer's 25% in half a test's seeds, and a hundred took seven
	// seconds to settle on a link dropping what queued past 35 ms (M31's third and fourth /code-review).
	confidence = 3.0
	// spurt is how many packets of the current talk-spurt must be known before the bitrate climbs, and
	// the most of them it climbs on: the loss among them, at most congested. Only the spurt's own: a
	// window reaching across a silence let clean speech from before it hide loss that started after it,
	// and the bitrate climbed on a link losing 40% (M31's fourth /code-review).
	spurt = 25
	// pause is a gap between two departures that ends a talk-spurt: DTX sends nothing for 400 ms.
	pause = 3 * Frame
	// MinLossPercent and MaxLossPercent bound the loss the FEC is sized for. libopus turns in-band FEC off
	// altogether at 0% (decide_fec), so the floor is what M29 sized it for before the controller, and keeps
	// the first loss of a burst recoverable. Past the ceiling, Opus spends so much of the bitrate on
	// redundancy that what is protected is worth little.
	MinLossPercent = 5
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
	remembered = ring * Frame
)

// Controller is one call's. Sent is called by the sender, Feedback by the RTCP reader, and the rest by
// either.
type Controller struct {
	mu      sync.Mutex
	ceiling float64
	rate    float64
	// fec is the loss last judged on a window of at least judged packets, which the FEC is sized for, and
	// measured whether there has been one.
	fec      float64
	measured bool

	sent [ring]departure
	base time.Time // what departures are measured from
	// last is the sequence number of the newest packet sent, which the window is counted back from.
	last uint16
	// cut is the departure of the newest packet sent when the bitrate last came down. Loss is judged on the
	// packets sent after it, so the evidence a decrease acted on is never acted on again: the window
	// remembers an outage for a second after it ends, and judging on it let random loss in that second cut
	// again (M31's second /code-review), and the queue's overflow, which its backoffs answered, cut once
	// more once the queue fell (the third).
	cut time.Duration
	// baseline is the loss of the first baselined packets after the last decrease, the newest of which
	// left at baselineAt; haveBaseline is whether they are known yet. climbed is whether the bitrate has
	// risen since the last decrease: loss can be the bitrate's doing only then, and an outage at the
	// ceiling was otherwise cut twice, once as risen loss and once as a policer's.
	baseline              float64
	baselineAt            time.Duration
	haveBaseline, climbed bool

	// The smallest one-way delay in each half of baseWindow, the current half started at half.
	minNow, minPrev time.Duration
	half            time.Time
	haveBase        bool

	lastDecrease time.Time

	visited int // packets read out of feedback, for a test of the bound
}

type departure struct {
	seq  uint16
	at   time.Duration // since base
	used bool
	// reported is set once a feedback has counted it, lost or received. pion's recorder reports packets
	// again after a reordering, and a feedback can arrive twice; neither is counted twice. lost is set
	// when that count was a loss, which a later report of it received takes back: a packet reordered across
	// a feedback was late, not lost.
	reported, lost bool
}

// New starts at ceiling, which is what a call is configured for: a call starts as it would without the
// controller, and comes down only on the evidence of its own link.
func New(ceiling int, now time.Time) *Controller {
	return &Controller{ceiling: float64(max(ceiling, Floor)), rate: float64(max(ceiling, Floor)), base: now, cut: -1}
}

// Sent records that the packet carrying transport-wide sequence number seq left at now.
func (c *Controller) Sent(seq uint16, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent[seq%ring] = departure{seq: seq, at: now.Sub(c.base), used: true}
	c.last = seq
}

// Feedback reads one transport-wide feedback, arriving at now.
func (c *Controller) Feedback(fb *rtcp.TransportLayerCC, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	arrival := time.Duration(fb.ReferenceTime) * 64 * time.Millisecond
	// timed counts the received packets with a delta, which alone measure the queue; late, those counted
	// lost before that were received after all.
	var late, total, received, timed int
	queue := time.Duration(math.MaxInt64)
	deltas := fb.RecvDeltas
	n := min(int(fb.PacketStatusCount), MaxStatuses)
	i := 0
	// visit reads the next packet's status, and reports whether the walk goes on.
	visit := func(symbol uint16) bool {
		if i >= n {
			return false
		}
		seq := fb.BaseSequenceNumber + uint16(i) //nolint:gosec // i is under MaxStatuses
		i++
		c.visited++
		// A received packet has a delta, in order, whether or not it is one this controller sent, except
		// one received without a delta (symbol 3), which pion's parser gives none. A feedback whose deltas
		// run out is malformed, and nothing after that point in it can be placed.
		hasDelta := symbol == rtcp.TypeTCCPacketReceivedSmallDelta || symbol == rtcp.TypeTCCPacketReceivedLargeDelta
		if hasDelta {
			if len(deltas) == 0 {
				return false
			}
			arrival += time.Duration(deltas[0].Delta) * time.Microsecond
			deltas = deltas[1:]
		}
		d := &c.sent[seq%ring]
		if !d.used || d.seq != seq || now.Sub(c.base)-d.at >= remembered {
			return true
		}
		if d.reported {
			if d.lost && symbol != rtcp.TypeTCCPacketNotReceived {
				d.lost = false
				late++
			}
			return true
		}
		d.reported = true
		total++
		if symbol == rtcp.TypeTCCPacketNotReceived {
			d.lost = true
			return true
		}
		received++
		if hasDelta {
			timed++
			queue = min(queue, c.delay(arrival-d.at, now))
		}
		return true
	}
walk:
	for _, chunk := range fb.PacketChunks {
		switch ch := chunk.(type) {
		case *rtcp.RunLengthChunk:
			for range ch.RunLength {
				if !visit(ch.PacketStatusSymbol) {
					break walk
				}
			}
		case *rtcp.StatusVectorChunk:
			// A one-bit symbol is 0 or 1, not received or received with a small delta, which are the
			// two-bit symbols' own values.
			for _, s := range ch.SymbolList {
				if !visit(s) {
					break walk
				}
			}
		}
	}
	if total == 0 && late == 0 {
		return
	}
	c.adjust(feedback{received: received, timed: timed, queue: queue}, now)
}

// feedback is what one transport-wide feedback said about the queue on this controller's packets.
type feedback struct {
	received, timed int
	queue           time.Duration // the least a timed packet waited; meaningless at timed 0
}

// count is packets of known outcome, and how many of them were lost.
type count struct{ lost, known int }

func (n *count) add(d *departure) {
	n.known++
	if d.lost {
		n.lost++
	}
}

func (n count) ratio() float64 { return float64(n.lost) / float64(n.known) }

// tally is what the newest lossWindow packets of known outcome say about the loss: all of them, which
// the FEC is sized from; those sent since the last decrease; those sent after the baseline; and the
// current talk-spurt's newest.
type tally struct{ window, since, risen, spurt count }

// counts walks the departures back from the newest sent, as far as the ring reaches, which in DTX is
// minutes: about 155 of them in speech, ten times a second.
func (c *Controller) counts() tally {
	var t tally
	inSpurt := true
	next := c.sent[c.last%ring].at
	for k := 0; k < ring && t.window.known < lossWindow; k++ {
		seq := c.last - uint16(k) //nolint:gosec // k is under ring
		d := &c.sent[seq%ring]
		if !d.used || d.seq != seq {
			break
		}
		inSpurt = inSpurt && next-d.at <= pause
		next = d.at
		if !d.reported {
			continue
		}
		t.window.add(d)
		if d.at > c.cut {
			t.since.add(d)
		}
		if c.haveBaseline && d.at > c.baselineAt {
			t.risen.add(d)
		}
		if inSpurt && t.spurt.known < spurt {
			t.spurt.add(d)
		}
	}
	return t
}

// past is whether the loss in n is past line, by the lower bound of its Wilson interval at confidence.
func (n count) past(line float64) bool {
	if n.known < judged {
		return false
	}
	k, p, z := float64(n.known), n.ratio(), confidence
	lower := (p + z*z/(2*k) - z*math.Sqrt(p*(1-p)/k+z*z/(4*k*k))) / (1 + z*z/k)
	return lower > line
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

func (c *Controller) adjust(f feedback, now time.Time) {
	t := c.counts()
	if t.window.known >= judged {
		c.fec, c.measured = t.window.ratio(), true
	}
	if !c.haveBaseline && t.since.known >= baselined {
		c.baseline, c.baselineAt, c.haveBaseline = t.since.ratio(), c.sent[c.last%ring].at, true
	}
	canDecrease := now.Sub(c.lastDecrease) >= settle
	// The queue is read off packets that carried a delta; a feedback with none says nothing about it.
	queued := f.timed > 0 && f.queue > high
	standing := f.timed > 0 && f.queue > low
	drained := f.timed > 0 && f.queue < low
	switch {
	case queued && canDecrease:
		c.decrease(backoff, now)
	case canDecrease && standing && t.since.past(congested):
		c.decrease(1-t.since.ratio()/2, now)
	case canDecrease && f.received >= flowing && c.climbed && c.haveBaseline && t.risen.past(c.baseline+overflowed):
		c.decrease(backoff, now)
	case canDecrease && t.since.past(policed):
		// A policer passes what fits its rate and drops the rest, so what it passed is where to go: halving
		// the loss instead left a link dropping what queued past 35 ms at 17% loss, which reads as random.
		// An outage passes nothing, and is not a rate: no decrease takes more than half.
		c.decrease(max(0.5, 1-t.since.ratio()), now)
	case f.received >= flowing && drained && t.spurt.known >= spurt && t.spurt.ratio() <= c.climbable():
		speech := min(time.Duration(f.received)*Frame, settle)
		rate := min(c.ceiling, c.rate*math.Pow(climb, speech.Seconds()))
		c.climbed = c.climbed || rate > c.rate
		c.rate = rate
	}
}

// climbable is the most loss the current talk-spurt may show for the bitrate to climb: congested, and
// once the link's own loss is known, no more than overflowed past it, so the climb stops where its own
// loss starts.
func (c *Controller) climbable() float64 {
	if c.haveBaseline {
		return min(congested, c.baseline+overflowed)
	}
	return congested
}

// decrease brings the bitrate down by a factor, and starts the loss judged next after the newest packet
// sent: those already in flight were sent at the rate it acted on.
func (c *Controller) decrease(by float64, now time.Time) {
	c.rate = max(Floor, c.rate*by)
	c.lastDecrease = now
	c.cut = c.sent[c.last%ring].at
	c.haveBaseline, c.climbed = false, false
}

// Target is what the encoder should be set to, under one lock: the bitrate in bits per second, and the
// loss its FEC should be sized for, what the feedback measured on a full window between MinLossPercent and
// MaxLossPercent. The sender asks every frame.
func (c *Controller) Target() (bitrate, lossPercent int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return int(c.rate), max(MinLossPercent, min(MaxLossPercent, int(math.Round(c.fec*100))))
}

// Loss is the share of packets lost, as last judged on a window of at least judged packets, for the
// close's log line, and whether there has been one: a call whose feedback never came, or that sent fewer
// packets than that, measured nothing.
func (c *Controller) Loss() (loss float64, measured bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fec, c.measured
}
