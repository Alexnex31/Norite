// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/pion/webrtc/v4"

	"github.com/Alexnex31/Norite/backend/internal/voice/voicetest"
	"github.com/Alexnex31/Norite/backend/voiceproto"
)

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

// Past the bound a message is dropped and the connection kept: a burst of pins clicked at once is a
// client, not a flood.
func TestMessagesPastTheBoundAreDropped(t *testing.T) {
	s := newSFU(t, func(l *Limits) { l.MessagesPerSecond = 5 })
	j := join(t, s, ids.MustNext(), ids.MustNext(), 1)
	waitOp(t, j.client, voiceproto.OpReady, nil)
	for range 9 {
		if err := j.client.Subscribe(nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(1500 * time.Millisecond)
	if !s.hasConn(j.conn) {
		t.Fatal("a burst within twice the bound closed the connection")
	}
	for _, m := range j.client.Messages() {
		if op, _ := voiceproto.OpOf(m); op == voiceproto.OpRefused {
			t.Fatalf("a burst within twice the bound was refused: %s", m)
		}
	}
}

// Past twice the bound for StrikesToEvict seconds in a row, the peer is refused and closed.
func TestAPeerPastTwiceTheMessageBoundIsRefused(t *testing.T) {
	s := newSFU(t, func(l *Limits) { l.MessagesPerSecond = 5; l.StrikesToEvict = 2 })
	j := join(t, s, ids.MustNext(), ids.MustNext(), 1)
	waitOp(t, j.client, voiceproto.OpReady, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	go func() {
		// Fifteen a second, past twice five.
		tick := time.NewTicker(time.Second / 15)
		defer tick.Stop()
		for ctx.Err() == nil {
			<-tick.C
			if j.client.Subscribe(nil, nil, nil) != nil {
				return
			}
		}
	}()
	var r voiceproto.Refused
	if err := voiceproto.Decode(waitOp(t, j.client, voiceproto.OpRefused, nil), &r); err != nil {
		t.Fatal(err)
	}
	cancel()
	if !strings.Contains(r.Reason, "messages a second") {
		t.Fatalf("refused for another reason: %q", r.Reason)
	}
	eventually(t, 5*time.Second, "the flooding peer is closed", func() bool { return !s.hasConn(j.conn) })
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

// A panic in the room's selection is contained wherever the selection runs, and leaves no lock held. The
// tick is one place; a close is another, and there the panic would otherwise surface in the goroutine of
// whoever closed: here the test's own, which it would end.
func TestAPanicInSelectionIsContainedWhereverItRuns(t *testing.T) {
	s := newSFU(t, nil)
	room := ids.MustNext()
	a := join(t, s, room, ids.MustNext(), 1)
	b := join(t, s, room, ids.MustNext(), 1)
	s.mu.Lock()
	r := s.rooms[room]
	s.mu.Unlock()

	var panics atomic.Int32
	hook := func(where string, _ []byte) {
		if where == "selection" {
			panics.Add(1)
			panic("a panic in selection")
		}
	}
	testPanic.Store(&hook)
	t.Cleanup(func() { testPanic.Store(nil) })
	r.changed()

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
	testPanic.Store(nil)
	if panics.Load() < 2 {
		t.Fatalf("the selection ran %d times, and the close's run is the one under test", panics.Load())
	}
	if !s.hasConn(b.conn) {
		t.Fatal("the other connection went too")
	}
}

// A peer knows its own ICE credentials and the SFU's, from the answer, so it can send authenticated
// binding requests from as many source addresses as it likes, and ICE adds a peer-reflexive candidate for
// each. Past the bound, none is added.
func TestBindingRequestsFromManyAddressesAreBounded(t *testing.T) {
	s := newSFU(t, nil)
	client := newClient(t, 1)
	offer := offerOf(t, client)
	connID := ids.MustNext()
	answer, err := s.Provision(context.Background(), ids.MustNext(), ids.MustNext(), connID, ids.MustNext(), offer)
	if err != nil {
		t.Fatal(err)
	}
	attr := func(sdp, key string) string {
		for line := range strings.SplitSeq(sdp, "\r\n") {
			if v, ok := strings.CutPrefix(line, "a="+key+":"); ok {
				return v
			}
		}
		t.Fatalf("no a=%s", key)
		return ""
	}
	username := attr(answer, "ice-ufrag") + ":" + attr(offer, "ice-ufrag")
	password := attr(answer, "ice-pwd")
	target := s.opts.Conn.LocalAddr()

	const sources = 100
	for range sources {
		pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pc.Close() })
		msg, err := stun.Build(stun.TransactionID, stun.BindingRequest, stun.NewUsername(username),
			stun.NewShortTermIntegrity(password), stun.Fingerprint)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pc.WriteTo(msg.Raw, target); err != nil {
			t.Fatal(err)
		}
	}

	s.mu.Lock()
	c := s.conns[connID]
	s.mu.Unlock()
	c.mu.Lock()
	pc := c.pc
	c.mu.Unlock()
	remote := func() int {
		n := 0
		for _, st := range pc.GetStats() {
			if cs, ok := st.(webrtc.ICECandidateStats); ok && cs.Type == webrtc.StatsTypeRemoteCandidate {
				n++
			}
		}
		return n
	}
	bound := remoteCandidates(s.lim)
	// The requests reached ICE: it holds most of what the bound lets it, the rest spent on asks about
	// the offer's own candidates.
	eventually(t, 5*time.Second, "the binding requests reach ICE", func() bool { return remote() > bound/2 })
	time.Sleep(300 * time.Millisecond)
	if n := remote(); n > bound {
		t.Fatalf("binding requests from %d addresses left %d remote candidates, past the bound of %d", sources, n, bound)
	}
}
