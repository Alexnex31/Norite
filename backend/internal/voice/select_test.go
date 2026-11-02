// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/voice/voicetest"
	"github.com/Alexnex31/Norite/backend/voiceproto"
)

func TestLoudnessIsBelievedOnlyWhilePacketsFlow(t *testing.T) {
	src := &source{}
	at := time.Now()
	loud := []byte{20}
	for i := range 50 {
		src.hear(loud, uint32(i)*960, at.Add(time.Duration(i)*20*time.Millisecond))
	}
	end := at.Add(49 * 20 * time.Millisecond)
	if l, speaking := src.judge(end); !speaking || l < 8 {
		t.Fatalf("a second of loud packets: loudness %.1f, speaking %v", l, speaking)
	}
	// Between the thresholds it stays as it was: no flicker on the way down.
	if l, speaking := src.judge(end.Add(250 * time.Millisecond)); l < speakingUntil || !speaking {
		t.Fatalf("a quarter second on: loudness %.1f, speaking %v", l, speaking)
	}
	if l, speaking := src.judge(end.Add(time.Second)); speaking || l > 1 {
		t.Fatalf("a second after the last packet: loudness %.1f, speaking %v", l, speaking)
	}

	quiet := &source{}
	for i := range 50 {
		quiet.hear([]byte{127}, uint32(i)*960, at.Add(time.Duration(i)*20*time.Millisecond))
	}
	if _, speaking := quiet.judge(end); speaking {
		t.Fatal("a sender reporting silence is speaking")
	}
	none := &source{}
	none.hear(nil, 0, at)
	if l, speaking := none.judge(at); speaking || l != 0 {
		t.Fatal("a sender reporting no level is speaking")
	}
}

// talk has each sender send for a while at its own level, concurrently.
func talk(t *testing.T, d time.Duration, levels map[joined]uint8) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n := int(d / (20 * time.Millisecond))
	var wg sync.WaitGroup
	for j, lvl := range levels {
		packets := make([][]byte, n)
		for i := range packets {
			packets[i] = []byte{0xfc, 0xff, 0xfe}
		}
		wg.Go(func() {
			_ = j.client.Send(ctx, packets, voicetest.SendOptions{Level: func(int) *uint8 { return &lvl }})
		})
	}
	wg.Wait()
}

func holder(recv joined) string {
	m, _ := recv.client.LatestSlots()
	if len(m.Map) == 0 {
		return ""
	}
	return m.Map[0].User
}

func TestTheLoudestFillTheSlots(t *testing.T) {
	s := newSFU(t, nil)
	room := ids.MustNext()
	recv := join(t, s, room, ids.MustNext(), 1)
	b := join(t, s, room, ids.MustNext(), 1)
	c := join(t, s, room, ids.MustNext(), 1)

	talk(t, time.Second, map[joined]uint8{b: 127, c: 20})
	if got := holder(recv); got != c.user.String() {
		t.Fatalf("c spoke and b was silent; the slot holds %q", got)
	}
	talk(t, 1500*time.Millisecond, map[joined]uint8{b: 20, c: 127})
	if got := holder(recv); got != b.user.String() {
		t.Fatalf("b spoke and c fell silent; the slot holds %q", got)
	}
	// The slot changed hands at least twice and stayed one stream throughout.
	assertContinuous(t, recv.client.Received(recv.client.SlotMids()[0]))
}

func TestTheSlotsHolderKeepsItAgainstAnEqualVoice(t *testing.T) {
	s := newSFU(t, nil)
	room := ids.MustNext()
	recv := join(t, s, room, ids.MustNext(), 1)
	b := join(t, s, room, ids.MustNext(), 1)
	c := join(t, s, room, ids.MustNext(), 1)
	// b's track arrives first, so on a tie of loudness b comes first; c then holds the slot by speaking
	// alone. Only incumbency keeps it for c once the two are equal.
	talk(t, 200*time.Millisecond, map[joined]uint8{b: 127})
	talk(t, 600*time.Millisecond, map[joined]uint8{c: 20})
	if got := holder(recv); got != c.user.String() {
		t.Fatalf("c alone spoke; the slot holds %q", got)
	}
	before := len(recv.client.Messages())
	talk(t, time.Second, map[joined]uint8{b: 20, c: 20})
	for _, m := range recv.client.Messages()[before:] {
		if op, _ := voiceproto.OpOf(m); op == voiceproto.OpSlots {
			t.Fatalf("two equal voices traded the slot: %s", m)
		}
	}
}

func TestAPinOutranksTheLoudest(t *testing.T) {
	s := newSFU(t, nil)
	room := ids.MustNext()
	recv := join(t, s, room, ids.MustNext(), 1)
	b := join(t, s, room, ids.MustNext(), 1)
	c := join(t, s, room, ids.MustNext(), 1)
	if err := recv.client.Subscribe([]string{b.user.String()}, nil, nil); err != nil {
		t.Fatal(err)
	}
	talk(t, time.Second, map[joined]uint8{b: 127, c: 0})
	if got := holder(recv); got != b.user.String() {
		t.Fatalf("b was pinned; the slot holds %q", got)
	}
}

