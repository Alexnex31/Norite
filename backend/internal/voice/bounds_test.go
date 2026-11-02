// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Alexnex31/Norite/backend/internal/voice/voicetest"
	"github.com/Alexnex31/Norite/backend/voiceproto"
)

func TestMeterAdmitsTheBoundAndStrikesPastTwiceIt(t *testing.T) {
	var m meter
	at := time.Now()
	burst := func(n int) (admitted int, out bool) {
		for i := range n {
			ok, o := m.admit(at.Add(time.Duration(i)*time.Second/time.Duration(n)), 0, 10, 0, 2)
			if ok {
				admitted++
			}
			out = out || o
		}
		at = at.Add(time.Second)
		return admitted, out
	}
	if got, out := burst(15); got != 10 || out {
		t.Fatalf("one second at 1.5x the bound: %d admitted, out %v", got, out)
	}
	if _, out := burst(15); out {
		t.Fatal("a second at 1.5x struck out")
	}
	burst(25)
	if _, out := burst(25); out {
		t.Fatal("one strike is out")
	}
	if _, out := burst(1); !out {
		t.Fatal("two seconds past twice the bound are not out")
	}

	var clean meter
	at = time.Now()
	for i := range 25 {
		clean.admit(at.Add(time.Duration(i)*40*time.Millisecond), 0, 10, 0, 2)
	}
	clean.admit(at.Add(5*time.Second), 0, 10, 0, 2)
	for i := range 25 {
		if _, out := clean.admit(at.Add(5*time.Second+time.Duration(i)*40*time.Millisecond), 0, 10, 0, 2); out {
			t.Fatal("a strike survived a quiet gap")
		}
	}

	var bits meter
	if ok, _ := bits.admit(time.Now(), 200, 100, 1000, 3); ok {
		t.Fatal("1600 bits admitted against a bound of 1000")
	}
}

func sendPaced(t *testing.T, c *voicetest.Client, n, size int, pace time.Duration) {
	t.Helper()
	packets := make([][]byte, n)
	for i := range packets {
		packets[i] = bytes.Repeat([]byte{0xfc}, size)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = c.Send(ctx, packets, voicetest.SendOptions{Pace: pace, Level: voicetest.Loud})
}

func TestASenderPastThePacketBoundIsCappedThenClosed(t *testing.T) {
	s := newSFU(t, func(l *Limits) { l.PacketsPerSecond = 50; l.StrikesToEvict = 2 })
	room := ids.MustNext()
	recv := join(t, s, room, ids.MustNext(), 1)
	flood := join(t, s, room, ids.MustNext(), 1)

	start := time.Now()
	go sendPaced(t, flood.client, 1000, 3, 4*time.Millisecond)
	eventually(t, 6*time.Second, "a sender at five times the bound is closed", func() bool { return !s.hasConn(flood.conn) })
	elapsed := time.Since(start)
	got := len(recv.client.ReceivedAll())
	if limit := int(elapsed.Seconds()+1) * 50; got > limit {
		t.Fatalf("%d packets forwarded in %s, past %d", got, elapsed, limit)
	}
}

func TestASenderWithinTwiceTheBoundIsCappedAndKept(t *testing.T) {
	s := newSFU(t, func(l *Limits) { l.PacketsPerSecond = 50; l.StrikesToEvict = 2 })
	room := ids.MustNext()
	recv := join(t, s, room, ids.MustNext(), 1)
	eager := join(t, s, room, ids.MustNext(), 1)

	start := time.Now()
	sendPaced(t, eager.client, 225, 3, 13*time.Millisecond) // about 75 a second for three seconds
	elapsed := time.Since(start)
	if !s.hasConn(eager.conn) {
		t.Fatal("a sender at 1.5x the bound was closed")
	}
	time.Sleep(200 * time.Millisecond)
	got := len(recv.client.ReceivedAll())
	if limit := int(elapsed.Seconds()+1) * 50; got > limit || got < 100 {
		t.Fatalf("%d packets forwarded in %s; want the bound's share, at most %d", got, elapsed, limit)
	}
}

func TestASenderPastTheBitBoundIsClosed(t *testing.T) {
	s := newSFU(t, func(l *Limits) { l.BitsPerSecond = 40_000; l.StrikesToEvict = 2 })
	room := ids.MustNext()
	join(t, s, room, ids.MustNext(), 1)
	heavy := join(t, s, room, ids.MustNext(), 1)
	go sendPaced(t, heavy.client, 300, 500, 20*time.Millisecond) // 200 kbit/s
	eventually(t, 6*time.Second, "a sender at five times the bit bound is closed", func() bool { return !s.hasConn(heavy.conn) })
}

func TestAPeerPastTheRTCPBoundIsClosed(t *testing.T) {
	s := newSFU(t, func(l *Limits) { l.RTCPPerSecond = 20; l.StrikesToEvict = 2 })
	room := ids.MustNext()
	flood := join(t, s, room, ids.MustNext(), 1)
	talker := join(t, s, room, ids.MustNext(), 1)
	send(t, map[*voicetest.Client][][]byte{talker.client: voicetest.TonePackets()[:5]}, voicetest.SendOptions{})
	got := flood.client.ReceivedAll()
	if len(got) == 0 {
		t.Fatal("nothing arrived to name a slot by")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = flood.client.FloodRTCP(ctx, got[0].SSRC, 2000, 2*time.Millisecond) }()
	eventually(t, 6*time.Second, "a peer flooding RTCP is closed", func() bool { return !s.hasConn(flood.conn) })
	if !s.hasConn(talker.conn) {
		t.Fatal("another peer was closed with it")
	}
}

func TestMessagesPastTheBoundAreRefused(t *testing.T) {
	s := newSFU(t, func(l *Limits) { l.MessagesPerSecond = 5 })
	j := join(t, s, ids.MustNext(), ids.MustNext(), 1)
	waitOp(t, j.client, voiceproto.OpReady, nil)
	for range 10 {
		if err := j.client.Subscribe(nil, nil, nil); err != nil {
			break
		}
	}
	var r voiceproto.Refused
	if err := voiceproto.Decode(waitOp(t, j.client, voiceproto.OpRefused, nil), &r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Reason, "messages a second") {
		t.Fatalf("refused for another reason: %q", r.Reason)
	}
}

// A panic where a stranger's input is handled ends that one connection: the SFU shares the server's
// process, and nothing else in the room or the process may go with it.
func TestAPanicClosesOneConnectionAndNothingElse(t *testing.T) {
	hook := func(where string, input []byte) {
		if bytes.HasPrefix(input, []byte("boom")) {
			panic("a panic in " + where)
		}
	}
	testPanic.Store(&hook)
	t.Cleanup(func() { testPanic.Store(nil) })

	s := newSFU(t, nil)
	room := ids.MustNext()
	recv := join(t, s, room, ids.MustNext(), 2)
	talker := join(t, s, room, ids.MustNext(), 1)
	bomber := join(t, s, room, ids.MustNext(), 1)
	messenger := join(t, s, room, ids.MustNext(), 1)
	waitOp(t, messenger.client, voiceproto.OpReady, nil)

	send(t, map[*voicetest.Client][][]byte{bomber.client: {[]byte("boom")}}, voicetest.SendOptions{})
	if err := messenger.client.SendRawUnchecked([]byte("boom")); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "both panicking connections are closed", func() bool {
		return !s.hasConn(bomber.conn) && !s.hasConn(messenger.conn)
	})

	before := len(recv.client.ReceivedAll())
	send(t, map[*voicetest.Client][][]byte{talker.client: voicetest.TonePackets()[:10]}, voicetest.SendOptions{})
	if got := len(recv.client.ReceivedAll()) - before; got != 10 || !s.hasConn(recv.conn) || !s.hasConn(talker.conn) {
		t.Fatalf("after the panics the room forwarded %d of 10 packets", got)
	}
}

