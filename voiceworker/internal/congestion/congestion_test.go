// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package congestion

import (
	"math/rand/v2"
	"sort"
	"testing"
	"time"

	"github.com/pion/interceptor/pkg/twcc"
	"github.com/pion/rtcp"
)

// overhead is what a packet carries beside its Opus: the RTP header with two extensions, the SRTP tag, and
// IPv4 and UDP. A link's rate is counted on all of it.
const overhead = 24 + 10 + 28

// link is a sender's uplink to the SFU, a token bucket with a drop-tail queue and random loss, and the
// SFU's feedback, built by pion's own recorder as the SFU's interceptor builds it. Its clock is the test's.
type link struct {
	t        *testing.T
	c        *Controller
	now      time.Time
	rate     float64 // bits a second on the wire; zero is unlimited
	maxQueue time.Duration
	loss     float64
	rng      *rand.Rand

	free     time.Time // when the bucket has sent everything queued
	seq      uint16
	inFlight []arrival
	rec      *twcc.Recorder
	frame    int

	// What was measured, since the last take: the worst queue a packet waited in, and how many were sent
	// and dropped.
	worstQueue    time.Duration
	sent, dropped int
	bitrates      []int
}

type arrival struct {
	seq uint16
	at  time.Time
}

// sfuClock is how far the SFU's clock is from the sender's: arbitrary, since only differences matter.
const sfuClock = 3*time.Hour + 17*time.Millisecond

func newLink(t *testing.T, ceiling int) *link {
	start := time.Unix(1_800_000_000, 0)
	return &link{t: t, c: New(ceiling, start), now: start, maxQueue: 300 * time.Millisecond,
		rng: rand.New(rand.NewPCG(31, 1)), rec: twcc.NewRecorder(1)} //nolint:gosec // a test's link
}

// run plays d of a sender, talking or in DTX, over the link: a frame every 20 ms, a feedback every 100.
func (l *link) run(d time.Duration, talking bool) {
	for end := l.now.Add(d); l.now.Before(end); l.now = l.now.Add(20 * time.Millisecond) {
		l.frame++
		bitrate := l.c.Bitrate()
		l.bitrates = append(l.bitrates, bitrate)
		size := bitrate / 8 / 50
		if !talking {
			// A comfort-noise update every 400 ms, as Opus's DTX sends.
			if l.frame%20 != 0 {
				l.deliver()
				continue
			}
			size = 3
		}
		l.send(size + overhead)
		l.deliver()
		if l.frame%5 == 0 {
			l.feedback()
		}
	}
}

func (l *link) send(wire int) {
	l.seq++
	l.c.Sent(l.seq, l.now)
	l.sent++
	if l.loss > 0 && l.rng.Float64() < l.loss {
		return
	}
	at := l.now
	if l.rate > 0 {
		start := l.free
		if start.Before(l.now) {
			start = l.now
		}
		at = start.Add(time.Duration(float64(wire*8) / l.rate * float64(time.Second)))
		if at.Sub(l.now) > l.maxQueue {
			l.dropped++
			return
		}
		l.free = at
	}
	l.worstQueue = max(l.worstQueue, at.Sub(l.now))
	// Five milliseconds of propagation, which the base delay absorbs.
	l.inFlight = append(l.inFlight, arrival{l.seq, at.Add(5 * time.Millisecond)})
}

// deliver hands the SFU's recorder every packet that has arrived by now.
func (l *link) deliver() {
	sort.Slice(l.inFlight, func(i, j int) bool { return l.inFlight[i].at.Before(l.inFlight[j].at) })
	for len(l.inFlight) > 0 && !l.inFlight[0].at.After(l.now) {
		a := l.inFlight[0]
		l.inFlight = l.inFlight[1:]
		l.rec.Record(7, a.seq, a.at.Add(sfuClock).UnixMicro())
	}
}

// feedback is what the SFU sends, through its wire format and back, ten milliseconds later.
func (l *link) feedback() {
	for _, p := range l.rec.BuildFeedbackPacket() {
		b, err := p.Marshal()
		if err != nil {
			l.t.Fatal(err)
		}
		got, err := rtcp.Unmarshal(b)
		if err != nil {
			l.t.Fatal(err)
		}
		for _, q := range got {
			if fb, ok := q.(*rtcp.TransportLayerCC); ok {
				l.c.Feedback(fb, l.now.Add(10*time.Millisecond))
			}
		}
	}
}

type measured struct {
	worstQueue        time.Duration
	sent, dropped     int
	minRate, lastRate int
}