func speakingNow(t *testing.T, j joined) []string {
	t.Helper()
	msgs := j.client.Messages()
	for i := len(msgs) - 1; i >= 0; i-- {
		if op, _ := voiceproto.OpOf(msgs[i]); op == voiceproto.OpSpeaking {
			var m voiceproto.Speaking
			if err := voiceproto.Decode(msgs[i], &m); err != nil {
				t.Fatal(err)
			}
			return m.Users
		}
	}
	return nil
}

func TestSpeakingFollowsTheLevelAndThePackets(t *testing.T) {
	s := newSFU(t, nil)
	room := ids.MustNext()
	recv := join(t, s, room, ids.MustNext(), 1)
	b := join(t, s, room, ids.MustNext(), 1)
	excluded := join(t, s, room, ids.MustNext(), 1)
	if err := s.SetExclusion(context.Background(), room, recv.user, []snowflake.ID{excluded.user}); err != nil {
		t.Fatal(err)
	}

	talk(t, 600*time.Millisecond, map[joined]uint8{b: 20, excluded: 20})
	if got := speakingNow(t, recv); !slices.Equal(got, []string{b.user.String()}) {
		t.Fatalf("b spoke; the receiver hears %v speaking", got)
	}
	if got := speakingNow(t, b); !slices.Contains(got, excluded.user.String()) {
		t.Fatalf("somebody who did not exclude them hears %v speaking", got)
	}
	// Silence, by packets that report it, and then by no packets at all.
	talk(t, 600*time.Millisecond, map[joined]uint8{b: 127})
	if got := speakingNow(t, recv); len(got) != 0 {
		t.Fatalf("b went quiet; the receiver hears %v speaking", got)
	}
	talk(t, 600*time.Millisecond, map[joined]uint8{b: 20})
	eventually(t, 3*time.Second, "speaking ends when the packets do", func() bool {
		return len(speakingNow(t, recv)) == 0
	})
}

// choose itself, without the network's timing: b arrived first and is a little louder, c holds the slot.
func TestIncumbencyKeepsASlotAgainstAVoiceALittleLouder(t *testing.T) {
	b := &source{id: 1, conn: &conn{user: ids.MustNext()}}
	c := &source{id: 2, conn: &conn{user: ids.MustNext()}}
	s := &slot{}
	s.assign(c)
	pick := func(lb, lc float64) *source {
		return choose([]*source{b, c}, noSubscription, map[*source]float64{b: lb, c: lc}, []*slot{s})[0]
	}
	if got := pick(10, 9); got != c {
		t.Fatal("a voice a tenth louder took the slot from its holder")
	}
	if got := pick(12, 9); got != b {
		t.Fatal("a voice a third louder did not take the slot")
	}
	if got := pick(0, 0); got != c {
		t.Fatal("in silence the slot left its holder")
	}
}

// A microphone's noise floor, sent through silence without DTX, is not speech.
func TestNoiseIsNotSpeaking(t *testing.T) {
	src := &source{}
	at := time.Now()
	for i := range 250 {
		src.hear([]byte{55}, uint32(i)*960, at.Add(time.Duration(i)*20*time.Millisecond))
	}
	if l, speaking := src.judge(at.Add(5 * time.Second)); speaking {
		t.Fatalf("five seconds of -55 dBov is speaking, loudness %.1f", l)
	}
}

// Loudness is per time, not per packet: 10 ms frames at a level are as loud as 20 ms frames at it, and a
// sender claiming more time per packet than one frame gains nothing past it.
func TestLoudnessIsPerTimeNotPerPacket(t *testing.T) {
	at := time.Now()
	settle := func(frame time.Duration, ticksPerPacket uint32) float64 {
		src := &source{}
		n := int(2 * time.Second / frame)
		for i := range n {
			src.hear([]byte{20}, uint32(i)*ticksPerPacket, at.Add(time.Duration(i)*frame))
		}
		l, _ := src.judge(at.Add(time.Duration(n-1) * frame))
		return l
	}
	twenty, ten := settle(20*time.Millisecond, 960), settle(10*time.Millisecond, 480)
	if ten > twenty*1.1 || ten < twenty*0.9 {
		t.Fatalf("10 ms frames %.1f against 20 ms frames %.1f", ten, twenty)
	}
	if liar := settle(20*time.Millisecond, 4800); liar > twenty*1.01 {
		t.Fatalf("a sender claiming 100 ms a packet is %.1f against %.1f", liar, twenty)
	}
}