// An admitted peer holds the SRTP keys, and each SSRC it invents would open a stream with a buffer of its
// own that nothing reads and nothing frees until the connection ends. Past what its offer accounts for,
// the connection is closed.
func TestAPeerSendingToUndeclaredSSRCsIsClosed(t *testing.T) {
	s := newSFU(t, nil)
	room := ids.MustNext()
	spray := join(t, s, room, ids.MustNext(), 1)
	other := join(t, s, room, ids.MustNext(), 1)
	// The bound can close the connection before the spray ends, and the client's next write then fails;
	// that is the outcome under test, so the error is logged rather than fatal. A spray that never got
	// going leaves the connection up, which the wait below catches.
	if err := spray.client.SprayRTP(200); err != nil {
		t.Logf("the spray stopped: %v", err)
	}
	eventually(t, 5*time.Second, "a peer sending to undeclared SSRCs is closed", func() bool { return !s.hasConn(spray.conn) })
	if !s.hasConn(other.conn) {
		t.Fatal("another peer was closed with it")
	}
}

// A panic in the room's selection is recovered by its tick, and must not leave the room's lock held:
// the next close in the room takes the SFU's lock and then the room's, and would hold the first for ever.
func TestAPanicInSelectionLeavesNoLockHeld(t *testing.T) {
	s := newSFU(t, nil)
	room := ids.MustNext()
	a := join(t, s, room, ids.MustNext(), 1)
	b := join(t, s, room, ids.MustNext(), 1)
	s.mu.Lock()
	r := s.rooms[room]
	s.mu.Unlock()

	hook := func(where string, _ []byte) {
		if where == "selection" {
			panic("a panic in selection")
		}
	}
	testPanic.Store(&hook)
	r.tick()
	testPanic.Store(nil)

	done := make(chan struct{})
	go func() {
		_ = s.EvictConnection(context.Background(), a.conn)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a close after a contained panic never finished: a lock was left held")
	}
	if !s.hasConn(b.conn) {
		t.Fatal("the other connection went too")
	}
}
