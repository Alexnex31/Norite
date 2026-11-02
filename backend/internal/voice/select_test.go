// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/voice/voicetest"
	"github.com/Alexnex31/Norite/backend/voiceproto"
)

func TestLoudnessIsBelievedOnlyWhilePacketsFlow(t *testing.T) {
	src := &source{}
	at := time.Now()
	loud := []byte{20}
	for i := range 50 {
		src.hear(loud, at.Add(time.Duration(i)*20*time.Millisecond))
	}
	end := at.Add(49 * 20 * time.Millisecond)
	if l, speaking := src.judge(end); !speaking || l < 10 {
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
		quiet.hear([]byte{127}, at.Add(time.Duration(i)*20*time.Millisecond))
	}
	if _, speaking := quiet.judge(end); speaking {
		t.Fatal("a sender reporting silence is speaking")
	}
	none := &source{}
	none.hear(nil, at)
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
