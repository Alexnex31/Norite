// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/voice/voicetest"
	"github.com/Alexnex31/Norite/backend/voiceproto"
)

var ids = func() *snowflake.Generator {
	g, err := snowflake.NewGenerator(1)
	if err != nil {
		panic(err)
	}
	return g
}()

// newSFU starts an SFU on a loopback socket, with limits adjusted by tune.
func newSFU(t *testing.T, tune func(*Limits)) *SFU {
	t.Helper()
	return newSFUWith(t, func(o *Options) {
		if tune != nil {
			tune(&o.Limits)
		}
	})
}

// newSFUWith starts an SFU on a loopback socket, with its options adjusted by tune.
func newSFUWith(t *testing.T, tune func(*Options)) *SFU {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Conn: pc, Loopback: true, Version: "dev", Limits: DefaultLimits(), Logger: zerolog.Nop()}
	tune(&opts)
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newClient(t *testing.T, slots int) *voicetest.Client {
	t.Helper()
	return newClientWith(t, voicetest.Options{Slots: slots})
}

// newClientWith builds a client whose every data-channel message, both ways, is held to the contract when
// the test ends.
func newClientWith(t *testing.T, opts voicetest.Options) *voicetest.Client {
	t.Helper()
	opts.Loopback = true
	c, err := voicetest.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	t.Cleanup(func() { checkContract(t, c) })
	return c
}

func offerOf(t *testing.T, c *voicetest.Client) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	offer, err := c.Offer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return offer
}

// joined is one client admitted and connected.
type joined struct {
	client *voicetest.Client
	user   snowflake.ID
	conn   snowflake.ID
}

// join provisions a new client for user in room and waits for it to connect.
func join(t *testing.T, s *SFU, room, user snowflake.ID, slots int) joined {
	t.Helper()
	return joinWith(t, s, room, user, voicetest.Options{Slots: slots})
}

func joinWith(t *testing.T, s *SFU, room, user snowflake.ID, opts voicetest.Options) joined {
	t.Helper()
	c := newClientWith(t, opts)
	conn := ids.MustNext()
	answer, err := s.Provision(context.Background(), room, user, conn, ids.MustNext(), offerOf(t, c))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := c.Accept(answer); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.WaitConnected(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.ChannelOpen():
	case <-ctx.Done():
		t.Fatal("the data channel did not open")
	}
	// A client takes part once its hello is answered, as a real one does: a packet sent before then is
	// one the room has not counted yet, and is not forwarded.
	if !opts.NoHello {
		if _, err := c.WaitFor(ctx, func(op string, _ []byte) bool {
			return op == voiceproto.OpReady || op == voiceproto.OpRefused
		}); err != nil {
			t.Fatalf("no answer to the hello: %v", err)
		}
	}
	return joined{client: c, user: user, conn: conn}
}

func (s *SFU) hasConn(id snowflake.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.conns[id]
	return ok
}

func (s *SFU) roomCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rooms)
}

// eventually polls cond until it holds or the time is up.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %s: %s", within, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTwoClientsAreAdmittedAndConnect(t *testing.T) {
	s := newSFU(t, nil)
	room := ids.MustNext()
	a := join(t, s, room, ids.MustNext(), 2)
	b := join(t, s, room, ids.MustNext(), 2)
	if !s.hasConn(a.conn) || !s.hasConn(b.conn) {
		t.Fatal("a connected client is not held by the SFU")
	}
}

func TestAPeerNobodyProvisionedIsRefused(t *testing.T) {
	s := newSFU(t, nil)
	room := ids.MustNext()

	// The answer to A's offer, applied by C: C's ICE credentials are not the ones the SFU was told to
	// expect, so its connectivity checks are refused and it never connects.
	a := newClient(t, 1)
	connA := ids.MustNext()
	answer, err := s.Provision(context.Background(), room, ids.MustNext(), connA, ids.MustNext(), offerOf(t, a))
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, 1)
	offerOf(t, c)
	if err := c.Accept(answer); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.WaitConnected(ctx); err == nil {
		t.Fatal("a peer nobody provisioned connected")
	}
	s.mu.Lock()
	connected := s.conns[connA].connected
	s.mu.Unlock()
	if connected {
		t.Fatal("the provisioned connection was taken by another peer")
	}
}