func (l *link) take() measured {
	m := measured{worstQueue: l.worstQueue, sent: l.sent, dropped: l.dropped, minRate: 1 << 30,
		lastRate: l.c.Bitrate()}
	for _, b := range l.bitrates {
		m.minRate = min(m.minRate, b)
	}
	l.worstQueue, l.sent, l.dropped, l.bitrates = 0, 0, 0, nil
	return m
}

// A link slower than the ceiling: the bitrate comes down until what is sent fits, the queue drains and
// stays short, and once the link is lifted the bitrate climbs back to the ceiling.
func TestTheBitrateFollowsTheLink(t *testing.T) {
	l := newLink(t, 64000)
	l.run(10*time.Second, true)
	if m := l.take(); m.lastRate != 64000 || m.minRate != 64000 {
		t.Fatalf("on an open link the bitrate went to %d, ending at %d", m.minRate, m.lastRate)
	}
	// 64 kbps on the wire is 160 bytes a packet at 50 a second, 98 of them Opus: 39.2 kbps.
	l.rate = 64000
	fits := (64000/8/50 - overhead) * 8 * 50
	l.run(5*time.Second, true)
	converging := l.take()
	l.run(20*time.Second, true)
	held := l.take()
	t.Logf("at 64 kbps: converging, worst queue %v; then worst queue %v, %d of %d dropped, bitrate %d to %d",
		converging.worstQueue, held.worstQueue, held.dropped, held.sent, held.minRate, held.lastRate)
	if held.worstQueue > 120*time.Millisecond || held.dropped > 0 {
		t.Errorf("held at 64 kbps, a packet waited %v and %d were dropped", held.worstQueue, held.dropped)
	}
	if held.lastRate > fits || held.lastRate < fits/2 {
		t.Errorf("held at 64 kbps, the bitrate is %d, want what fits, %d, or not under half of it", held.lastRate,
			fits)
	}
	l.rate = 0
	l.run(15*time.Second, true)
	if m := l.take(); m.lastRate != 64000 {
		t.Errorf("15 s after the link was lifted the bitrate is %d, not back at 64 kbps", m.lastRate)
	}
}

// Without the controller, the same link keeps its queue full: the property the test above holds is the
// controller's, not the link's.
func TestAFixedBitrateFillsTheQueue(t *testing.T) {
	l := newLink(t, 64000)
	l.rate = 64000
	l.c.rate, l.c.ceiling = 64000, 64000
	for range 25 * 50 {
		l.c.mu.Lock()
		l.c.rate = 64000
		l.c.mu.Unlock()
		l.run(20*time.Millisecond, true)
	}
	if m := l.take(); m.worstQueue < 250*time.Millisecond || m.dropped == 0 {
		t.Errorf("at a fixed 64 kbps the worst queue was %v and %d were dropped", m.worstQueue, m.dropped)
	}
}

// In DTX the encoder sends a comfort-noise update every 400 ms, and what a link carries that says nothing
// about what speech needs: the bitrate does not climb through a silence, so the next sentence starts
// where the last one ended.
func TestSilenceDoesNotRaiseTheBitrate(t *testing.T) {
	l := newLink(t, 64000)
	l.rate = 64000
	l.run(25*time.Second, true)
	before := l.c.Bitrate()
	l.run(10*time.Second, false)
	if after := l.c.Bitrate(); after > before {
		t.Errorf("ten seconds of silence raised the bitrate from %d to %d", before, after)
	}
	l.take()
	l.run(time.Second, true)
	if m := l.take(); m.worstQueue > 150*time.Millisecond {
		t.Errorf("the sentence after the silence queued %v", m.worstQueue)
	}
}

// Loss with no queue is the link's own, which a lower bitrate does nothing for: the bitrate stays, and the
// FEC is sized for what was lost.
func TestRandomLossSizesTheFECAndKeepsTheBitrate(t *testing.T) {
	l := newLink(t, 64000)
	l.loss = 0.15
	l.run(20*time.Second, true)
	m := l.take()
	if m.minRate < 64000 {
		t.Errorf("15%% random loss took the bitrate down to %d", m.minRate)
	}
	if p := l.c.LossPercent(); p < 10 || p > 20 {
		t.Errorf("at 15%% loss the FEC is sized for %d%%", p)
	}
	l.loss = 0
	l.run(10*time.Second, true)
	if p := l.c.LossPercent(); p != MinLossPercent {
		t.Errorf("ten seconds after the loss stopped the FEC is sized for %d%%, want the floor %d%%", p,
			MinLossPercent)
	}
}

