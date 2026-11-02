// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/voice/voicetest"
	"github.com/Alexnex31/Norite/backend/voiceproto"
)

const contractID = "https://norite.example/contracts/voice-datachannel.schema.json"

var contract = sync.OnceValues(func() (map[string]*jsonschema.Schema, error) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "voice-datachannel.schema.json"))
	if err != nil {
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(contractID, doc); err != nil {
		return nil, err
	}
	out := map[string]*jsonschema.Schema{}
	for _, def := range []string{"ClientMessage", "ServerMessage"} {
		if out[def], err = c.Compile(contractID + "#/$defs/" + def); err != nil {
			return nil, err
		}
	}
	return out, nil
})

// checkContract holds every message a client received to the server's half of the contract, and every
// message it sent to the client's half.
func checkContract(t *testing.T, c *voicetest.Client) {
	t.Helper()
	schemas, err := contract()
	if err != nil {
		t.Fatal(err)
	}
	for def, msgs := range map[string][][]byte{"ServerMessage": c.Messages(), "ClientMessage": c.Sent()} {
		for _, m := range msgs {
			inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(m))
			if err != nil {
				t.Errorf("not JSON: %s", m)
				continue
			}
			if err := schemas[def].Validate(inst); err != nil {
				t.Errorf("a message the contract does not describe as a %s:\n%s\n%v", def, m, err)
			}
		}
	}
}

func waitOp(t *testing.T, c *voicetest.Client, op string, match func([]byte) bool) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, err := c.WaitFor(ctx, func(got string, raw []byte) bool {
		return got == op && (match == nil || match(raw))
	})
	if err != nil {
		t.Fatalf("waiting for %s: %v", op, err)
	}
	return m
}

// prime has each sender send a few packets, which is what makes its track known: Pion reports a track on
// its first packet, so a sender that has sent nothing fills no slot.
func prime(t *testing.T, senders ...joined) {
	t.Helper()
	sends := map[*voicetest.Client][][]byte{}
	for _, j := range senders {
		sends[j.client] = voicetest.TonePackets()[:3]
	}
	send(t, sends, voicetest.SendOptions{})
}

func slotsWith(n int) func([]byte) bool {
	return func(raw []byte) bool {
		var m voiceproto.Slots
		return voiceproto.Decode(raw, &m) == nil && len(m.Map) == n
	}
}

func TestTheSlotMapNamesWhoFillsEachSlot(t *testing.T) {
	s := newSFU(t, nil)
	room := ids.MustNext()
	a := join(t, s, room, ids.MustNext(), 2)
	b := join(t, s, room, ids.MustNext(), 1)
	c := join(t, s, room, ids.MustNext(), 1)
	prime(t, b, c)

	var ready voiceproto.Ready
	if err := voiceproto.Decode(waitOp(t, a.client, voiceproto.OpReady, nil), &ready); err != nil {
		t.Fatal(err)
	}
	midOf := map[int]string{}
	for _, si := range ready.Slots {
		midOf[si.Slot] = si.Mid
	}
	if len(midOf) != 2 {
		t.Fatalf("ready names %d slots, want 2", len(midOf))
	}

	var m voiceproto.Slots
	if err := voiceproto.Decode(waitOp(t, a.client, voiceproto.OpSlots, slotsWith(2)), &m); err != nil {
		t.Fatal(err)
	}
	tone := voicetest.TonePackets()[:10]
	send(t, map[*voicetest.Client][][]byte{b.client: tone, c.client: tone}, voicetest.SendOptions{})

	users := map[string]bool{}
	for _, e := range m.Map {
		got := a.client.Received(midOf[e.Slot])
		if len(got) == 0 {
			t.Fatalf("slot %d received nothing", e.Slot)
		}
		for _, p := range got {
			if p.CSRC[0] != e.Source {
				t.Fatalf("slot %d: the map names source %d, its packets %d", e.Slot, e.Source, p.CSRC[0])
			}
		}
		want := map[string]joined{b.user.String(): b, c.user.String(): c}[e.User]
		if want.client == nil || want.conn.String() != e.Conn {
			t.Fatalf("slot %d names user %s on connection %s", e.Slot, e.User, e.Conn)
		}
		users[e.User] = true
	}
	if len(users) != 2 {
		t.Fatalf("the map names %v", users)
	}
}