func TestAPeerPresentingAnotherFingerprintIsRefused(t *testing.T) {
	s := newSFU(t, nil)
	c := newClient(t, 1)
	offer := offerOf(t, c)

	// Another client's fingerprint, in this client's offer. ICE succeeds, since the credentials are this
	// client's; DTLS must not, since the certificate is not the one the offer names.
	other := offerOf(t, newClient(t, 1))
	fp, otherFP := fingerprintLine(offer), fingerprintLine(other)
	if fp == "" || otherFP == "" || fp == otherFP {
		t.Fatalf("fingerprints: %q and %q", fp, otherFP)
	}
	forged := strings.ReplaceAll(offer, fp, otherFP)

	conn := ids.MustNext()
	answer, err := s.Provision(context.Background(), ids.MustNext(), ids.MustNext(), conn, ids.MustNext(), forged)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Accept(answer); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := c.WaitConnected(ctx); err == nil {
		t.Fatal("a peer whose certificate is not the offer's connected")
	}
	eventually(t, 10*time.Second, "the SFU drops the connection", func() bool { return !s.hasConn(conn) })
}

func fingerprintLine(sdp string) string {
	for _, line := range strings.Split(sdp, "\r\n") {
		if strings.HasPrefix(line, "a=fingerprint:") {
			return line
		}
	}
	return ""
}

func TestAProvisionNobodyConnectsToIsDropped(t *testing.T) {
	s := newSFU(t, func(l *Limits) { l.ProvisionTimeout = 300 * time.Millisecond })
	conn := ids.MustNext()
	if _, err := s.Provision(context.Background(), ids.MustNext(), ids.MustNext(), conn, ids.MustNext(),
		offerOf(t, newClient(t, 1))); err != nil {
		t.Fatal(err)
	}
	if !s.hasConn(conn) {
		t.Fatal("a provision is not held while it waits")
	}
	eventually(t, 3*time.Second, "an unconnected provision is dropped", func() bool {
		return !s.hasConn(conn) && s.roomCount() == 0
	})
}

func TestAProvisionOlderThanTheTimeoutIsRefused(t *testing.T) {
	s := newSFU(t, nil)
	issued := ids.MustNext()
	s.now = func() time.Time { return time.Now().Add(DefaultLimits().ProvisionTimeout) }
	_, err := s.Provision(context.Background(), ids.MustNext(), ids.MustNext(), ids.MustNext(), issued,
		offerOf(t, newClient(t, 1)))
	if !errors.Is(err, ErrStale) {
		t.Fatalf("got %v, want ErrStale", err)
	}
}

func TestAProvisionIssuedBeforeAnEvictionIsRefused(t *testing.T) {
	s := newSFU(t, nil)
	room, user := ids.MustNext(), ids.MustNext()

	// The kick and the join were resolved concurrently, and the kick arrived first.
	before := ids.MustNext()
	cut := ids.MustNext()
	if err := s.Evict(context.Background(), room, user, cut); err != nil {
		t.Fatal(err)
	}
	_, err := s.Provision(context.Background(), room, user, ids.MustNext(), before, offerOf(t, newClient(t, 1)))
	if !errors.Is(err, ErrEvicted) {
		t.Fatalf("a provision issued before the cutoff: got %v, want ErrEvicted", err)
	}
	// The cutoff is the user's in that room, nobody else's, and nothing after it.
	if _, err := s.Provision(context.Background(), room, ids.MustNext(), ids.MustNext(), before,
		offerOf(t, newClient(t, 1))); err != nil {
		t.Fatalf("another user: %v", err)
	}
	if _, err := s.Provision(context.Background(), room, user, ids.MustNext(), ids.MustNext(),
		offerOf(t, newClient(t, 1))); err != nil {
		t.Fatalf("a provision issued after the cutoff: %v", err)
	}
}