// libopus turns in-band FEC off at 0% loss, so the FEC is never sized for less than MinLossPercent: from
// the call's first frame, on a link that has lost nothing, the first loss of a burst stays recoverable.
func TestTheFECIsNeverSizedForNoLoss(t *testing.T) {
	l := newLink(t, 64000)
	if p := l.c.LossPercent(); p != MinLossPercent {
		t.Errorf("a call starts with its FEC sized for %d%%", p)
	}
	l.run(5*time.Second, true)
	if p := l.c.LossPercent(); p != MinLossPercent {
		t.Errorf("on a clean link the FEC is sized for %d%%", p)
	}
}

// In DTX a feedback covers a comfort-noise packet or none, and one of them lost is not the link lost: a
// minute of silence at 3% random loss leaves the bitrate where the speech left it.
func TestLossInSilenceIsNotJudged(t *testing.T) {
	l := newLink(t, 64000)
	l.run(5*time.Second, true)
	l.loss = 0.03
	// Nor is the FEC sized from it: speech resuming on a link losing 3% is not told to expect 25%.
	worst := 0
	for range 600 {
		l.run(100*time.Millisecond, false)
		worst = max(worst, l.c.LossPercent())
	}
	if m := l.take(); m.minRate < 64000 {
		t.Errorf("a minute of silence at 3%% loss took the bitrate down to %d", m.minRate)
	}
	if worst > 10 {
		t.Errorf("in a minute of silence at 3%% loss the FEC was sized for as much as %d%%", worst)
	}
}

// An outage is one loss event: the bitrate comes down once for it, not once every settle for as long as
// the window remembers it.
func TestAnOutageIsOneDecrease(t *testing.T) {
	l := newLink(t, 64000)
	l.run(5*time.Second, true)
	l.loss = 1
	l.run(time.Second, true)
	l.loss = 0
	l.run(3*time.Second, true)
	m := l.take()
	if m.minRate < 64000/2 {
		t.Errorf("a second's outage took the bitrate down to %d", m.minRate)
	}
	l.run(5*time.Second, true)
	if r := l.c.Bitrate(); r != 64000 {
		t.Errorf("eight seconds after the outage the bitrate is %d", r)
	}
	// With random loss beside the outage, each loss in the second after it is judged on what came since
	// the outage's decrease, not on the window's memory of the outage.
	for _, background := range []float64{0.03, 0.05} {
		l := newLink(t, 64000)
		l.loss = background
		l.run(5*time.Second, true)
		l.take()
		l.loss = 1
		l.run(time.Second, true)
		l.loss = background
		l.run(3*time.Second, true)
		if m := l.take(); m.minRate < 64000/2 {
			t.Errorf("a second's outage on a link losing %.0f%% took the bitrate down to %d", 100*background,
				m.minRate)
		}
	}
}

// A packet reported lost and then received, which pion's recorder does for one reordered across a
// feedback, was late: it is taken back out of the loss.
func TestAReorderedPacketIsNotLost(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	c := New(64000, start)
	rec := twcc.NewRecorder(1)
	feed := func(at time.Time) {
		for _, p := range rec.BuildFeedbackPacket() {
			b, err := p.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			got, err := rtcp.Unmarshal(b)
			if err != nil {
				t.Fatal(err)
			}
			for _, q := range got {
				if fb, ok := q.(*rtcp.TransportLayerCC); ok {
					c.Feedback(fb, at)
				}
			}
		}
	}
	for seq := range uint16(10) {
		c.Sent(seq, start.Add(time.Duration(seq)*20*time.Millisecond))
	}
	for _, seq := range []uint16{0, 1, 3, 4} {
		rec.Record(7, seq, start.Add(time.Duration(seq)*20*time.Millisecond+5*time.Millisecond).UnixMicro())
	}
	feed(start.Add(100 * time.Millisecond))
	if c.lost < 0.9 {
		t.Fatalf("packet 2, not yet arrived, counted %v lost", c.lost)
	}
	for _, seq := range []uint16{2, 5, 6, 7, 8, 9} {
		rec.Record(7, seq, start.Add(time.Duration(seq)*20*time.Millisecond+30*time.Millisecond).UnixMicro())
	}
	feed(start.Add(200 * time.Millisecond))
	if c.lost > 0.01 {
		t.Errorf("packet 2, received late, is still counted %v lost", c.lost)
	}
}

