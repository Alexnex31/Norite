// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/voiceproto"
	"github.com/Alexnex31/Norite/daemon/voiceipc"
	"github.com/Alexnex31/Norite/voiceworker/internal/audio"
	"github.com/Alexnex31/Norite/voiceworker/internal/dsp"
	"github.com/Alexnex31/Norite/voiceworker/internal/jitter"
	"github.com/Alexnex31/Norite/voiceworker/internal/opus"
)

func speakingMsg(t *testing.T, users ...string) []byte {
	t.Helper()
	b, err := voiceproto.Encode(voiceproto.Speaking{Op: voiceproto.OpSpeaking, Users: users})
	require.NoError(t, err)
	return b
}

func slotsMsg(t *testing.T, entries ...voiceproto.SlotEntry) []byte {
	t.Helper()
	b, err := voiceproto.Encode(voiceproto.Slots{Op: voiceproto.OpSlots, Map: entries})
	require.NoError(t, err)
	return b
}

// The SFU's speaking lists, as fast as it sends them, reach the daemon a few a second, and the newest
// always does (M34): the daemon's bound on the worker's frames is not the SFU's to break. Read as the
// worker's loop reads them, at once.
func TestSpeakingIsSpacedAndTheNewestArrives(t *testing.T) {
	c := newControl(dsp.AllOn())
	defer c.stop()
	var mu sync.Mutex
	var got []voiceipc.Speaking
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case s := <-c.speakingOut:
				mu.Lock()
				got = append(got, s)
				mu.Unlock()
			case <-done:
				return
			}
		}
	}()
	deadline := time.Now().Add(time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		c.onSpeaking(speakingMsg(t, fmt.Sprint(i+1)))
		time.Sleep(2 * time.Millisecond)
	}
	c.onSpeaking(speakingMsg(t, "9999"))
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) > 0 && got[len(got)-1].Users[0] == "9999"
	}, time.Second, time.Millisecond)
	time.Sleep(2 * speakingEvery)
	mu.Lock()
	defer mu.Unlock()
	require.LessOrEqual(t, len(got), int(time.Second/speakingEvery)+2, "%d lists in a second", len(got))
}

// What the SFU says that is not an id, or more slots than the worker has, changes nothing.
func TestTheSFUsListsAreBounded(t *testing.T) {
	c := newControl(dsp.AllOn())
	defer c.stop()
	c.subscribe(voiceipc.Subscribe{Drop: []string{"7"}})
	c.onSlots(slotsMsg(t, voiceproto.SlotEntry{Slot: 0, Kind: "audio", Source: 1, User: "7", Conn: "8"}))
	require.True(t, c.isMuted(1))

	c.onSlots(slotsMsg(t, voiceproto.SlotEntry{Slot: 0, Kind: "audio", Source: 2, User: "7x", Conn: "8"}))
	require.True(t, c.isMuted(1), "a slot map naming no id replaced the last")
	var many []voiceproto.SlotEntry
	for i := range maxSlotMap + 1 {
		many = append(many, voiceproto.SlotEntry{Slot: i, Kind: "audio", Source: uint32(10 + i), User: "7", Conn: "8"})
	}
	c.onSlots(slotsMsg(t, many...))
	require.True(t, c.isMuted(1), "a slot map past the slots replaced the last")
	require.False(t, c.isMuted(10))

	c.onSpeaking(speakingMsg(t, "1", "not an id"))
	select {
	case s := <-c.speakingOut:
		t.Fatalf("a list naming no id was handed over: %v", s)
	case <-time.After(2 * speakingEvery):
	}
}

// A source is muted while its sender is dropped, whichever of the two changed last.
func TestASourceIsMutedForItsSendersDrop(t *testing.T) {
	c := newControl(dsp.AllOn())
	defer c.stop()
	c.onSlots(slotsMsg(t, voiceproto.SlotEntry{Slot: 0, Kind: "audio", Source: 1, User: "7", Conn: "8"},
		voiceproto.SlotEntry{Slot: 1, Kind: "audio", Source: 2, User: "9", Conn: "10"}))
	require.False(t, c.isMuted(1))
	c.subscribe(voiceipc.Subscribe{Drop: []string{"7"}})
	require.True(t, c.isMuted(1))
	require.False(t, c.isMuted(2))
	// The dropped sender takes another slot, under another source.
	c.onSlots(slotsMsg(t, voiceproto.SlotEntry{Slot: 2, Kind: "audio", Source: 3, User: "7", Conn: "8"}))
	require.True(t, c.isMuted(3))
	require.True(t, c.isMuted(1), "the sender's first source is still theirs")
	c.subscribe(voiceipc.Subscribe{Drop: []string{}})
	require.False(t, c.isMuted(3))
	require.False(t, c.isMuted(1))
}