func TestAHelloFromAnotherMinorIsRefusedWithItsReason(t *testing.T) {
	s := newSFUWith(t, func(o *Options) { o.Version = "0.2.0" })
	j := joinWith(t, s, ids.MustNext(), ids.MustNext(), voicetest.Options{Slots: 1, Version: "0.1.0"})
	var r voiceproto.Refused
	if err := voiceproto.Decode(waitOp(t, j.client, voiceproto.OpRefused, nil), &r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Reason, "upgrade the client") {
		t.Fatalf("the reason does not say what to do: %q", r.Reason)
	}
	eventually(t, 5*time.Second, "the refused connection is closed", func() bool { return !s.hasConn(j.conn) })
}

func TestAMatchingHelloIsAnsweredWithReady(t *testing.T) {
	s := newSFUWith(t, func(o *Options) { o.Version = "0.2.3" })
	j := joinWith(t, s, ids.MustNext(), ids.MustNext(), voicetest.Options{Slots: 1, Version: "0.2.0"})
	var r voiceproto.Ready
	if err := voiceproto.Decode(waitOp(t, j.client, voiceproto.OpReady, nil), &r); err != nil {
		t.Fatal(err)
	}
	if r.Version != "0.2.3" || len(r.Slots) != 1 || r.Slots[0].Mid != j.client.SlotMids()[0] {
		t.Fatalf("ready: %+v", r)
	}
}

func TestAProtocolViolationIsRefusedAndClosed(t *testing.T) {
	cases := map[string]struct {
		hello  bool
		msg    string
		reason string
	}{
		"subscribe before hello": {false, `{"op":"subscribe","pin":[],"drop":[]}`, "first message must be hello"},
		"a malformed hello":      {false, `{"op":"hello","version":"dev","extra":1}`, "hello is malformed"},
		"not JSON":               {true, `nope`, "name its op"},
		"an unknown op":          {true, `{"op":"speaking","users":[]}`, "not a message the client sends"},
		"an unknown field":       {true, `{"op":"subscribe","pin":[],"drop":[],"mute":true}`, "subscribe is malformed"},
		"an id that is not one":  {true, `{"op":"subscribe","pin":["x"],"drop":[]}`, "not one"},
		"a negative cap":         {true, `{"op":"subscribe","pin":[],"drop":[],"cap":-1}`, "not negative"},
		"too long a list": {true, `{"op":"subscribe","pin":[` +
			strings.TrimSuffix(strings.Repeat(`"1",`, voiceproto.MaxSubscribeList+1), ",") + `],"drop":[]}`, "more than"},
	}
	s := newSFU(t, nil)
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			j := joinWith(t, s, ids.MustNext(), ids.MustNext(), voicetest.Options{Slots: 1, NoHello: !tc.hello})
			if tc.hello {
				waitOp(t, j.client, voiceproto.OpReady, nil)
			}
			// Sent raw and kept out of the contract check: these are what the contract excludes.
			if err := j.client.SendRawUnchecked([]byte(tc.msg)); err != nil {
				t.Fatal(err)
			}
			var r voiceproto.Refused
			if err := voiceproto.Decode(waitOp(t, j.client, voiceproto.OpRefused, nil), &r); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(r.Reason, tc.reason) {
				t.Fatalf("refused for another reason: %q", r.Reason)
			}
			eventually(t, 5*time.Second, "the connection is closed", func() bool { return !s.hasConn(j.conn) })
		})
	}
}

func TestASecondDataChannelIsClosed(t *testing.T) {
	s := newSFU(t, func(l *Limits) { l.ExtraChannels = 2 })
	j := join(t, s, ids.MustNext(), ids.MustNext(), 1)
	waitOp(t, j.client, voiceproto.OpReady, nil)

	for i := range 2 {
		dc, err := j.client.OpenChannel("extra")
		if err != nil {
			t.Fatal(err)
		}
		closed := make(chan struct{})
		dc.OnClose(func() { close(closed) })
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatalf("extra channel %d was left open", i)
		}
		if !s.hasConn(j.conn) {
			t.Fatalf("the connection was closed after %d extra channels, within the bound", i+1)
		}
	}
	if _, err := j.client.OpenChannel("one-too-many"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the connection is closed past the bound", func() bool { return !s.hasConn(j.conn) })
}

