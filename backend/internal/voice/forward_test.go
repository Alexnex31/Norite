// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/voice/voicetest"
)

// send has each client send its packets at once and waits for all of them, then for the tail to arrive.
func send(t *testing.T, sends map[*voicetest.Client][][]byte, opts voicetest.SendOptions) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, len(sends))
	for c, packets := range sends {
		wg.Go(func() { errs <- c.Send(ctx, packets, opts) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(300 * time.Millisecond)
}

// assertContinuous checks a slot's packets form one stream: each sequence number one more than the last,
// each timestamp later.
func assertContinuous(t *testing.T, got []voicetest.Packet) {
	t.Helper()
	for i := 1; i < len(got); i++ {
		if got[i].Seq != got[i-1].Seq+1 {
			t.Fatalf("packet %d: sequence %d after %d", i, got[i].Seq, got[i-1].Seq)
		}
		if int32(got[i].TS-got[i-1].TS) <= 0 {
			t.Fatalf("packet %d: timestamp %d after %d", i, got[i].TS, got[i-1].TS)
		}
	}
}

// The milestone's done-when: two clients exchange real Opus through the SFU, over ICE, DTLS and SRTP, every
// payload byte-identical and in order, in one continuous stream, and what arrives is a file a person can
// play.
func TestTwoClientsExchangeRealOpus(t *testing.T) {
	s := newSFU(t, nil)
	room := ids.MustNext()
	a := join(t, s, room, ids.MustNext(), 1)
	b := join(t, s, room, ids.MustNext(), 1)

	tone := voicetest.TonePackets()
	send(t, map[*voicetest.Client][][]byte{a.client: tone, b.client: tone}, voicetest.SendOptions{Level: voicetest.Loud})

	for name, c := range map[string]*voicetest.Client{"a": a.client, "b": b.client} {
		assertTone(t, name, c, tone)
	}
}

// assertTone holds what c's first slot received to the tone, sent once: every payload byte-identical and
// in order, timestamps a frame apart, one source named throughout, and a valid Ogg file once written.
func assertTone(t *testing.T, name string, c *voicetest.Client, tone [][]byte) {
	t.Helper()
	got := c.Received(c.SlotMids()[0])
	if len(got) != len(tone) {
		t.Fatalf("%s received %d packets, want %d", name, len(got), len(tone))
	}
	for i, p := range got {
		if !bytes.Equal(p.Payload, tone[i]) {
			t.Fatalf("%s: packet %d differs from what was sent", name, i)
		}
		if p.TS-got[0].TS != uint32(i)*voicetest.FrameSamples {
			t.Fatalf("%s: packet %d at timestamp %d, want %d", name, i, p.TS-got[0].TS, i*voicetest.FrameSamples)
		}
		if len(p.CSRC) != 1 || p.CSRC[0] != got[0].CSRC[0] {
			t.Fatalf("%s: packet %d names sources %v", name, i, p.CSRC)
		}
	}
	assertContinuous(t, got)
	if !got[0].Marker {
		t.Fatalf("%s: the first packet does not mark the start of a talkspurt", name)
	}

	var file bytes.Buffer
	if err := voicetest.WriteOgg(&file, got); err != nil {
		t.Fatal(err)
	}
	back, err := voicetest.ReadOgg(&file)
	if err != nil {
		t.Fatalf("%s: what arrived is not a valid Ogg file: %v", name, err)
	}
	if len(back) != len(tone) {
		t.Fatalf("%s: the file holds %d packets, want %d", name, len(back), len(tone))
	}
}

func TestOnlyTheAudioLevelReachesTheReceiver(t *testing.T) {
	s := newSFU(t, nil)
	room := ids.MustNext()
	a := join(t, s, room, ids.MustNext(), 1)
	b := join(t, s, room, ids.MustNext(), 1)

	send(t, map[*voicetest.Client][][]byte{a.client: voicetest.TonePackets()[:20]},
		voicetest.SendOptions{Level: voicetest.Loud, Extra: []byte("sender's own")})
	got := b.client.Received(b.client.SlotMids()[0])
	if len(got) == 0 {
		t.Fatal("nothing arrived")
	}
	for i, p := range got {
		if p.Extensions != 1 || p.Level != int(*voicetest.Loud(0)) {
			t.Fatalf("packet %d: %d extensions, level %d; want the audio level alone, as sent", i, p.Extensions, p.Level)
		}
	}
}

func TestAThirdJoiningNeedsNoRenegotiation(t *testing.T) {
	s := newSFU(t, nil)
	room := ids.MustNext()
	a := join(t, s, room, ids.MustNext(), 2)
	b := join(t, s, room, ids.MustNext(), 2)
	// a's offer and the SFU's answer are the only negotiation a ever does.
	c := join(t, s, room, ids.MustNext(), 2)

	tone := voicetest.TonePackets()[:25]
	send(t, map[*voicetest.Client][][]byte{b.client: tone, c.client: tone}, voicetest.SendOptions{Level: voicetest.Loud})

	sources := map[uint32]int{}
	for _, mid := range a.client.SlotMids() {
		got := a.client.Received(mid)
		if len(got) != len(tone) {
			t.Fatalf("slot %s received %d packets, want %d", mid, len(got), len(tone))
		}
		sources[got[0].CSRC[0]]++
	}
	if len(sources) != 2 {
		t.Fatalf("a's two slots carry sources %v, want b's and c's", sources)
	}
}

// A slot whose sender changes stays one stream: the sequence numbers run on, the timestamps advance, and the
// CSRC changes at exactly the new sender's first packet. Synthetic payloads, so which sender a packet came
// from is known without trusting the CSRC under test.
func TestASlotChangingHandsStaysOneStream(t *testing.T) {
	s := newSFU(t, nil)
	room := ids.MustNext()
	recv := join(t, s, room, ids.MustNext(), 1)
	first := join(t, s, room, ids.MustNext(), 1)
	second := join(t, s, room, ids.MustNext(), 1)

	tagged := func(tag byte, n int) [][]byte {
		out := make([][]byte, n)
		for i := range out {
			out[i] = fmt.Appendf([]byte{tag}, "%04d", i)
		}
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	// A source is numbered when its first packet arrives, and the lower number has the slot: the first
	// sender starts first, so it has the slot until it leaves and the second takes it.
	wg.Go(func() { _ = first.client.Send(ctx, tagged('A', 50), voicetest.SendOptions{}) })
	time.Sleep(200 * time.Millisecond)
	wg.Go(func() { _ = second.client.Send(ctx, tagged('B', 100), voicetest.SendOptions{}) })
	time.Sleep(300 * time.Millisecond)
	if err := s.EvictConnection(context.Background(), first.conn); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	time.Sleep(300 * time.Millisecond)

	got := recv.client.Received(recv.client.SlotMids()[0])
	csrcOf := map[byte]uint32{}
	switched := -1
	for i, p := range got {
		tag := p.Payload[0]
		if c, ok := csrcOf[tag]; ok && c != p.CSRC[0] {
			t.Fatalf("packet %d from %c names source %d, then %d", i, tag, c, p.CSRC[0])
		}
		csrcOf[tag] = p.CSRC[0]
		if i > 0 && tag != got[i-1].Payload[0] {
			if switched >= 0 {
				t.Fatalf("the slot changed hands twice, at %d and %d", switched, i)
			}
			switched = i
		}
	}
	if switched < 0 || csrcOf['A'] == csrcOf['B'] {
		t.Fatalf("no change of hands seen (at %d, sources %v)", switched, csrcOf)
	}
	if got[0].Payload[0] != 'A' {
		t.Fatal("the slot did not start with the first sender")
	}
	if !got[switched].Marker {
		t.Fatal("the new sender's first packet does not mark a talkspurt")
	}
	assertContinuous(t, got)
	gap := got[switched].At.Sub(got[switched-1].At)
	ticks := got[switched].TS - got[switched-1].TS
	// At least one frame, so the new sender's frame starts where the last one ended, and about the time
	// that passed.
	if want := uint32(gap.Seconds() * 48000); ticks < 960 || ticks+960 < want || ticks > want+960*5 {
		t.Fatalf("the timestamp advanced %d ticks over %s at the switch", ticks, gap)
	}
}

func TestNobodyReceivesTheirOwnUsersTracks(t *testing.T) {
	s := newSFU(t, nil)
	room, user := ids.MustNext(), ids.MustNext()
	one := join(t, s, room, user, 1)
	two := join(t, s, room, user, 1)
	send(t, map[*voicetest.Client][][]byte{one.client: voicetest.TonePackets()[:10]}, voicetest.SendOptions{})
	if n := len(two.client.ReceivedAll()); n != 0 {
		t.Fatalf("a user's second connection received %d packets of their first", n)
	}
}

func TestAnExcludedUserIsNotForwarded(t *testing.T) {
	s := newSFU(t, nil)
	room := ids.MustNext()
	recv := join(t, s, room, ids.MustNext(), 2)
	excluded := join(t, s, room, ids.MustNext(), 1)
	other := join(t, s, room, ids.MustNext(), 1)
	if err := s.SetExclusion(context.Background(), room, recv.user, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetExclusion(context.Background(), room, recv.user, []snowflake.ID{excluded.user}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetExclusion(context.Background(), room, ids.MustNext(), nil); err == nil {
		t.Fatal("an exclusion for somebody not in the room was accepted")
	}

	tagged := func(tag byte) [][]byte {
		out := make([][]byte, 20)
		for i := range out {
			out[i] = []byte{tag}
		}
		return out
	}
	send(t, map[*voicetest.Client][][]byte{excluded.client: tagged('X'), other.client: tagged('O')}, voicetest.SendOptions{})
	all := recv.client.ReceivedAll()
	if len(all) == 0 {
		t.Fatal("nothing arrived from the user not excluded")
	}
	for _, p := range all {
		if p.Payload[0] == 'X' {
			t.Fatal("an excluded user's packet was forwarded")
		}
	}
}