// A muted sender the SFU took off the slot map is still muted: what of theirs is buffered is still theirs.
// The senders remembered are bounded.
func TestAMutedSenderStaysMutedOffTheMap(t *testing.T) {
	c := newControl(dsp.AllOn())
	defer c.stop()
	c.onSlots(slotsMsg(t, voiceproto.SlotEntry{Slot: 0, Kind: "audio", Source: 1, User: "7", Conn: "8"}))
	c.subscribe(voiceipc.Subscribe{Drop: []string{"7"}})
	c.onSlots(slotsMsg(t, voiceproto.SlotEntry{Slot: 0, Kind: "audio", Source: 2, User: "9", Conn: "10"}))
	require.True(t, c.isMuted(1), "a muted sender off the map was unmuted")
	for i := range maxSources {
		c.onSlots(slotsMsg(t, voiceproto.SlotEntry{Slot: 0, Kind: "audio", Source: uint32(100 + i), User: "9",
			Conn: "10"}))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	require.Len(t, c.seen, maxSources)
	require.Len(t, c.order, maxSources)
}

// A local mute is judged by whose samples the mix plays, never by whom the slot holds now: a packet of the
// next sender's moves the slot between the frame's decode and its mix (M34's /code-review). Staged at that
// moment: the slot holds the unmuted sender 9 while its samples are the muted 7's, decoded before.
func TestALocalMuteJudgesTheSamplesNotTheSlot(t *testing.T) {
	c := newControl(dsp.AllOn())
	defer c.stop()
	c.onSlots(slotsMsg(t, voiceproto.SlotEntry{Slot: 0, Kind: "audio", Source: 1, User: "7", Conn: "8"},
		voiceproto.SlotEntry{Slot: 1, Kind: "audio", Source: 2, User: "9", Conn: "10"}))
	c.subscribe(voiceipc.Subscribe{Drop: []string{"7"}})
	require.True(t, c.isMuted(1))
	require.False(t, c.isMuted(2))

	play := func(from uint32, holds uint32) int16 {
		buf := jitter.New()
		buf.Push(holds, 0, 0, []byte{0xf8, 0xff, 0xfe}, time.Now())
		sl := &slot{buf: buf, pcm: make([]int16, audio.FrameSamples, opus.MaxDecodeSamples+audio.FrameSamples),
			tmp: make([]int16, opus.MaxDecodeSamples), from: from, fromOK: true}
		for i := range sl.pcm {
			sl.pcm[i] = 1000
		}
		m := &media{s: &Session{ctl: c}, base: time.Now(), slots: []*slot{sl}}
		out := make([]int16, audio.FrameSamples)
		m.fill(out)
		return out[0]
	}
	require.Equal(t, int16(0), play(1, 2), "the muted sender's samples played under the next sender's name")
	require.Equal(t, int16(1000), play(2, 1), "the unmuted sender's samples were muted for whom the slot holds")
}

// sends records what a control sends on its data channel.
type sends struct {
	mu  sync.Mutex
	got []string
}

func (s *sends) SendText(m string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, m)
	return nil
}

func (s *sends) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.got...)
}

// A subscription set before the SFU's ready waits for it and is sent then: the channel is open before its
// hello goes, and the SFU closes a call whose first message is not hello (M34's /code-review).
func TestASubscriptionWaitsForTheSFUsReady(t *testing.T) {
	c := newControl(dsp.AllOn())
	dc := &sends{}
	c.dc = dc
	two := 2
	c.subscribe(voiceipc.Subscribe{Drop: []string{"7"}, Cap: &two})
	require.Empty(t, dc.seen(), "a subscribe went before the SFU's ready")
	c.onReady()
	want, err := voiceproto.Encode(voiceproto.Subscribe{Op: voiceproto.OpSubscribe, Pin: []string{},
		Drop: []string{"7"}, Cap: &two})
	require.NoError(t, err)
	require.Equal(t, []string{string(want)}, dc.seen())
	c.subscribe(voiceipc.Subscribe{Drop: []string{}})
	require.Len(t, dc.seen(), 2, "a subscribe after ready is sent at once")
}