// slotHolder is who fills a one-slot receiver's slot after a burst from every sender, read off the
// packets' CSRCs against the newest slot map.
func slotHolder(t *testing.T, recv joined, senders ...joined) string {
	t.Helper()
	m, _ := recv.client.LatestSlots()
	before := len(recv.client.ReceivedAll())
	sends := map[*voicetest.Client][][]byte{}
	for _, j := range senders {
		sends[j.client] = voicetest.TonePackets()[:10]
	}
	send(t, sends, voicetest.SendOptions{})
	got := recv.client.ReceivedAll()[before:]
	if len(m.Map) == 0 {
		if len(got) != 0 {
			t.Fatalf("an empty slot map, and %d packets arrived", len(got))
		}
		return ""
	}
	for _, p := range got {
		if p.CSRC[0] != m.Map[0].Source {
			t.Fatalf("the map names source %d, a packet %d", m.Map[0].Source, p.CSRC[0])
		}
	}
	if len(got) == 0 {
		t.Fatalf("the slot is filled (%+v) and nothing arrived; %d packets before", m.Map, before)
	}
	return m.Map[0].User
}

func TestASubscriptionPinsDropsAndCaps(t *testing.T) {
	s := newSFU(t, nil)
	room := ids.MustNext()
	recv := join(t, s, room, ids.MustNext(), 1)
	b := join(t, s, room, ids.MustNext(), 1)
	c := join(t, s, room, ids.MustNext(), 1)
	prime(t, b, c)
	waitOp(t, recv.client, voiceproto.OpSlots, slotsWith(1))
	first := slotHolder(t, recv, b, c)
	other := map[string]string{b.user.String(): c.user.String(), c.user.String(): b.user.String()}[first]

	// subscribe sends a subscription and waits until the newest slot map is what it should produce. A
	// subscription that leaves the map as it was sends none, so this cannot wait for a new one; the
	// moment after is for the subscription to be applied either way.
	subscribe := func(pin, drop []string, slotCap *int, want func([]byte) bool) {
		t.Helper()
		if err := recv.client.Subscribe(pin, drop, slotCap); err != nil {
			t.Fatal(err)
		}
		eventually(t, 5*time.Second, "the slot map follows the subscription", func() bool {
			m, ok := recv.client.LatestSlots()
			if !ok {
				return false
			}
			raw, err := voiceproto.Encode(m)
			return err == nil && want(raw)
		})
		time.Sleep(200 * time.Millisecond)
	}
	names := func(user string) func([]byte) bool {
		return func(raw []byte) bool {
			var m voiceproto.Slots
			return voiceproto.Decode(raw, &m) == nil && len(m.Map) == 1 && m.Map[0].User == user
		}
	}

	subscribe([]string{other}, nil, nil, names(other))
	if got := slotHolder(t, recv, b, c); got != other {
		t.Fatalf("pinned %s, the slot carries %s", other, got)
	}
	subscribe(nil, []string{first}, nil, names(other))
	if got := slotHolder(t, recv, b, c); got != other {
		t.Fatalf("dropped %s, the slot carries %s", first, got)
	}
	subscribe([]string{first}, []string{first}, nil, names(other))
	if got := slotHolder(t, recv, b, c); got != other {
		t.Fatal("a user pinned and dropped was served")
	}
	none := 0
	subscribe(nil, nil, &none, slotsWith(0))
	if got := slotHolder(t, recv, b, c); got != "" {
		t.Fatalf("a cap of none, and the slot carries %s", got)
	}
	subscribe(nil, nil, nil, slotsWith(1))
}

func TestAPinDoesNotOverrideTheServersExclusion(t *testing.T) {
	s := newSFU(t, nil)
	room := ids.MustNext()
	recv := join(t, s, room, ids.MustNext(), 1)
	excluded := join(t, s, room, ids.MustNext(), 1)
	prime(t, excluded)
	waitOp(t, recv.client, voiceproto.OpSlots, slotsWith(1))
	if err := s.SetExclusion(context.Background(), room, recv.user, []snowflake.ID{excluded.user}); err != nil {
		t.Fatal(err)
	}
	if err := recv.client.Subscribe([]string{excluded.user.String()}, nil, nil); err != nil {
		t.Fatal(err)
	}
	waitOp(t, recv.client, voiceproto.OpSlots, slotsWith(0))
	time.Sleep(200 * time.Millisecond)
	if got := slotHolder(t, recv, excluded); got != "" {
		t.Fatalf("a pin brought back an excluded user: %s", got)
	}
}