func TestACutoffIsForgottenOnlyOnceItCanRefuseNothing(t *testing.T) {
	s := newSFU(t, nil)
	room, user := ids.MustNext(), ids.MustNext()
	before := ids.MustNext()
	timeout := DefaultLimits().ProvisionTimeout

	// The eviction lands at the very millisecond the refused provision was issued, the case where the
	// cutoff's lapse and the provision's staleness meet exactly.
	at := before.Time()
	s.now = func() time.Time { return at }
	if err := s.Evict(context.Background(), room, user, before); err != nil {
		t.Fatal(err)
	}
	for _, after := range []time.Duration{0, timeout - time.Millisecond, timeout, timeout + time.Second} {
		s.now = func() time.Time { return at.Add(after) }
		_, err := s.Provision(context.Background(), room, user, ids.MustNext(), before, offerOf(t, newClient(t, 1)))
		if !errors.Is(err, ErrEvicted) && !errors.Is(err, ErrStale) {
			t.Fatalf("%s after the eviction: got %v, want a refusal", after, err)
		}
	}
}

func TestEvictionClosesTheUsersConnectionsUpToTheCutoff(t *testing.T) {
	s := newSFU(t, nil)
	room, user := ids.MustNext(), ids.MustNext()
	a := join(t, s, room, user, 1)
	other := join(t, s, room, ids.MustNext(), 1)

	if err := s.Evict(context.Background(), room, user, ids.MustNext()); err != nil {
		t.Fatal(err)
	}
	if s.hasConn(a.conn) {
		t.Fatal("an evicted connection is still held")
	}
	if !s.hasConn(other.conn) {
		t.Fatal("eviction closed another user's connection")
	}
	select {
	case <-a.client.Failed():
	case <-time.After(10 * time.Second):
		t.Fatal("the evicted client was not disconnected")
	}
}

func TestEvictionSparesAConnectionIssuedAfterTheCutoff(t *testing.T) {
	s := newSFU(t, nil)
	room, user := ids.MustNext(), ids.MustNext()
	cut := ids.MustNext()
	late := join(t, s, room, user, 1)
	if err := s.Evict(context.Background(), room, user, cut); err != nil {
		t.Fatal(err)
	}
	if !s.hasConn(late.conn) {
		t.Fatal("an eviction closed a connection issued after its cutoff")
	}
}

func TestASecondConnectionForOneUserIsItsOwn(t *testing.T) {
	s := newSFU(t, nil)
	room, user := ids.MustNext(), ids.MustNext()
	first := join(t, s, room, user, 1)
	second := join(t, s, room, user, 1)
	if err := s.EvictConnection(context.Background(), second.conn); err != nil {
		t.Fatal(err)
	}
	if s.hasConn(second.conn) || !s.hasConn(first.conn) {
		t.Fatal("EvictConnection did not close exactly its own connection")
	}
	if err := s.EvictConnection(context.Background(), second.conn); err != nil {
		t.Fatalf("evicting a connection already gone: %v", err)
	}
}

func TestTheLastLeaveFreesTheRoom(t *testing.T) {
	s := newSFU(t, nil)
	room := ids.MustNext()
	a := join(t, s, room, ids.MustNext(), 1)
	b := join(t, s, room, ids.MustNext(), 1)
	_ = s.EvictConnection(context.Background(), a.conn)
	if s.roomCount() != 1 {
		t.Fatal("the room was freed with somebody still in it")
	}
	_ = b.client.Close()
	eventually(t, 15*time.Second, "the room is freed", func() bool { return s.roomCount() == 0 })
}

func TestCloseRoomClosesEveryConnection(t *testing.T) {
	s := newSFU(t, nil)
	room := ids.MustNext()
	a := join(t, s, room, ids.MustNext(), 1)
	b := join(t, s, room, ids.MustNext(), 1)
	if err := s.CloseRoom(context.Background(), room, ids.MustNext()); err != nil {
		t.Fatal(err)
	}
	if s.hasConn(a.conn) || s.hasConn(b.conn) || s.roomCount() != 0 {
		t.Fatal("CloseRoom left something behind")
	}
}