// A feedback whose received packets carry no delta (symbol 3) says nothing of the queue: twenty of them,
// nothing lost, leave the bitrate where it was.
func TestAFeedbackWithNoDeltasSaysNothingOfTheQueue(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	c := New(64000, start)
	now := start
	for k := range 20 {
		base := uint16(k * 5) //nolint:gosec // a test's count
		for i := range uint16(5) {
			c.Sent(base+i, now)
		}
		now = now.Add(100 * time.Millisecond)
		c.Feedback(&rtcp.TransportLayerCC{BaseSequenceNumber: base, PacketStatusCount: 5,
			PacketChunks: []rtcp.PacketStatusChunk{&rtcp.RunLengthChunk{
				PacketStatusSymbol: rtcp.TypeTCCPacketReceivedWithoutDelta, RunLength: 5}}}, now)
	}
	if r := c.Bitrate(); r != 64000 {
		t.Errorf("twenty feedbacks with no delta and nothing lost took the bitrate to %d", r)
	}
}

// Speech resuming after a silence, on a link losing 40%: the window is too small to judge the loss on,
// and too small to climb on either. The bitrate holds until there is enough to judge.
func TestLossTooSoonToJudgeDoesNotRaiseTheBitrate(t *testing.T) {
	l := newLink(t, 64000)
	l.c.rate = 40000
	l.run(5*time.Second, false)
	l.take()
	l.loss = 0.4
	l.run(300*time.Millisecond, true)
	for _, b := range l.bitrates {
		if b > 40000 {
			t.Fatalf("speech on a link losing 40%% raised the bitrate to %d", b)
		}
	}
}

// A policer drops what exceeds its rate without queuing it, so loss is the only sign: past policed the
// bitrate comes down. The FEC's loss stops at MaxLossPercent.
func TestHeavyLossBringsTheBitrateDown(t *testing.T) {
	l := newLink(t, 64000)
	l.loss = 0.45
	l.run(10*time.Second, true)
	if m := l.take(); m.lastRate > 32000 {
		t.Errorf("at 45%% loss the bitrate is %d", m.lastRate)
	}
	if p := l.c.LossPercent(); p != MaxLossPercent {
		t.Errorf("at 45%% loss the FEC is sized for %d%%, want %d", p, MaxLossPercent)
	}
}

// A link that carries less than the floor: the controller stops at the floor, and never below.
func TestTheFloorHolds(t *testing.T) {
	l := newLink(t, 64000)
	l.rate = 20000
	l.run(20*time.Second, true)
	if m := l.take(); m.minRate != Floor {
		t.Errorf("on a 20 kbps link the bitrate went to %d, want the floor %d", m.minRate, Floor)
	}
	if b := New(8000, l.now).Bitrate(); b != Floor {
		t.Errorf("a ceiling under the floor starts at %d", b)
	}
}

// The feedback is a stranger's: one naming packets this controller never sent changes nothing, and one
// claiming 65,535 packets is read for MaxStatuses of them.
func TestFeedbackIsBounded(t *testing.T) {
	l := newLink(t, 64000)
	l.rate = 64000
	l.run(25*time.Second, true)
	before, loss := l.c.Bitrate(), l.c.LossPercent()
	forged := &rtcp.TransportLayerCC{BaseSequenceNumber: l.seq + 2000, PacketStatusCount: 7,
		PacketChunks: []rtcp.PacketStatusChunk{&rtcp.RunLengthChunk{
			PacketStatusSymbol: rtcp.TypeTCCPacketNotReceived, RunLength: 7}}}
	for range 100 {
		l.now = l.now.Add(100 * time.Millisecond)
		l.c.Feedback(forged, l.now)
	}
	if after := l.c.Bitrate(); after != before {
		t.Errorf("feedback on packets never sent moved the bitrate from %d to %d", before, after)
	}
	if after := l.c.LossPercent(); after != loss {
		t.Errorf("feedback on packets never sent moved the loss from %d%% to %d%%", loss, after)
	}
	// Sequence numbers sharing the ring's places with packets just sent, which no feedback has counted yet.
	for range 3 {
		l.send(100)
	}
	lost := l.c.lost
	l.c.Feedback(&rtcp.TransportLayerCC{BaseSequenceNumber: l.seq - 2 + ring, PacketStatusCount: 3,
		PacketChunks: []rtcp.PacketStatusChunk{&rtcp.RunLengthChunk{
			PacketStatusSymbol: rtcp.TypeTCCPacketNotReceived, RunLength: 3}}}, l.now)
	if l.c.lost != lost {
		t.Errorf("feedback naming another packet's place in the ring counted %v lost", l.c.lost-lost)
	}
	huge := &rtcp.TransportLayerCC{BaseSequenceNumber: l.seq, PacketStatusCount: 65535}
	for range 8 {
		huge.PacketChunks = append(huge.PacketChunks, &rtcp.RunLengthChunk{
			PacketStatusSymbol: rtcp.TypeTCCPacketNotReceived, RunLength: 8191})
	}
	l.c.visited = 0
	l.c.Feedback(huge, l.now)
	if l.c.visited > MaxStatuses {
		t.Errorf("one feedback had %d packets read, past %d", l.c.visited, MaxStatuses)
	}
}