// A sender of 40 or 60 ms frames, which Opus allows and the answer does not forbid, is as loud as one of
// 20 ms frames at the same level, and speaks. Capped at one 20 ms frame a packet, 60 ms frames of a
// steady voice settled below the speaking threshold and lost every slot.
func TestLongerFramesAreAsLoud(t *testing.T) {
	at := time.Now()
	settle := func(frame time.Duration) (float64, bool) {
		src := &source{}
		ticks := uint32(frame / (20 * time.Millisecond) * 960)
		n := int(2 * time.Second / frame)
		for i := range n {
			src.hear([]byte{20}, uint32(i)*ticks, at.Add(time.Duration(i)*frame))
		}
		// Judged half a frame after the last packet: the selection's tick lands anywhere between two.
		return src.judge(at.Add(time.Duration(n-1)*frame + frame/2))
	}
	twenty, _ := settle(20 * time.Millisecond)
	for _, frame := range []time.Duration{40 * time.Millisecond, 60 * time.Millisecond} {
		l, speaking := settle(frame)
		if !speaking || l < twenty*0.75 {
			t.Fatalf("%s frames: loudness %.1f against %.1f for 20 ms, speaking %v", frame, l, twenty, speaking)
		}
	}
}

// A sender claiming more time than passes, 120 ms a packet at the packet bound, counts as no more than
// one speaking all the time: the wall clock bounds the claim, not the sender's timestamps.
func TestTimeClaimedPastTheClockCountsForNothing(t *testing.T) {
	at := time.Now()
	honest := &source{}
	liar := &source{}
	for i := range 100 {
		honest.hear([]byte{20}, uint32(i)*960, at.Add(time.Duration(i)*20*time.Millisecond))
	}
	// 150 packets a second, each claiming 120 ms.
	pace := time.Second / 150
	n := int(2 * time.Second / pace)
	for i := range n {
		liar.hear([]byte{20}, uint32(i)*5760, at.Add(time.Duration(i)*pace))
	}
	h, _ := honest.judge(at.Add(99 * 20 * time.Millisecond))
	l, _ := liar.judge(at.Add(time.Duration(n-1) * pace))
	if l > h*1.1 {
		t.Fatalf("claiming 120 ms a packet: loudness %.1f against %.1f speaking honestly", l, h)
	}
}

// The first packet after a silence counts for one frame, not for the silence its timestamp skipped.
func TestASilenceIsNotCountedAsSpeech(t *testing.T) {
	at := time.Now()
	src := &source{}
	for i := range 5 {
		src.hear([]byte{20}, uint32(i)*960, at.Add(time.Duration(i)*20*time.Millisecond))
	}
	before, _ := src.judge(at.Add(80 * time.Millisecond))
	// Two seconds of DTX, then one loud packet.
	resume := at.Add(80*time.Millisecond + 2*time.Second)
	src.hear([]byte{0}, 4*960+96000, resume)
	after, speaking := src.judge(resume)
	if speaking || after > 1.01 {
		t.Fatalf("one packet after a silence: loudness %.2f (from %.2f), speaking %v", after, before, speaking)
	}
}

// A tick that changes nothing publishes nothing: the forward path keeps the slice it has.
func TestAQuietTickPublishesNothing(t *testing.T) {
	r := benchRoom(8, 8)
	r.assign()
	before := map[*source]*[]*slot{}
	for _, src := range r.sources {
		before[src] = src.slots.Load()
	}
	r.assign()
	for _, src := range r.sources {
		if src.slots.Load() != before[src] {
			t.Fatal("a tick that changed nothing published a new slice")
		}
	}
}

// A cap is over the connection's slots of every kind together, as the contract says, not per kind: with
// video slots (M105–M107), a cap of two per kind would fill four.
func TestACapCountsEveryKindTogether(t *testing.T) {
	s := &SFU{lim: DefaultLimits(), log: zerolog.Nop()}
	r := newRoom(s, ids.MustNext())
	member := func() *conn {
		uid, cid := ids.MustNext(), ids.MustNext()
		c := &conn{sfu: s, room: r, id: cid, user: uid, userText: uid.String(), idText: cid.String(),
			sub: noSubscription, log: zerolog.Nop(), helloed: true}
		r.conns[c.id] = c
		r.perUser[c.user]++
		return c
	}
	recv := member()
	recv.slots = map[string][]*slot{}
	for _, kind := range []string{voiceproto.KindAudio, voiceproto.KindVideo} {
		for j := range 2 {
			recv.slots[kind] = append(recv.slots[kind], &slot{conn: recv, index: j, kind: kind, codec: "audio/opus",
				track: newSlotTrack(kind, j, opusCapability)})
		}
	}
	for i, kind := range []string{voiceproto.KindAudio, voiceproto.KindAudio, voiceproto.KindVideo, voiceproto.KindVideo} {
		src := &source{conn: member(), id: uint32(i + 1), kind: kind, codec: "audio/opus"}
		r.sources[src.id] = src
	}
	recv.sub = subscription{cap: 2}
	r.assign()
	if n := len(recv.slotMap); n != 2 {
		t.Fatalf("a cap of two filled %d slots across two kinds", n)
	}
}