func TestAdmissionHoldsItsBounds(t *testing.T) {
	provision := func(s *SFU, room, user, conn snowflake.ID) error {
		_, err := s.Provision(context.Background(), room, user, conn, ids.MustNext(), offerOf(t, newClient(t, 1)))
		return err
	}

	t.Run("rooms", func(t *testing.T) {
		s := newSFU(t, func(l *Limits) { l.Rooms = 1 })
		if err := provision(s, ids.MustNext(), ids.MustNext(), ids.MustNext()); err != nil {
			t.Fatal(err)
		}
		if err := provision(s, ids.MustNext(), ids.MustNext(), ids.MustNext()); !errors.Is(err, ErrFull) {
			t.Fatalf("a second room: got %v, want ErrFull", err)
		}
	})
	t.Run("participants", func(t *testing.T) {
		s := newSFU(t, func(l *Limits) { l.ParticipantsPerRoom = 2 })
		room := ids.MustNext()
		for range 2 {
			if err := provision(s, room, ids.MustNext(), ids.MustNext()); err != nil {
				t.Fatal(err)
			}
		}
		if err := provision(s, room, ids.MustNext(), ids.MustNext()); !errors.Is(err, ErrFull) {
			t.Fatalf("a third participant: got %v, want ErrFull", err)
		}
	})
	t.Run("connections per user", func(t *testing.T) {
		s := newSFU(t, func(l *Limits) { l.ConnsPerUser = 1 })
		room, user := ids.MustNext(), ids.MustNext()
		if err := provision(s, room, user, ids.MustNext()); err != nil {
			t.Fatal(err)
		}
		if err := provision(s, room, user, ids.MustNext()); !errors.Is(err, ErrFull) {
			t.Fatalf("a second connection: got %v, want ErrFull", err)
		}
		if s.roomCount() != 1 {
			t.Fatal("a refused provision left a room behind")
		}
	})
	t.Run("a refused provision in a new room leaves no room", func(t *testing.T) {
		// Set after New, which refuses a bound of zero.
		s := newSFU(t, nil)
		s.lim.ParticipantsPerRoom = 0
		if err := provision(s, ids.MustNext(), ids.MustNext(), ids.MustNext()); !errors.Is(err, ErrFull) {
			t.Fatalf("got %v, want ErrFull", err)
		}
		if s.roomCount() != 0 {
			t.Fatal("a refused provision left a room behind")
		}
	})
	t.Run("a connection id twice", func(t *testing.T) {
		s := newSFU(t, nil)
		conn := ids.MustNext()
		if err := provision(s, ids.MustNext(), ids.MustNext(), conn); err != nil {
			t.Fatal(err)
		}
		if err := provision(s, ids.MustNext(), ids.MustNext(), conn); !errors.Is(err, ErrDuplicate) {
			t.Fatalf("got %v, want ErrDuplicate", err)
		}
	})
}

// A channel deleted while a join for it is in flight: the join was authorized first and arrives last, and
// must not reopen the room.
func TestAProvisionIssuedBeforeARoomClosedIsRefused(t *testing.T) {
	s := newSFU(t, nil)
	room := ids.MustNext()
	before := ids.MustNext()
	if err := s.CloseRoom(context.Background(), room, ids.MustNext()); err != nil {
		t.Fatal(err)
	}
	_, err := s.Provision(context.Background(), room, ids.MustNext(), ids.MustNext(), before, offerOf(t, newClient(t, 1)))
	if !errors.Is(err, ErrEvicted) {
		t.Fatalf("got %v, want ErrEvicted", err)
	}
	if s.roomCount() != 0 {
		t.Fatal("a closed room was reopened")
	}
	if _, err := s.Provision(context.Background(), room, ids.MustNext(), ids.MustNext(), ids.MustNext(),
		offerOf(t, newClient(t, 1))); err != nil {
		t.Fatalf("a provision issued after the close: %v", err)
	}
}