// A packet received without a delta (symbol 3) takes none of the deltas, which belong to the packets after
// it; and a feedback whose deltas run out stops there, rather than counting what follows against packets
// it cannot place.
func TestEachDeltaIsTheRightPacketsAndAShortFeedbackStops(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	c := New(64000, start)
	for seq := range uint16(8) {
		c.Sent(seq, start.Add(time.Duration(seq)*20*time.Millisecond))
	}
	at := start.Add(time.Second)
	two := func(syms ...uint16) *rtcp.StatusVectorChunk {
		return &rtcp.StatusVectorChunk{SymbolSize: rtcp.TypeTCCSymbolSizeTwoBit, SymbolList: syms}
	}
	// Four packets, 20 ms apart as sent, each arriving 20 ms after the last: no queue. The first arrived
	// without a delta, so the three deltas are the next three packets'. Read off by one, the third would
	// arrive at the second's time and the base would be set 20 ms early, making the rest look queued.
	c.Feedback(&rtcp.TransportLayerCC{BaseSequenceNumber: 0, PacketStatusCount: 4, ReferenceTime: 100,
		PacketChunks: []rtcp.PacketStatusChunk{two(3, 1, 1, 1)},
		RecvDeltas:   []*rtcp.RecvDelta{{Delta: 20000}, {Delta: 20000}, {Delta: 20000}}}, at)
	if c.lost != 0 || c.counted != 4 {
		t.Errorf("four received packets counted as %v lost of %v", c.lost, c.counted)
	}
	if want := 100*64*time.Millisecond + 20*time.Millisecond - 20*time.Millisecond; c.minNow != want {
		t.Errorf("the base delay is %v, want %v: a delta was given to the wrong packet", c.minNow, want)
	}
	// Two received packets with one delta between them, then, in the next chunk, two lost: the walk stops
	// at the second, the whole walk and not only its chunk, so the two after it are not counted lost.
	c.Feedback(&rtcp.TransportLayerCC{BaseSequenceNumber: 4, PacketStatusCount: 4, ReferenceTime: 100,
		PacketChunks: []rtcp.PacketStatusChunk{two(1, 1), &rtcp.RunLengthChunk{
			PacketStatusSymbol: rtcp.TypeTCCPacketNotReceived, RunLength: 2}},
		RecvDeltas: []*rtcp.RecvDelta{{Delta: 100000}}}, at)
	if c.lost != 0 {
		t.Errorf("a feedback whose deltas ran out counted %v packets lost after it", c.lost)
	}
	// The same in run-length chunks: three received with one delta, then two lost.
	for seq := uint16(8); seq < 13; seq++ {
		c.Sent(seq, start.Add(time.Duration(seq)*20*time.Millisecond))
	}
	c.Feedback(&rtcp.TransportLayerCC{BaseSequenceNumber: 8, PacketStatusCount: 5, ReferenceTime: 100,
		PacketChunks: []rtcp.PacketStatusChunk{
			&rtcp.RunLengthChunk{PacketStatusSymbol: rtcp.TypeTCCPacketReceivedSmallDelta, RunLength: 3},
			&rtcp.RunLengthChunk{PacketStatusSymbol: rtcp.TypeTCCPacketNotReceived, RunLength: 2}},
		RecvDeltas: []*rtcp.RecvDelta{{Delta: 100000}}}, at)
	if c.lost != 0 {
		t.Errorf("a feedback whose deltas ran out in a run counted %v packets lost after it", c.lost)
	}
}

// A packet reported twice, by a feedback arriving twice or by pion's recorder after a reordering, is
// counted once.
func TestAPacketIsCountedOnce(t *testing.T) {
	l := newLink(t, 64000)
	l.run(time.Second, true)
	fb := &rtcp.TransportLayerCC{BaseSequenceNumber: l.seq + 1, PacketStatusCount: 1,
		PacketChunks: []rtcp.PacketStatusChunk{&rtcp.RunLengthChunk{
			PacketStatusSymbol: rtcp.TypeTCCPacketNotReceived, RunLength: 1}}}
	l.send(100)
	before := l.c.lost
	for range 5 {
		l.c.Feedback(fb, l.now)
	}
	if got := l.c.lost - before; got != 1 {
		t.Errorf("one packet reported lost five times counted %v times", got)
	}
}