// A connection evicted before its own provision arrives is not admitted when it does.
func TestAConnectionEvictedBeforeItsProvisionIsRefused(t *testing.T) {
	s := newSFU(t, nil)
	conn := ids.MustNext()
	if err := s.EvictConnection(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	_, err := s.Provision(context.Background(), ids.MustNext(), ids.MustNext(), conn, ids.MustNext(), offerOf(t, newClient(t, 1)))
	if !errors.Is(err, ErrEvicted) {
		t.Fatalf("got %v, want ErrEvicted", err)
	}
}

// Evicted while Pion is applying the offer: the answer is the eviction, not a bad offer.
func TestAnEvictionDuringNegotiationIsReportedAsOne(t *testing.T) {
	s := newSFU(t, nil)
	conn := ids.MustNext()
	hook := func(where string, _ []byte) {
		if where == "negotiating" {
			_ = s.EvictConnection(context.Background(), conn)
		}
	}
	testPanic.Store(&hook)
	t.Cleanup(func() { testPanic.Store(nil) })
	_, err := s.Provision(context.Background(), ids.MustNext(), ids.MustNext(), conn, ids.MustNext(), offerOf(t, newClient(t, 1)))
	if !errors.Is(err, ErrEvicted) {
		t.Fatalf("got %v, want ErrEvicted", err)
	}
}

// Limits with a field left at zero would start an SFU that admits nobody, and nothing would say why. The
// zero Limits is the defaults; anything else must be whole.
func TestNewRefusesLimitsThatAdmitNobody(t *testing.T) {
	open := func(t *testing.T) net.PacketConn {
		t.Helper()
		pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pc.Close() })
		return pc
	}
	partial := DefaultLimits()
	partial.ProvisionTimeout = 0
	noAudio := DefaultLimits()
	noAudio.Tracks = map[string]TrackBounds{"audio": {Send: 1, Slots: 0}}
	for name, opts := range map[string]Options{
		"one field set":        {Limits: Limits{Rooms: 100}, Version: "dev"},
		"no provision timeout": {Limits: partial, Version: "dev"},
		"no audio slot":        {Limits: noAudio, Version: "dev"},
		"no version":           {Version: ""},
	} {
		t.Run(name, func(t *testing.T) {
			opts.Conn = open(t)
			if s, err := New(opts); err == nil {
				_ = s.Close()
				t.Fatal("New accepted it")
			}
		})
	}
	t.Run("the zero Limits", func(t *testing.T) {
		s, err := New(Options{Conn: open(t), Version: "dev"})
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
	})
}

// Closing a room closes its members without choosing its slots again for each one that goes: one at a
// time, a full room would be 63 selections and thousands of slot maps sent to connections about to close.
func TestCloseRoomChoosesNoSlotsAgain(t *testing.T) {
	s := newSFU(t, nil)
	room := ids.MustNext()
	for range 5 {
		join(t, s, room, ids.MustNext(), 1)
	}
	var selections atomic.Int32
	hook := func(where string, _ []byte) {
		if where == "selection" {
			selections.Add(1)
		}
	}
	testPanic.Store(&hook)
	t.Cleanup(func() { testPanic.Store(nil) })
	if err := s.CloseRoom(context.Background(), room, ids.MustNext()); err != nil {
		t.Fatal(err)
	}
	testPanic.Store(nil)
	// The room's own tick may land once before the room is shut.
	if n := selections.Load(); n > 1 {
		t.Fatalf("closing a room of five chose its slots %d times", n)
	}
}

// A join in flight when the SFU stops is told the SFU stopped, not that it was evicted.
func TestAStopDuringNegotiationIsReportedAsOne(t *testing.T) {
	s := newSFU(t, nil)
	hook := func(where string, _ []byte) {
		if where == "negotiating" {
			_ = s.Close()
		}
	}
	testPanic.Store(&hook)
	t.Cleanup(func() { testPanic.Store(nil) })
	_, err := s.Provision(context.Background(), ids.MustNext(), ids.MustNext(), ids.MustNext(), ids.MustNext(),
		offerOf(t, newClient(t, 1)))
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v, want ErrClosed", err)
	}
}
